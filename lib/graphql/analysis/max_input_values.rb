# frozen_string_literal: true
module GraphQL
  module Analysis
    # Used under the hood to implement input value budgets, see
    # {Schema#max_input_value_depth}, {Schema#max_input_value_count} and
    # {Schema#max_input_value_string_bytes}.
    #
    # This analyzer measures the argument values that will actually be used by
    # the selected operation and rejects the query -- before any resolver runs --
    # when a configured budget is exceeded. Literal values, query variables and
    # default values are all measured by the same rules, and argument defaults
    # (including defaults nested inside input objects) are merged in before
    # measuring, so the measured structure matches what the runtime will coerce.
    # Measuring never coerces values, so custom scalar `coerce_input`
    # implementations are not run an extra time for this check.
    #
    # Each argument value is measured as a tree of Ruby values:
    #
    # - **Depth**: a scalar has depth `0`; each `Array` or `Hash` layer adds one
    #   (an empty container has depth `1`). Depth is checked per argument value.
    # - **Element count**: every value in the tree counts as one element,
    #   including arrays and hashes themselves; hash keys count as one element
    #   each. Element counts are added up over all argument values used by the
    #   operation.
    # - **String bytes**: the `bytesize` of every `String` (or `Symbol`) in the
    #   tree, including hash keys. String bytes are added up over all argument
    #   values used by the operation.
    #
    # When the same variable is used at several argument positions, its value is
    # measured once _per position_ and each use counts toward the query's
    # budget, because each position is coerced separately at runtime.
    #
    # Operations in the document that aren't selected for execution don't count
    # toward the budget, and neither do fields skipped by `@skip` or `@include`.
    class MaxInputValues < Analyzer
      def initialize(query)
        super
        schema = query.schema
        @max_depth = schema.max_input_value_depth
        @max_count = schema.max_input_value_count
        @max_string_bytes = schema.max_input_value_string_bytes
        @total_count = 0
        @total_string_bytes = 0
        @count_exceeded = false
        @string_bytes_exceeded = false
        @errors = []
      end

      def analyze?
        !@max_depth.nil? || !@max_count.nil? || !@max_string_bytes.nil?
      end

      def on_enter_field(node, parent, visitor)
        # Skipped fields won't be executed, so their arguments aren't used.
        return if visitor.skipping?
        if (field_defn = visitor.field_definition)
          check_arguments(visitor.query, node, field_defn)
        end
      end

      def on_enter_directive(node, parent, visitor)
        if (directive_defn = visitor.directive_definition)
          check_arguments(visitor.query, node, directive_defn)
        end
      end

      def result
        @errors.empty? ? nil : @errors
      end

      private

      # A sentinel for "this argument position has no value"
      NO_VALUE_GIVEN = Object.new

      # Measure each argument of `owner_defn` that will actually be used:
      # the value written in the query, the variable given for it, or its
      # default value when neither is present.
      def check_arguments(query, owner_node, owner_defn)
        ast_arguments = {}
        owner_node.arguments.each do |arg_node|
          ast_arguments[arg_node.name] = arg_node
        end

        query.types.arguments(owner_defn).each do |arg_defn|
          arg_node = ast_arguments[arg_defn.name]
          value = if arg_node
            v = ruby_value_for(arg_node.value, query)
            next if v.equal?(NO_VALUE_GIVEN)
            v
          elsif arg_defn.default_value?
            arg_defn.default_value
          else
            next
          end

          value = apply_default_values(value, arg_defn.type, query)
          measure_and_check(value, arg_defn, arg_node || owner_node)
        end
      end

      # Convert an AST literal to the plain Ruby value it represents,
      # resolving variables to their provided (or defaulted) values.
      # This never coerces anything, so custom scalars aren't invoked here.
      def ruby_value_for(ast_value, query)
        case ast_value
        when GraphQL::Language::Nodes::VariableIdentifier
          if query.variables.key?(ast_value.name)
            variable_value = query.variables[ast_value.name]
            if variable_value.is_a?(GraphQL::Language::Nodes::AbstractNode)
              # This is the variable's default value from the query string
              ruby_value_for(variable_value, query)
            else
              variable_value
            end
          else
            NO_VALUE_GIVEN
          end
        when GraphQL::Language::Nodes::InputObject
          object_value = {}
          ast_value.arguments.each do |arg|
            v = ruby_value_for(arg.value, query)
            if !v.equal?(NO_VALUE_GIVEN)
              object_value[arg.name] = v
            end
          end
          object_value
        when Array
          ast_value.each_with_object([]) do |v, arr|
            v = ruby_value_for(v, query)
            arr << v if !v.equal?(NO_VALUE_GIVEN)
          end
        when GraphQL::Language::Nodes::Enum
          ast_value.name
        when GraphQL::Language::Nodes::NullValue
          nil
        else
          # Strings, numbers and booleans are already plain Ruby values
          ast_value
        end
      end

      # Merge in default values for input object fields that aren't present
      # in `value`, so that defaults are measured by the same rules as
      # literals and variables.
      def apply_default_values(value, type, query)
        if type.kind.non_null?
          apply_default_values(value, type.of_type, query)
        elsif value.nil?
          nil
        elsif type.kind.list?
          items = value.is_a?(Array) ? value : [value]
          items.map { |v| apply_default_values(v, type.of_type, query) }
        elsif type.kind.input_object? && value.is_a?(Hash)
          merged_value = nil
          query.types.arguments(type).each do |input_field_defn|
            # Variable values and literals use GraphQL-style (String) keys,
            # while Ruby default hashes may use keyword (Symbol) keys.
            key = if value.key?(input_field_defn.name)
              input_field_defn.name
            elsif value.key?(input_field_defn.keyword)
              input_field_defn.keyword
            end

            if key
              inner_value = apply_default_values(value[key], input_field_defn.type, query)
              if !inner_value.equal?(value[key])
                merged_value ||= value.dup
                merged_value[key] = inner_value
              end
            elsif input_field_defn.default_value?
              merged_value ||= value.dup
              merged_value[input_field_defn.name] = apply_default_values(input_field_defn.default_value, input_field_defn.type, query)
            end
          end
          merged_value || value
        else
          value
        end
      end

      # @return [Array(Integer, Integer, Integer)] depth, element count and string bytes of `value`
      def measure_value(value)
        case value
        when Hash
          depth = 1
          count = 1
          string_bytes = 0
          value.each do |k, v|
            _key_depth, key_count, key_bytes = measure_value(k)
            value_depth, value_count, value_bytes = measure_value(v)
            depth = value_depth + 1 if value_depth + 1 > depth
            count += key_count + value_count
            string_bytes += key_bytes + value_bytes
          end
          [depth, count, string_bytes]
        when Array
          depth = 1
          count = 1
          string_bytes = 0
          value.each do |v|
            value_depth, value_count, value_bytes = measure_value(v)
            depth = value_depth + 1 if value_depth + 1 > depth
            count += value_count
            string_bytes += value_bytes
          end
          [depth, count, string_bytes]
        when String
          [0, 1, value.bytesize]
        when Symbol
          [0, 1, value.to_s.bytesize]
        else
          [0, 1, 0]
        end
      end

      def measure_and_check(value, arg_defn, ast_node)
        depth, count, string_bytes = measure_value(value)

        if @max_depth && depth > @max_depth
          @errors << GraphQL::AnalysisError.new(
            "Argument `#{arg_defn.path}` has input depth of #{depth}, which exceeds max input value depth of #{@max_depth}",
            ast_node: ast_node
          )
        end

        if @max_count
          @total_count += count
          if @total_count > @max_count && !@count_exceeded
            @count_exceeded = true
            @errors << GraphQL::AnalysisError.new(
              "Argument `#{arg_defn.path}` brings the query to #{@total_count} input value elements, which exceeds max input value count of #{@max_count}",
              ast_node: ast_node
            )
          end
        end

        if @max_string_bytes
          @total_string_bytes += string_bytes
          if @total_string_bytes > @max_string_bytes && !@string_bytes_exceeded
            @string_bytes_exceeded = true
            @errors << GraphQL::AnalysisError.new(
              "Argument `#{arg_defn.path}` brings the query to #{@total_string_bytes} input value string bytes, which exceeds max input value string bytes of #{@max_string_bytes}",
              ast_node: ast_node
            )
          end
        end
      end
    end
  end
end
