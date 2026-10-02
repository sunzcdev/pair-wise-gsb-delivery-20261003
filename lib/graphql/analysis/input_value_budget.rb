# frozen_string_literal: true
module GraphQL
  module Analysis
    # Enforce input value budgets on the arguments of the operation being executed.
    #
    # Unlike the query-string token limit, these budgets are measured against the
    # actual values used by field and directive arguments, after variable
    # references and default values have been resolved:
    #
    # - max_input_depth limits how deeply values may nest into lists and input objects.
    # - max_input_elements limits how many scalar values an argument may contain.
    # - max_input_string_bytes limits the total number of bytes in String values.
    #
    # Literal values, variable values, variable default values and argument
    # default values all follow the same counting rules. A variable that is used
    # in more than one argument position is counted once per position, because
    # each position receives its own copy of the value. Only the selected
    # operation is visited, so arguments belonging to unexecuted operations do
    # not consume any budget.
    #
    # Values are counted structurally -- custom scalar `coerce_input` methods
    # are never called -- and the analyzer runs before field resolution begins.
    class InputValueBudget < Analyzer
      # Running totals for one top-level argument position.
      class Totals
        attr_accessor :depth, :elements, :string_bytes

        def initialize
          @depth = 0
          @elements = 0
          @string_bytes = 0
        end
      end

      # A minimal argument stand-in carrying the item type of a list, so the
      # same traversal code keeps working without building a full Argument.
      class ListItemArgument
        attr_reader :type, :owner, :graphql_name

        def initialize(argument_defn, item_type)
          @type = item_type
          @owner = argument_defn.owner
          @graphql_name = argument_defn.graphql_name
        end

        def default_value?
          false
        end
      end

      def initialize(query_or_multiplex)
        super
        @query = subject if subject.is_a?(GraphQL::Query)
        @schema = subject.schema
        @max_input_depth = @query ? @query.max_input_depth : @schema.max_input_depth
        @max_input_elements = @query ? @query.max_input_elements : @schema.max_input_elements
        @max_input_string_bytes = @query ? @query.max_input_string_bytes : @schema.max_input_string_bytes
        @violations = []
      end

      def analyze?
        !(@max_input_depth.nil? && @max_input_elements.nil? && @max_input_string_bytes.nil?)
      end

      def on_enter_field(node, parent, visitor)
        return if visitor.skipping? || visitor.visiting_fragment_definition?
        field_defn = visitor.field_definition
        return if field_defn.nil?
        count_omitted_defaults(node.arguments, field_defn, visitor)
      end

      def on_enter_directive(node, parent, visitor)
        return if visitor.skipping?
        directive_defn = visitor.directive_definition
        return if directive_defn.nil?
        count_omitted_defaults(node.arguments, directive_defn, visitor)
      end

      def on_enter_argument(node, parent, visitor)
        # Only top-level field/directive arguments begin a budget position.
        # Nested input object arguments are handled as part of the value walk.
        return unless parent.is_a?(GraphQL::Language::Nodes::Field) ||
          parent.is_a?(GraphQL::Language::Nodes::Directive)
        return if visitor.skipping? || visitor.visiting_fragment_definition?

        argument_defn = visitor.argument_definition
        return if argument_defn.nil?

        totals = Totals.new
        count_value(node.value, argument_defn, totals, visitor, depth: 1)
        add_violations(node, totals)
      end

      def result
        @violations
      end

      private

      # Arguments absent from the query fall back to their configured default
      # value, which follows the same counting rules as the other value forms.
      def count_omitted_defaults(ast_arguments, argument_owner, visitor)
        provided_names = {}
        ast_arguments.each { |arg| provided_names[arg.name] = true }
        visitor.query.types.arguments(argument_owner).each do |argument_defn|
          next if provided_names[argument_defn.graphql_name]
          next unless argument_defn.default_value?
          totals = Totals.new
          count_value(argument_defn.default_value, argument_defn, totals, visitor, depth: 1)
          add_violations(nil, totals)
        end
      end

      # Dispatch on the value representation, which may be an AST node (query
      # literals and variable default values), a plain-Ruby value (provided
      # variables and argument default values), or a variable identifier.
      #
      # The declared argument type drives traversal: composite-shaped values
      # (hashes/arrays) passed to custom scalars such as JSON are counted as a
      # single scalar element, so literals and variables behave identically.
      def count_value(value, argument_defn, totals, visitor, depth:)
        case value
        when GraphQL::Language::Nodes::VariableIdentifier
          # Every position where a variable is used pays for the whole value,
          # even when the same variable appears in several positions.
          if visitor.query.variables.key?(value.name)
            count_value(visitor.query.variables[value.name], argument_defn, totals, visitor, depth: depth)
          end
        when GraphQL::Language::Nodes::NullValue
          count_scalar(nil, totals)
        when GraphQL::Language::Nodes::Enum
          count_scalar(value.name, totals)
        when GraphQL::Language::Nodes::InputObject
          if input_object_type?(argument_defn)
            count_input_object(value.arguments, argument_defn, totals, visitor, depth)
          else
            count_scalar(ruby_value_from_ast(value), totals)
          end
        when Hash
          if input_object_type?(argument_defn)
            count_input_object(hash_to_pairs(value), argument_defn, totals, visitor, depth)
          else
            count_scalar(value, totals)
          end
        when Array
          if list_type?(argument_defn)
            count_container_depth(depth, totals)
            inner_defn = list_inner_argument(argument_defn)
            value.each do |item|
              count_value(item, inner_defn, totals, visitor, depth: depth + 1)
            end
          else
            count_scalar(value, totals)
          end
        else
          count_scalar(value, totals)
        end
      end

      def input_object_type?(argument_defn)
        argument_defn.type.unwrap.kind.input_object?
      end

      def list_type?(argument_defn)
        type = argument_defn.type
        type = type.of_type if type.non_null?
        type.list?
      end

      # Turn an AST value into a plain-Ruby value for custom scalar counting,
      # without invoking any scalar's `coerce_input`.
      def ruby_value_from_ast(value)
        case value
        when GraphQL::Language::Nodes::InputObject
          value.arguments.each_with_object({}) do |arg, memo|
            memo[arg.name] = ruby_value_from_ast(arg.value)
          end
        when Array
          value.map { |item| ruby_value_from_ast(item) }
        when GraphQL::Language::Nodes::Enum
          value.name
        when GraphQL::Language::Nodes::NullValue
          nil
        else
          value
        end
      end

      def count_input_object(pairs, argument_defn, totals, visitor, depth)
        count_container_depth(depth, totals)
        input_object_type = argument_defn.type.unwrap
        counted_keys = {}
        pairs.each do |pair|
          key = pair.name
          raw_value = pair.value
          inner_defn = visitor.query.types.argument(input_object_type, key)
          if inner_defn
            counted_keys[inner_defn.graphql_name] = true
            count_value(raw_value, inner_defn, totals, visitor, depth: depth + 1)
          else
            count_value(raw_value, argument_defn, totals, visitor, depth: depth + 1)
          end
        end
        # Default values of omitted input object fields use the same rules.
        visitor.query.types.arguments(input_object_type).each do |inner_defn|
          next if counted_keys[inner_defn.graphql_name]
          next unless inner_defn.default_value?
          count_value(inner_defn.default_value, inner_defn, totals, visitor, depth: depth + 1)
        end
      end

      # Minimal duck-typed pair mirroring {Nodes::Argument} for Ruby hash values.
      Pair = Struct.new(:name, :value)

      def hash_to_pairs(hash)
        hash.map { |key, val| Pair.new(key.to_s, val) }
      end

      def count_container_depth(depth, totals)
        totals.depth = depth if depth > totals.depth
      end

      # A scalar (including enums and nulls) counts as one element; String
      # values also contribute their byte length.
      def count_scalar(value, totals)
        totals.elements += 1
        if value.is_a?(String)
          totals.string_bytes += value.bytesize
        end
      end

      # Build an argument definition representing the item type of a list type.
      def list_inner_argument(argument_defn)
        type = argument_defn.type
        type = type.of_type if type.non_null?
        if type.list?
          ListItemArgument.new(argument_defn, type.of_type)
        else
          argument_defn
        end
      end

      def add_violations(node, totals)
        if @max_input_depth && totals.depth > @max_input_depth
          @violations << GraphQL::AnalysisError.new(
            "Input value has depth of #{totals.depth}, which exceeds max input depth of #{@max_input_depth}",
            ast_node: node
          )
        end
        if @max_input_elements && totals.elements > @max_input_elements
          @violations << GraphQL::AnalysisError.new(
            "Input value has #{totals.elements} elements, which exceeds max input elements of #{@max_input_elements}",
            ast_node: node
          )
        end
        if @max_input_string_bytes && totals.string_bytes > @max_input_string_bytes
          @violations << GraphQL::AnalysisError.new(
            "Input value has #{totals.string_bytes} bytes of string data, which exceeds max input string bytes of #{@max_input_string_bytes}",
            ast_node: node
          )
        end
      end
    end
  end
end
