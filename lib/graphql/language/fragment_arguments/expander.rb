# frozen_string_literal: true

module GraphQL
  module Language
    module FragmentArguments
      # Rewrites a document that uses the project-specific fragment arguments
      # extension into an ordinary, executable GraphQL document.
      #
      # @api private
      class Expander
        # Sentinel bound to a nullable parameter which was omitted at a spread
        # site (and has no default value). It models an *absent* value, like
        # an omitted nullable operation variable: a top-level argument
        # resolving to it is dropped rather than passed as explicit `null`.
        ABSENT = :__fragment_arguments_absent__

        # Operation/enclosing-scope variable names that were passed as fragment
        # arguments but did not survive substitution into the fragment body.
        # They are still legitimately "used" from the caller's perspective.
        attr_reader :forwarded_variable_names

        def initialize(schema, context = nil)
          @schema = schema
          @context = context
          @errors = []
          @literal_validator = nil
          @forwarded_variable_names = []
        end

        # Perform the expansion.
        # @param document [GraphQL::Language::Nodes::Document]
        # @return [Array(GraphQL::Language::Nodes::Document, Array<GraphQL::StaticValidation::Error>)]
        #   the (possibly rewritten) document and the collected errors
        def expand(document)
          @fragments = {}
          @parameterized = {}
          @errors = []
          @forwarded_variable_names = []
          @fragment_parameter_indexes = nil
          document.definitions.each do |defn|
            if defn.is_a?(Nodes::FragmentDefinition)
              # Duplicate fragment names are reported by the standard validator.
              @fragments[defn.name] ||= defn
            end
          end

          @fragments.each do |name, frag|
            if !frag.variables.empty?
              @parameterized[name] = frag
              validate_fragment_parameters(frag)
            end
          end

          if !@errors.empty?
            return [document, @errors]
          end

          @expansion_stack = []

          new_definitions = document.definitions.map do |defn|
            case defn
            when Nodes::OperationDefinition
              env = root_environment(defn)
              new_selections = expand_selections(defn.selections, env)
              new_selections.equal?(defn.selections) ? defn : defn.merge(selections: new_selections)
            when Nodes::FragmentDefinition
              if @parameterized.key?(defn.name)
                nil
              else
                env = root_environment(defn)
                new_selections = expand_selections(defn.selections, env)
                new_selections.equal?(defn.selections) ? defn : defn.merge(selections: new_selections)
              end
            else
              defn
            end
          end

          changed = new_definitions.any?(&:nil?)
          new_definitions.compact!
          new_document = if !changed && new_definitions.length == document.definitions.length
            document
          else
            document.merge(definitions: new_definitions)
          end
          [new_document, @errors, @forwarded_variable_names.uniq]
        end

        private

        # An environment represents the lexical scope in which a fragment body
        # is expanded.
        #
        # - `variables` is the set of variable definitions visible in scope:
        #   operation variables at the root, plus every enclosing fragment's
        #   local parameters. It is used to type-check `$outerVariable`
        #   arguments passed to nested spreads.
        # - `bindings` maps an *in-scope local parameter name* to the value
        #   node it should be substituted with. Bindings only cover parameters
        #   declared by parameterized fragments on the expansion stack, so an
        #   operation variable that happens to share a parameter's name is
        #   never captured by it (and vice versa).
        class Environment
          attr_reader :variables, :bindings, :root_variable_names

          def initialize(variables: nil, bindings: nil, parent: nil, inherit_bindings: true)
            @variables = if variables
              parent ? parent.variables.merge(variables) : variables
            elsif parent
              parent.variables.dup
            else
              {}
            end
            @bindings = if bindings
              inherit_bindings && parent ? parent.bindings.merge(bindings) : bindings.dup
            elsif inherit_bindings && parent
              parent.bindings.dup
            else
              {}
            end
            @root_variable_names = parent ? parent.root_variable_names : []
          end

          def bind(name, value_node)
            @bindings[name] = value_node
          end

          def bound?(name)
            @bindings.key?(name)
          end

          def binding_for(name)
            @bindings[name]
          end
        end

        # Root environment for an operation definition: its declared variables
        # are the "operation variables" used when deciding forwarding.
        class RootEnvironment < Environment
          def initialize(operation_variables)
            super(variables: operation_variables, bindings: {}, parent: nil)
            @root_variable_names = operation_variables.keys
          end
        end

        def root_environment(defn)
          vars = {}
          if defn.is_a?(Nodes::OperationDefinition)
            defn.variables.each { |v| vars[v.name] = v }
          end
          # Operation variables are visible for type-checking but are not
          # "bindings": references to them pass through unchanged.
          RootEnvironment.new(vars)
        end

        def operation_variable?(env, name)
          env.root_variable_names.include?(name)
        end

        def validate_fragment_parameters(frag)
          seen = {}
          frag.variables.each do |var_defn|
            if seen.key?(var_defn.name)
              add_error("Fragment parameter `$#{var_defn.name}` can only be declared once", var_defn)
            end
            seen[var_defn.name] = var_defn

            var_type = schema_type(var_defn.type)
            if var_type.nil?
              add_error("Fragment parameter $#{var_defn.name} has an unknown type: #{type_signature(var_defn.type)}", var_defn)
            elsif !var_type.unwrap.kind.input?
              add_error("Fragment parameter $#{var_defn.name} must be an input type, but #{var_type.to_type_signature} is not an input type", var_defn)
            elsif !var_defn.default_value.nil?
              result = literal_validator.validate(var_defn.default_value, var_type)
              if !result.valid?
                problem = result.problems && result.problems.first
                explanation = problem && problem["explanation"]
                explanation ||= "Default value for $#{var_defn.name} doesn't match type #{var_type.to_type_signature}"
                add_error(explanation, var_defn)
              end
            end
          end
        end

        def expand_selections(selections, env)
          new_selections = nil
          selections.each_with_index do |selection, idx|
            new_selection = expand_selection(selection, env)
            if !new_selection.equal?(selection)
              new_selections ||= selections.dup
              new_selections[idx] = new_selection
            end
          end
          new_selections || selections
        end

        def expand_selection(node, env)
          case node
          when Nodes::FragmentSpread
            expand_spread(node, env)
          when Nodes::InlineFragment
            new_directives = substitute_directives(node.directives, env)
            new_selections = expand_selections(node.selections, env)
            if new_directives.equal?(node.directives) && new_selections.equal?(node.selections)
              node
            else
              node.merge(directives: new_directives, selections: new_selections)
            end
          when Nodes::Field
            new_args = substitute_arguments(node.arguments, env)
            new_directives = substitute_directives(node.directives, env)
            new_selections = node.selections.empty? ? node.selections : expand_selections(node.selections, env)
            if new_args.equal?(node.arguments) && new_directives.equal?(node.directives) && new_selections.equal?(node.selections)
              node
            else
              node.merge(arguments: new_args, directives: new_directives, selections: new_selections)
            end
          else
            node
          end
        end

        def substitute_arguments(arguments, env)
          kept = nil
          arguments.each_with_index do |arg, idx|
            new_value = substitute_argument_value(arg.value, env)
            if new_value == ABSENT
              # Top-level argument resolves to an omitted nullable parameter:
              # drop it so it isn't sent as explicit null.
              kept ||= arguments[0...idx]
              next
            end
            resolved_arg = new_value.equal?(arg.value) ? arg : arg.merge(value: new_value)
            if kept
              kept << resolved_arg
            elsif !resolved_arg.equal?(arg)
              kept = arguments[0...idx] + [resolved_arg]
            end
          end
          kept.nil? ? arguments : kept
        end

        def substitute_directives(directives, env)
          new_directives = nil
          directives.each_with_index do |dir, idx|
            new_args = substitute_arguments(dir.arguments, env)
            if !new_args.equal?(dir.arguments)
              new_directives ||= directives.dup
              new_directives[idx] = dir.merge(arguments: new_args)
            end
          end
          new_directives || directives
        end

        def expand_spread(spread, env)
          frag = @fragments[spread.name]
          if frag.nil?
            if !spread.arguments.empty?
              # Halt instead of letting the standard validator encounter an
              # `Argument` whose parent is a fragment spread (which it doesn't
              # know how to resolve).
              add_error("Unknown fragment `#{spread.name}`; a fragment spread with arguments must refer to a declared fragment", spread)
            end
            return spread
          end

          if !@parameterized.key?(spread.name)
            if !spread.arguments.empty?
              add_error("Fragment `#{spread.name}` doesn't declare any arguments, but #{spread.arguments.size} #{spread.arguments.size == 1 ? "was" : "were"} provided", spread)
            end
            return spread
          end

          if @expansion_stack.include?(spread.name)
            cycle = (@expansion_stack + [spread.name]).join(" -> ")
            add_error("Cannot spread fragment `#{spread.name}` within itself via #{cycle}", spread)
            # Drop the recursive spread so expansion can't loop; validation
            # will reject the query because of the error above.
            return Nodes::InlineFragment.new(
              type: frag.type.merge({}),
              directives: spread.directives,
              selections: [],
              filename: spread.filename,
              line: spread.line,
              col: spread.col,
            )
          end

          params = fragment_parameter_index(frag)
          provided = {}
          spread.arguments.each do |arg|
            if provided.key?(arg.name)
              add_error("There can be only one argument named \"#{arg.name}\"", arg)
            else
              provided[arg.name] = arg
            end
          end

          # Unknown parameters
          provided.each do |arg_name, arg|
            if !params.key?(arg_name)
              add_error("Fragment `#{spread.name}` doesn't accept parameter `$#{arg_name}`", arg)
            end
          end

          child_variables = {}
          params.each_value { |v| child_variables[v.name] = v }
          # Variables (definitions) form the lexical chain: the new fragment
          # can see operation variables and all enclosing fragment parameters
          # for type checking. Bindings, however, start fresh: inside this
          # fragment body only its own parameters are substituted, so an
          # enclosing parameter with the same name can't capture a reference
          # here (and vice versa).
          child_env = Environment.new(variables: child_variables, bindings: {}, parent: env, inherit_bindings: false)

          # Build the binding for each declared parameter
          params.each do |param_name, var_defn|
            arg = provided[param_name]
            if arg
              # Argument values are evaluated in the *parent* scope: a
              # `$variable` passed here refers to an operation variable or an
              # enclosing fragment parameter, not to this fragment's locals.
              evaluated_value = substitute_argument_value(arg.value, env)
              evaluated_arg = evaluated_value.equal?(arg.value) ? arg : arg.merge(value: evaluated_value)
              bind_parameter(frag, var_defn, evaluated_arg, env, child_env)
            elsif !var_defn.default_value.nil?
              child_env.bind(param_name, var_defn.default_value)
            elsif var_defn.type.is_a?(Nodes::NonNullType)
              add_error("Missing required parameter `$#{param_name}` in spread of fragment `#{spread.name}`", spread)
            else
              # Nullable parameter omitted without a default: treat the value
              # as absent, like an omitted operation variable.
              child_env.bind(param_name, ABSENT)
            end
          end

          @expansion_stack.push(spread.name)
          begin
            expanded_body = expand_selections(frag.selections, child_env)
          ensure
            @expansion_stack.pop
          end

          # Directives declared on the fragment definition apply to the
          # expansion and may use fragment parameters; directives on the
          # spread apply too, and their arguments are evaluated in the
          # surrounding (parent) scope.
          merged_directives = substitute_directives(frag.directives, child_env) +
            substitute_directives(spread.directives, env)

          Nodes::InlineFragment.new(
            type: frag.type.merge({}),
            directives: merged_directives,
            selections: expanded_body,
            filename: spread.filename,
            line: spread.line,
            col: spread.col,
          )
        end

        def bind_parameter(frag, var_defn, arg, env, child_env)
          param_type = schema_type(var_defn.type)
          value = arg.value

          case value
          when ABSENT
            child_env.bind(var_defn.name, ABSENT)
          when Nodes::VariableIdentifier
            outer_var_defn = env.variables[value.name]
            if outer_var_defn.nil?
              # Leave it untouched; the ordinary "variable is not defined"
              # validator will report it after expansion.
              child_env.bind(var_defn.name, value)
            elsif param_type.nil?
              child_env.bind(var_defn.name, value)
            else
              if operation_variable?(env, value.name)
                @forwarded_variable_names << value.name
              end
              outer_type = schema_type(outer_var_defn.type)
              if outer_type && !variable_type_allowed?(param_type, outer_type, outer_var_defn.default_value)
                add_error(
                  "Type mismatch on fragment parameter $#{var_defn.name}: variable $#{value.name} of type #{outer_type.to_type_signature} is not compatible with parameter type #{param_type.to_type_signature}",
                  arg,
                )
              end
              # Local parameter and operation variable with the same name must
              # not capture each other: rebinding the local name to the outer
              # variable node keeps the reference to the *outer* definition.
              child_env.bind(var_defn.name, value)
            end
          else
            if param_type
              result = literal_validator.validate(value, param_type)
              if !result.valid?
                problem = result.problems && result.problems.first
                explanation = problem && problem["explanation"]
                explanation ||= "Argument `#{arg.name}` on fragment `#{frag.name}` has incompatible value with parameter type #{param_type.to_type_signature}"
                add_error(explanation, arg)
              end
            end
            child_env.bind(var_defn.name, value)
          end
        end

        def fragment_parameter_index(frag)
          @fragment_parameter_indexes ||= {}
          @fragment_parameter_indexes[frag.name] ||= frag.variables.each_with_object({}) { |v, h| h[v.name] = v }
        end

        # Replace local variable references in a value node. Literal nodes are
        # shared (they are never mutated later), but whenever a variable is
        # replaced the surrounding structure is rebuilt so that two spreads of
        # the same fragment never share a substituted result.
        # Like {#substitute_value}, but preserves the {ABSENT} sentinel so a
        # field/directive argument resolving to an omitted nullable parameter
        # can be dropped entirely rather than passed as explicit null.
        def substitute_argument_value(value, env)
          if value.is_a?(Nodes::VariableIdentifier) && env.bound?(value.name)
            env.binding_for(value.name)
          else
            substitute_value(value, env)
          end
        end

        def substitute_value(value, env)
          case value
          when Nodes::VariableIdentifier
            if env.bound?(value.name)
              binding = env.binding_for(value.name)
              binding == ABSENT ? Nodes::NullValue.new(name: "null") : binding
            else
              value
            end
          when Nodes::InputObject
            new_args = nil
            value.arguments.each_with_index do |arg, idx|
              new_value = substitute_value(arg.value, env)
              if !new_value.equal?(arg.value)
                new_args ||= value.arguments.dup
                new_args[idx] = arg.merge(value: new_value)
              end
            end
            new_args ? value.merge(arguments: new_args) : value
          when Nodes::NullValue
            value
          when ::Array
            new_values = nil
            value.each_with_index do |v, idx|
              new_value = substitute_value(v, env)
              if !new_value.equal?(v)
                new_values ||= value.dup
                new_values[idx] = new_value
              end
            end
            new_values || value
          else
            value
          end
        end

        # Implements the "All Variable Usages are Allowed" check from the
        # GraphQL specification, comparing a variable definition's type with
        # the location's (here: fragment parameter's) expected type.
        #
        # A non-null variable may be used where a nullable location is expected
        # (its value can never be null), and a variable with a default value
        # is treated as non-null for this comparison.
        def variable_type_allowed?(location_type, variable_type, variable_default)
          effective_variable_type = if !variable_type.kind.non_null? && !variable_default.nil?
            variable_type.to_non_null_type
          else
            variable_type
          end
          type_allowed_recursive?(location_type, effective_variable_type)
        end

        def type_allowed_recursive?(location_type, variable_type)
          if location_type.kind.non_null?
            return false unless variable_type.kind.non_null?
            return type_allowed_recursive?(location_type.of_type, variable_type.of_type)
          end

          if variable_type.kind.non_null?
            # A non-null variable is fine at a nullable location.
            return type_allowed_recursive?(location_type, variable_type.of_type)
          end

          if location_type.kind.list?
            return false unless variable_type.kind.list?
            return type_allowed_recursive?(location_type.of_type, variable_type.of_type)
          end

          if variable_type.kind.list?
            return false
          end

          variable_type.unwrap == location_type.unwrap
        end

        def literal_validator
          @literal_validator ||= begin
            require "graphql/static_validation/literal_validator"
            if @context
              StaticValidation::LiteralValidator.new(context: @context)
            else
              ctx = StandaloneValidationContext.new(@schema)
              StaticValidation::LiteralValidator.new(context: ctx)
            end
          end
        end

        def type_signature(type_node)
          case type_node
          when Nodes::TypeName then type_node.name
          when Nodes::NonNullType then "#{type_signature(type_node.of_type)}!"
          when Nodes::ListType then "[#{type_signature(type_node.of_type)}]"
          else
            type_node.to_query_string
          end
        end

        def add_error(message, node)
          @errors << GraphQL::StaticValidation::Error.new(message, nodes: [node])
        end

        def schema_type(type_ast)
          if @context
            @schema.type_from_ast(type_ast, context: @context)
          else
            @schema.type_from_ast(type_ast)
          end
        end

        # Minimal stand-in for StaticValidation::ValidationContext when the
        # expander is invoked outside of query validation (standalone tooling).
        class StandaloneValidationContext
          attr_reader :schema, :types

          def initialize(schema)
            @schema = schema
            @types = schema
          end

          def validate_literal(ast_value, type)
            StaticValidation::LiteralValidator.new(context: self).validate(ast_value, type)
          end
        end
      end
    end
  end
end
