# frozen_string_literal: true

require "graphql/language/nodes"
require "graphql/static_validation/literal_validator"

module GraphQL
  module FragmentArguments
    # Raised while transforming a parsed document into the ordinary
    # fragment-free structure the rest of the execution pipeline understands.
    class Error < GraphQL::Error
      attr_reader :ast_node

      # When the error concerns an operation variable forwarded to a fragment,
      # the variable's bare name (without `$`).
      attr_accessor :variable_name

      def initialize(message, ast_node: nil, variable_name: nil)
        @ast_node = ast_node
        @variable_name = variable_name
        super(message)
      end
    end

    # Rewrites a document parsed with fragment parameters into an equivalent
    # document containing only standard GraphQL AST nodes:
    #
    # - every parameterized spread (`...Frag(a: 1)`) is replaced by an inline
    #   fragment whose body is a fully substituted copy of the named fragment;
    # - parameterized fragment definitions are dropped (regular fragments are
    #   left untouched);
    # - operation variables are never renamed, so local names and operation
    #   variable names cannot capture each other.
    #
    # Each spread gets its own deep-copied expansion, so the same fragment
    # spread with two different argument sets can never share a result.
    #
    # This pass also enforces the extension-specific rules: missing required
    # arguments, unknown arguments, value/type incompatibility and recursive
    # parameterized references.
    class Expander
      # @return [Language::Nodes::Document]
      attr_reader :document, :errors

      # @param document [Language::Nodes::Document] a document produced by {FragmentArguments::Parser}
      # @param context [GraphQL::Query::Context, #schema, #types] used for input-type validation
      def initialize(document, context: nil)
        @document = document
        @context = context
        @fragments = {}
        @document.definitions.each do |defn|
          if defn.is_a?(Language::Nodes::FragmentDefinition)
            @fragments[defn.name] = defn
          end
        end
        @literal_validator = @context && GraphQL::StaticValidation::LiteralValidator.new(context: @context)
        @errors = []
      end

      # @return [Language::Nodes::Document] a document using only standard AST nodes
      def transform
        new_definitions = @document.definitions.map do |defn|
          case defn
          when Language::Nodes::OperationDefinition
            expand_in_node(defn)
          when Language::Nodes::FragmentDefinition
            # Parameterized fragments are referenced through inline expansions;
            # plain fragments are kept as-is (but bodies still need nested
            # parameterized spreads expanded).
            if defn.parameterized?
              nil
            else
              expand_in_node(defn)
            end
          else
            defn
          end
        end

        Language::Nodes::Document.new(
          definitions: new_definitions.compact,
          filename: @document.filename,
        )
      end

      # True if the parsed document uses any fragment-arguments syntax.
      def self.parameterized?(document)
        fragments = {}
        document.definitions.each do |defn|
          if defn.is_a?(Language::Nodes::FragmentDefinition)
            fragments[defn.name] = defn
            return true if defn.parameterized?
          end
        end
        document.definitions.any? do |defn|
          case defn
          when Language::Nodes::FragmentDefinition, Language::Nodes::OperationDefinition
            contains_parameterized_spread?(defn.selections, fragments)
          else
            false
          end
        end
      end

      def self.contains_parameterized_spread?(selections, fragments)
        selections.any? do |sel|
          case sel
          when Language::Nodes::FragmentSpread
            sel.with_arguments? || (fragments[sel.name] && fragments[sel.name].parameterized?)
          when Language::Nodes::Field
            sel.selections.any? && contains_parameterized_spread?(sel.selections, fragments)
          when Language::Nodes::InlineFragment
            contains_parameterized_spread?(sel.selections, fragments)
          else
            false
          end
        end
      end

      private

      def expand_in_node(node)
        case node
        when Language::Nodes::OperationDefinition, Language::Nodes::FragmentDefinition
          node.merge(selections: expand_selections(node.selections, []))
        else
          node
        end
      end

      def expand_selections(selections, active_chain)
        selections.map do |sel|
          case sel
          when Language::Nodes::FragmentSpread
            target = @fragments[sel.name]
            if sel.with_arguments? || (target && target.parameterized?)
              expand_spread(sel, active_chain)
            else
              sel
            end
          when Language::Nodes::Field
            sel.selections.any? ? sel.merge(selections: expand_selections(sel.selections, active_chain)) : sel
          when Language::Nodes::InlineFragment
            sel.merge(selections: expand_selections(sel.selections, active_chain))
          else
            sel
          end
        end
      end

      # Build the inline fragment for `spread`, applying argument values.
      # `active_chain` is the stack of parameterized fragment names currently
      # being expanded, used for recursion detection.
      def expand_spread(spread, active_chain)
        fragment = @fragments[spread.name]
        if fragment.nil?
          add_error("No fragment named '#{spread.name}'", spread)
          # Leave the spread as-is so the normal validators can report it.
          return spread
        end

        if active_chain.include?(spread.name)
          add_error(
            "Fragment '#{spread.name}' contains a recursive parameterized fragment reference (#{(active_chain + [spread.name]).join(" -> ")})",
            spread
          )
          return Language::Nodes::InlineFragment.new(
            type: nil,
            directives: spread.directives,
            selections: [],
            filename: spread.filename,
          )
        end

        bindings = resolve_arguments(fragment, spread)

        # Substitute this fragment's local parameters first -- including inside
        # nested spreads' arguments, where a local may be forwarded -- and only
        # then expand nested parameterized spreads. By that point any remaining
        # variable identifier is an operation variable.
        substituted_selections = substitute_in_selections(fragment.selections, bindings)
        expanded_selections = expand_selections(substituted_selections, active_chain + [spread.name])

        Language::Nodes::InlineFragment.new(
          type: fragment.type,
          directives: fragment.directives,
          selections: expanded_selections,
          filename: spread.filename,
        )
      end

      # @return [Hash{String => Language::Nodes::AbstractNode or Ruby value}]
      def resolve_arguments(fragment, spread)
        declared = fragment.fragment_arguments
        declared_by_name = {}
        declared.each do |vdef|
          declared_by_name[vdef.name] = vdef
          if @context && resolve_type(vdef.type).nil?
            add_error("Unknown type '#{ast_type_signature(vdef.type)}' for fragment argument '$#{vdef.name}'", vdef)
          end
        end

        seen = {}
        spread.fragment_arguments.each do |arg|
          if seen.key?(arg.name)
            add_error("Fragment argument '#{arg.name}' is repeated in spread of '#{fragment.name}'", arg)
          end
          seen[arg.name] = arg

          vdef = declared_by_name[arg.name]
          if vdef.nil?
            add_error("Fragment '#{fragment.name}' has no argument named '#{arg.name}'", arg)
            next
          end
          validate_value_type(vdef, arg.value, spread)
        end

        bindings = {}
        declared.each do |vdef|
          if seen.key?(vdef.name)
            bindings[vdef.name] = clone_value(seen[vdef.name].value)
          elsif !vdef.default_value.nil?
            validate_value_type(vdef, vdef.default_value, vdef)
            bindings[vdef.name] = clone_value(vdef.default_value)
          else
            if type_non_null?(vdef.type)
              add_error("Missing required argument '$#{vdef.name}' on spread of '#{fragment.name}'", spread)
            end
            # Bind null so substitution can continue and collect more errors.
            bindings[vdef.name] = Language::Nodes::NullValue.new(name: vdef.name)
          end
        end

        bindings
      end

      def validate_value_type(vdef, ast_value, error_node)
        if ast_value.is_a?(Language::Nodes::VariableIdentifier)
          # A variable passed as a fragment argument only needs structural
          # assignability against the operation variable declaration; the
          # operation variable itself is checked by the standard validators.
          op_type = operation_variable_type(ast_value.name)
          if op_type.nil?
            add_error(
              "Variable '$#{ast_value.name}' is not defined by the operation but is passed as fragment argument '$#{vdef.name}'",
              error_node,
              variable_name: ast_value.name
            )
          elsif !types_compatible?(op_type, vdef.type)
            add_error(
              "Variable '$#{ast_value.name}' of type '#{ast_type_signature(op_type)}' is not compatible with fragment argument '$#{vdef.name}' of type '#{ast_type_signature(vdef.type)}'",
              error_node,
              variable_name: ast_value.name
            )
          end
          return
        end

        type = resolve_type(vdef.type)
        if type.nil?
          add_error("Unknown type '#{ast_type_signature(vdef.type)}' for fragment argument '$#{vdef.name}'", vdef)
          return
        end

        if @literal_validator
          result = @literal_validator.validate(ast_value, type)
          if !result.valid?
            problem = result.problems && result.problems.first
            explanation = problem && problem["explanation"]
            add_error(
              "Value for fragment argument '$#{vdef.name}' is not compatible with type '#{type.to_type_signature}'#{explanation ? " (#{explanation})" : ""}",
              error_node
            )
          end
        end
      end

      # Replace every local `$name` variable identifier with its bound value
      # inside a deep copy of the fragment's tree. Operation-level variables
      # that aren't fragment-local are left untouched -- and a local name that
      # matches an operation variable is replaced by the binding, so the two
      # scopes can never capture one another.
      def substitute_in_selections(selections, bindings)
        selections.map { |sel| substitute_node(sel, bindings) }
      end

      def substitute_node(node, bindings)
        case node
        when Language::Nodes::Field
          new_args = node.arguments.map { |a| substitute_argument(a, bindings) }
          new_sels = node.selections.any? ? substitute_in_selections(node.selections, bindings) : node.selections
          new_dirs = node.directives.map { |d| substitute_directive(d, bindings) }
          node.merge(arguments: new_args, selections: new_sels, directives: new_dirs)
        when Language::Nodes::InlineFragment
          node.merge(
            selections: substitute_in_selections(node.selections, bindings),
            directives: node.directives.map { |d| substitute_directive(d, bindings) }
          )
        when Language::Nodes::FragmentSpread
          # Substitute locals forwarded to nested spreads (both their arguments
          # and directive arguments); a nested parameterized spread is expanded
          # in the next pass (`expand_selections`).
          new_frag_args = node.fragment_arguments.map { |a| substitute_argument(a, bindings) }
          new_dirs = node.directives.map { |d| substitute_directive(d, bindings) }
          node.merge(fragment_arguments: new_frag_args, directives: new_dirs)
        else
          node
        end
      end

      def substitute_argument(argument, bindings)
        argument.merge(value: substitute_value(argument.value, bindings))
      end

      def substitute_directive(directive, bindings)
        directive.merge(arguments: directive.arguments.map { |a| substitute_argument(a, bindings) })
      end

      def substitute_value(value, bindings)
        case value
        when Language::Nodes::VariableIdentifier
          if bindings.key?(value.name)
            bound = bindings[value.name]
            bound.is_a?(Language::Nodes::AbstractNode) ? clone_node(bound) : bound
          else
            # Keep operation variables as-is.
            value
          end
        when Language::Nodes::InputObject
          value.merge(arguments: value.arguments.map { |a| substitute_argument(a, bindings) })
        when Array
          value.map { |v| substitute_value(v, bindings) }
        else
          value
        end
      end

      # Each spread gets its own independent copy of bound literal values, so
      # two spreads of the same fragment can't share mutable AST nodes.
      def clone_value(value)
        value.is_a?(Language::Nodes::AbstractNode) ? clone_node(value) : value
      end

      def clone_node(node)
        case node
        when Language::Nodes::InputObject
          node.merge(arguments: node.arguments.map { |a| a.merge(value: clone_value_node(a.value)) })
        when Array
          node.map { |v| clone_value_node(v) }
        else
          node.dup
        end
      end

      def clone_value_node(value)
        case value
        when Array
          value.map { |v| clone_value_node(v) }
        when Language::Nodes::AbstractNode
          clone_node(value)
        else
          value
        end
      end

      def type_non_null?(ast_type)
        ast_type.is_a?(Language::Nodes::NonNullType)
      end

      def resolve_type(ast_type)
        return nil if @context.nil?
        type = @context.schema.type_from_ast(ast_type, context: @context)
        if type.nil? && ast_type.is_a?(Language::Nodes::TypeName) && (builtin = BUILT_IN_SCALARS[ast_type.name])
          # The built-in scalars are always usable for variable/argument typing
          # even when a schema never otherwise references them.
          type = builtin
        end
        type
      end

      BUILT_IN_SCALARS = {
        "Int" => GraphQL::Types::Int,
        "Float" => GraphQL::Types::Float,
        "String" => GraphQL::Types::String,
        "Boolean" => GraphQL::Types::Boolean,
        "ID" => GraphQL::Types::ID,
      }.freeze

      def ast_type_signature(ast_type)
        case ast_type
        when Language::Nodes::NonNullType
          "#{ast_type_signature(ast_type.of_type)}!"
        when Language::Nodes::ListType
          "[#{ast_type_signature(ast_type.of_type)}]"
        when Language::Nodes::TypeName
          ast_type.name
        else
          String(ast_type)
        end
      end

      # Is a variable usage of `actual_ast` allowed where `expected_ast` is
      # declared? (Structural equivalent of the spec's IsVariableUsageAllowed.)
      def types_compatible?(actual_ast, expected_ast)
        if expected_ast.is_a?(Language::Nodes::NonNullType)
          return false unless actual_ast.is_a?(Language::Nodes::NonNullType)
          return types_compatible?(actual_ast.of_type, expected_ast.of_type)
        end

        if actual_ast.is_a?(Language::Nodes::NonNullType)
          # A non-null variable may flow into a nullable position.
          return types_compatible?(actual_ast.of_type, expected_ast)
        end

        if expected_ast.is_a?(Language::Nodes::ListType)
          return false unless actual_ast.is_a?(Language::Nodes::ListType)
          return types_compatible?(actual_ast.of_type, expected_ast.of_type)
        end

        return false if actual_ast.is_a?(Language::Nodes::ListType)

        actual_ast.is_a?(Language::Nodes::TypeName) && expected_ast.is_a?(Language::Nodes::TypeName) &&
          actual_ast.name == expected_ast.name
      end

      def operation_variable_type(name)
        @operation_variables ||= begin
          vars = {}
          @document.definitions.each do |defn|
            if defn.is_a?(Language::Nodes::OperationDefinition)
              defn.variables.each { |v| vars[v.name] = v.type }
            end
          end
          vars
        end
        @operation_variables[name]
      end

      def add_error(message, ast_node, variable_name: nil)
        @errors << Error.new(message, ast_node: ast_node, variable_name: variable_name)
      end
    end
  end
end
