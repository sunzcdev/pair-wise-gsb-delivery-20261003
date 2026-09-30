# frozen_string_literal: true
module JMESPath
  module Errors
    # Raised by {JMESPath.assert_types!} when an expression provably
    # violates the declared input shape.
    class TypeCheckError < Error; end
  end

  # Compiles a JMESPath expression against a lightweight input shape (see
  # {Types}) and reports obvious function type errors before runtime.
  #
  # The checker only reports what it can *prove*: when a value's shape is
  # unknown (`any`) or only partially overlaps the expected type, the
  # expression passes and the existing runtime checks still apply.
  #
  #   result = JMESPath.check_types('abs(email)', 'email?' => :string)
  #   result.ok?         #=> false
  #   result.errors      #=> [#<JMESPath::TypeChecker::Error ...>]
  #   result.output_type #=> number
  class TypeChecker
    # A single compile-time finding: the offending expression node, what
    # went wrong, and the inference that backs it.
    class Error
      attr_reader :node, :message, :reason

      def initialize(node, message, reason = nil)
        @node = node
        @message = message
        @reason = reason
      end

      def to_s
        lines = ["#{message} (at #{node})"]
        lines << "  reason: #{reason}" if reason
        lines.join("\n")
      end
    end

    # The outcome of {TypeChecker.check}.
    class Result
      attr_reader :expression, :input_type, :output_type, :errors

      def initialize(expression, input_type, output_type, errors)
        @expression = expression
        @input_type = input_type
        @output_type = output_type
        @errors = errors
      end

      def ok?
        @errors.empty?
      end

      # @raise [Errors::TypeCheckError]
      def raise_on_error!
        return self if ok?

        raise Errors::TypeCheckError,
              "type check failed for #{expression.inspect}:\n" +
              @errors.map(&:to_s).join("\n")
      end
    end

    class << self
      # @param [String] expression a JMESPath expression
      # @param [Hash, Types::Type] shape the expected input shape
      # @return [Result]
      def check(expression, shape, options = {})
        ast = Runtime.new(options).parser.parse(expression).optimize
        checker = new
        input = Types.build(shape)
        output = ast.infer_type(checker, input)
        Result.new(expression, input, output, checker.errors)
      end

      # Extracts `[path, mode, value, description]` narrowings from a
      # comparison node. `right` may be a raw value (optimized literal
      # conditions store the value directly).
      def comparison_narrowings(left, right, mode)
        path = left.respond_to?(:type_path) ? left.type_path : nil
        return [] unless path
        return [] if right.is_a?(Nodes::Node) && !right.is_a?(Nodes::Literal)

        value = right.is_a?(Nodes::Literal) ? right.value : right
        description = comparison_description(left, right, mode)
        case mode
        when :eq
          [[path, :eq, value, description]]
        when :neq
          value.nil? ? [[path, :not_null, nil, description]] : []
        when :cmp
          case value
          when Numeric then [[path, :cmp_number, nil, description]]
          when String then [[path, :cmp_string, nil, description]]
          else []
          end
        else
          []
        end
      end

      def comparison_description(left, right, mode)
        op = { eq: '==', neq: '!=', cmp: 'cmp' }[mode]
        rhs = right.is_a?(Nodes::Node) ? right.describe : right.nil? ? 'null' : right.inspect
        "#{left.describe} #{op} #{rhs}"
      end
    end

    attr_reader :errors

    def initialize
      @errors = []
    end

    def error(node, message, reason = nil)
      @errors << Error.new(node, message, reason)
    end

    # --- helpers used by the node `infer_type` implementations ----------

    # Walks `keys` (field names / array indices) down from `type`.
    def access_path(keys, type)
      keys.reduce(type) { |t, key| access_one(t, key) }
    end

    # Applies `[path, mode, value, description]` narrowings (from filter
    # conditions and `&&` left operands) to `type`.
    def narrow(type, narrowings)
      narrowings.reduce(type) do |t, (path, mode, value, description)|
        Types.narrow_path(t, path) do |leaf|
          narrowed =
            case mode
            when :eq
              leaf_type = value.nil? ? Types::NULL : Types.from_value(value)
              Types.intersect(leaf, leaf_type)
            when :not_null
              Types.remove_null(leaf)
            when :cmp_number
              Types.intersect(leaf, Types::NUMBER)
            when :cmp_string
              Types.intersect(leaf, Types::STRING)
            end
          narrowed &&
            narrowed.with_note("narrowed to `#{narrowed}` by filter `#{description}`")
        end
      end
    end

    # Types a `[*]` / `.*` / filter / slice projection over `target_type`.
    # The block types the projection's right-hand side for one element.
    def projection_type(node, target_type, &block)
      case target_type
      when Types::ArrayType
        if node.is_a?(Nodes::ObjectProjection)
          return Types::NULL.with_note('object projection target is an array')
        end

        element = block.call(target_type.element)
        Types.array_of(Types.remove_null(element) || Types::ANY)
      when Types::ObjectType
        if node.is_a?(Nodes::ArrayProjection)
          return Types::NULL.with_note('array projection target is an object')
        end

        values = target_type.fields.values
        element = block.call(values.empty? ? Types::ANY : Types.union(*values))
        Types.array_of(Types.remove_null(element) || Types::ANY)
      when Types::UnionType
        Types.union(*target_type.types.map { |m| projection_type(node, m, &block) })
      when Types::AnyType
        Types.union(Types::NULL, Types.array_of(block.call(Types::ANY)))
      else
        Types::NULL.with_note("projection target is #{target_type}, not an array or object")
      end
    end

    # Element type of an array shape, used to type `&expression` args.
    def element_type(type)
      case type
      when Types::ArrayType
        type.element
      when Types::UnionType
        elements = type.types.select { |t| t.is_a?(Types::ArrayType) }.map(&:element)
        elements.empty? ? Types::ANY : Types.union(*elements)
      else
        Types::ANY
      end
    end

    # Validates a function call node against its spec and infers the
    # return type.
    def check_function(node, name, spec, arg_nodes, current)
      arg_types = arg_nodes.map { |arg| arg.infer_type(self, current) }
      check_arity(node, name, spec, arg_nodes.size)
      arg_types.each_with_index do |actual, index|
        param = spec[:params][index]
        param ||= spec[:params].last if spec[:variadic]
        next unless param

        if Types.intersect(actual, param).nil?
          error(
            node.describe,
            "function #{name}() expects argument #{index + 1} to be #{param}, got #{actual}",
            actual.explanation
          )
        end
      end
      spec[:infer].call(self, node, arg_nodes, arg_types)
    end

    private

    def check_arity(node, name, spec, count)
      min = spec[:min_arity]
      max = spec[:max_arity]
      return if count >= min && count <= max

      expectation =
        if min == max
          "expects #{min} argument(s)"
        else
          "expects between #{min} and #{max} arguments"
        end
      error(node.describe, "function #{name}() #{expectation}, got #{count}")
    end

    def access_one(type, key)
      case type
      when Types::AnyType
        type
      when Types::ScalarType
        Types::NULL.with_note("#{type} values have no fields")
      when Types::ArrayType
        if key.is_a?(Integer)
          type.element
        else
          Types::NULL.with_note("arrays have no field `#{key}`")
        end
      when Types::ObjectType
        if type.fields.key?(key)
          field = type.fields[key]
          if type.optional?(key)
            Types.union(field, Types::NULL)
                   .with_note("field `#{key}` is optional in the input shape")
          else
            field
          end
        else
          type.open? ? Types::ANY : Types::NULL.with_note("field `#{key}` is not declared in the input shape")
        end
      when Types::UnionType
        Types.union(*type.types.map { |member| access_one(member, key) })
      end
    end

    # --- function signatures --------------------------------------------

    # Each spec:
    #   :params    expected shape per argument (nil skips the check)
    #   :variadic  repeat the last param for extra arguments
    #   :min_arity / :max_arity
    #   :infer     lambda (checker, node, arg_nodes, arg_types) -> Type
    FUNCTION_SPECS = {}

    def self.def_function(name, params, returns = nil, min_arity: nil, max_arity: nil, variadic: false, &infer)
      infer ||= proc { |_checker, _node, _arg_nodes, _arg_types| returns }
      FUNCTION_SPECS[name] = {
        params: params,
        variadic: variadic,
        min_arity: min_arity || params.size,
        max_arity: max_arity || params.size,
        infer: infer
      }
    end

    number_array = Types.array_of(Types::NUMBER)
    string_array = Types.array_of(Types::STRING)
    number_or_string_array = Types.union(number_array, string_array)
    sized = Types.union(Types::STRING, Types::ARRAY_ANY, Types::OBJECT_ANY)
    sortable = Types.union(Types::NUMBER, Types::STRING)

    def_function('abs', [Types::NUMBER], Types::NUMBER)
    def_function('ceil', [Types::NUMBER], Types::NUMBER)
    def_function('floor', [Types::NUMBER], Types::NUMBER)
    def_function('sum', [number_array], Types::NUMBER)
    def_function('avg', [number_array], Types.union(Types::NUMBER, Types::NULL))
    def_function('max', [number_or_string_array], Types.union(Types::NUMBER, Types::STRING, Types::NULL))
    def_function('min', [number_or_string_array], Types.union(Types::NUMBER, Types::STRING, Types::NULL))
    def_function('length', [sized], Types::NUMBER)
    def_function('contains', [Types.union(Types::STRING, Types::ARRAY_ANY), Types::ANY], Types::BOOLEAN)
    def_function('starts_with', [Types::STRING, Types::STRING], Types::BOOLEAN)
    def_function('ends_with', [Types::STRING, Types::STRING], Types::BOOLEAN)
    def_function('join', [Types::STRING, string_array], Types::STRING)
    def_function('keys', [Types::OBJECT_ANY], string_array)
    def_function('type', [Types::ANY], Types::STRING)
    def_function('to_string', [Types::ANY], Types::STRING)
    def_function('to_number', [Types::ANY], Types.union(Types::NUMBER, Types::NULL))
    def_function('merge', [Types::OBJECT_ANY], Types::OBJECT_ANY,
                 min_arity: 1, max_arity: Float::INFINITY, variadic: true)

    def_function('sort', [number_or_string_array]) { |_c, _n, _a, arg_types| arg_types[0] }

    def_function('reverse', [Types.union(Types::ARRAY_ANY, Types::STRING)]) do |_c, _n, _a, arg_types|
      parts = []
      parts << Types.intersect(arg_types[0], Types::ARRAY_ANY)
      parts << Types.intersect(arg_types[0], Types::STRING)
      parts.compact!
      parts.empty? ? Types.union(Types::ARRAY_ANY, Types::STRING) : Types.union(*parts)
    end

    def_function('values', [Types.union(Types::OBJECT_ANY, Types::ARRAY_ANY)]) do |_c, _n, _a, arg_types|
      type = arg_types[0]
      if type.is_a?(Types::ObjectType) && !type.fields.empty?
        Types.array_of(Types.union(*type.fields.values))
      else
        Types::ARRAY_ANY
      end
    end

    def_function('to_array', [Types::ANY]) do |_c, _n, _a, arg_types|
      type = arg_types[0] || Types::ANY
      case type
      when Types::ArrayType
        type
      when Types::UnionType
        arrays, others = type.types.partition { |m| m.is_a?(Types::ArrayType) }
        parts = arrays.dup
        parts << Types.array_of(Types.union(*others)) unless others.empty?
        Types.union(*parts)
      when Types::AnyType
        Types::ARRAY_ANY
      else
        Types.array_of(type)
      end
    end

    def_function('not_null', [Types::ANY], min_arity: 1, max_arity: Float::INFINITY, variadic: true) do |_c, _n, _a, arg_types|
      combined = Types.union(*arg_types)
      if arg_types.any? { |t| !Types.maybe_null?(t) }
        Types.remove_null(combined) || combined
      else
        combined
      end
    end

    def_function('map', [Types::EXPRESSION, Types::ARRAY_ANY]) do |checker, _node, arg_nodes, arg_types|
      if arg_nodes.size == 2 && arg_nodes[0].is_a?(Nodes::Expression)
        element = checker.element_type(arg_types[1])
        Types.array_of(arg_nodes[0].expression.infer_type(checker, element))
      else
        Types::ARRAY_ANY
      end
    end

    def_function('sort_by', [Types::ARRAY_ANY, Types::EXPRESSION]) do |checker, node, arg_nodes, arg_types|
      if arg_nodes.size == 2 && arg_nodes[1].is_a?(Nodes::Expression)
        element = checker.element_type(arg_types[0])
        inner = arg_nodes[1].expression.infer_type(checker, element)
        if Types.intersect(inner, sortable).nil?
          checker.error(
            node.describe,
            "function sort_by() expression must evaluate to number or string, got #{inner}",
            inner.note
          )
        end
      end
      Types.intersect(arg_types[0] || Types::ANY, Types::ARRAY_ANY) || Types::ARRAY_ANY
    end

    %w[max_by min_by].each do |name|
      def_function(name, [Types::ARRAY_ANY, Types::EXPRESSION]) do |checker, node, arg_nodes, arg_types|
        element = checker.element_type(arg_types[0] || Types::ANY)
        if arg_nodes.size == 2 && arg_nodes[1].is_a?(Nodes::Expression)
          inner = arg_nodes[1].expression.infer_type(checker, element)
          if Types.intersect(inner, sortable).nil?
            checker.error(
              node.describe,
              "function #{name}() expression must evaluate to number or string, got #{inner}",
              inner.note
            )
          end
        end
        Types.union(element, Types::NULL)
      end
    end
  end

  # Type inference for the (optimized) AST. Each node answers its output
  # shape given the input shape; nodes that cannot prove anything return
  # `Types::ANY`, which never triggers an error.
  module Nodes
    class Node
      def infer_type(_checker, _current)
        Types::ANY
      end

      # Filter-style narrowings this node implies when it evaluates
      # truthy. See {TypeChecker#narrow}.
      def narrowings
        []
      end

      # Field path (list of string keys) this node resolves, or nil.
      def type_path
        nil
      end

      def describe
        self.class.name.split('::').last
      end
    end

    class Current
      def infer_type(_checker, current)
        current
      end

      def describe
        '@'
      end
    end

    class Literal
      def infer_type(_checker, _current)
        Types.from_value(@value)
      end

      def describe
        @value.inspect
      end
    end

    class Expression
      def infer_type(_checker, _current)
        Types::EXPRESSION
      end

      def describe
        "&#{@expression.describe}"
      end
    end

    class Field
      def infer_type(checker, current)
        checker.access_path([@key], current)
      end

      def type_path
        @key.is_a?(String) ? [@key] : nil
      end

      def describe
        @key.is_a?(Integer) ? "[#{@key}]" : @key.to_s
      end
    end

    class ChainedField < Field
      def infer_type(checker, current)
        checker.access_path(@keys, current)
      end

      def type_path
        @keys.all? { |k| k.is_a?(String) } ? @keys : nil
      end

      def describe
        @keys.map { |k| k.is_a?(Integer) ? "[#{k}]" : k.to_s }.join('.')
      end
    end

    class Chain
      def infer_type(checker, current)
        @children.reduce(current) { |type, child| child.infer_type(checker, type) }
      end

      def narrowings
        []
      end

      def type_path
        parts = @children.map(&:type_path)
        parts.any?(&:nil?) ? nil : parts.flatten
      end

      def describe
        @children.map(&:describe).join('.')
      end
    end

    class Subexpression
      def infer_type(checker, current)
        @right.infer_type(checker, @left.infer_type(checker, current))
      end

      def describe
        "#{@left.describe}.#{@right.describe}"
      end
    end

    class And
      def infer_type(checker, current)
        left_type = @left.infer_type(checker, current)
        narrowed = checker.narrow(current, @left.narrowings)
        right_type = @right.infer_type(checker, narrowed)
        Types.union(left_type, right_type)
      end

      def narrowings
        @left.narrowings + @right.narrowings
      end
    end

    class Or
      def infer_type(checker, current)
        Types.union(
          @left.infer_type(checker, current),
          @right.infer_type(checker, current)
        )
      end
    end

    class Not
      def infer_type(checker, current)
        @expression.infer_type(checker, current)
        Types::BOOLEAN
      end
    end

    class Comparator
      def infer_type(checker, current)
        @left.infer_type(checker, current)
        @right.infer_type(checker, current)
        comparison_mode == :cmp ? Types.union(Types::BOOLEAN, Types::NULL) : Types::BOOLEAN
      end

      def narrowings
        JMESPath::TypeChecker.comparison_narrowings(@left, @right, comparison_mode)
      end

      def describe
        op = { eq: '==', neq: '!=', cmp: 'cmp' }[comparison_mode]
        "#{@left.describe} #{op} #{@right.describe}"
      end

      private

      def comparison_mode
        nil
      end
    end

    module Comparators
      class Eq
        private

        def comparison_mode
          :eq
        end
      end

      class Neq
        private

        def comparison_mode
          :neq
        end
      end

      class Gt
        private

        def comparison_mode
          :cmp
        end
      end

      class Gte
        private

        def comparison_mode
          :cmp
        end
      end

      class Lt
        private

        def comparison_mode
          :cmp
        end
      end

      class Lte
        private

        def comparison_mode
          :cmp
        end
      end
    end

    class Condition
      def infer_type(checker, current)
        @test.infer_type(checker, current)
        @child.infer_type(checker, checker.narrow(current, @test.narrowings))
      end

      def describe
        "[?#{@test.describe}]"
      end
    end

    class ComparatorCondition
      def infer_type(checker, current)
        @left.infer_type(checker, current)
        @right.infer_type(checker, current) if @right.is_a?(Node)
        narrowed = checker.narrow(
          current, JMESPath::TypeChecker.comparison_narrowings(@left, @right, comparison_mode)
        )
        @child.infer_type(checker, narrowed)
      end

      private

      def comparison_mode
        nil
      end
    end

    class EqCondition
      private

      def comparison_mode
        :eq
      end
    end

    class NeqCondition
      private

      def comparison_mode
        :neq
      end
    end

    class GtCondition
      private

      def comparison_mode
        :cmp
      end
    end

    class GteCondition
      private

      def comparison_mode
        :cmp
      end
    end

    class LtCondition
      private

      def comparison_mode
        :cmp
      end
    end

    class LteCondition
      private

      def comparison_mode
        :cmp
      end
    end

    class Projection
      def infer_type(checker, current)
        target_type = @target.infer_type(checker, current)
        projection = @projection
        checker.projection_type(self, target_type) do |element_type|
          projection.infer_type(checker, element_type)
        end
      end

      def describe
        "#{@target.describe}[*]"
      end
    end

    class Flatten
      def infer_type(checker, current)
        flatten_type(@child.infer_type(checker, current))
      end

      private

      def flatten_type(type)
        case type
        when Types::ArrayType
          Types.array_of(flatten_element(type.element))
        when Types::UnionType
          Types.union(*type.types.map { |t| flatten_type(t) })
        when Types::AnyType
          Types.union(Types::NULL, Types::ARRAY_ANY)
        else
          Types::NULL
        end
      end

      def flatten_element(element)
        case element
        when Types::ArrayType
          element.element
        when Types::UnionType
          Types.union(*element.types.map { |t| flatten_element(t) })
        else
          element
        end
      end
    end

    class Slice
      def infer_type(checker, current)
        slice_type(current)
      end

      private

      def slice_type(type)
        case type
        when Types::ArrayType
          type
        when Types::UnionType
          Types.union(*type.types.map { |t| slice_type(t) })
        when Types::AnyType
          Types.union(Types::STRING, Types::ARRAY_ANY, Types::NULL)
        else
          type == Types::STRING ? Types::STRING : Types::NULL
        end
      end
    end

    class MultiSelectList
      def infer_type(checker, current)
        if current == Types::NULL
          Types::NULL
        else
          element = Types.union(*@children.map { |child| child.infer_type(checker, current) })
          list = Types.array_of(element)
          Types.maybe_null?(current) ? Types.union(list, Types::NULL) : list
        end
      end
    end

    class MultiSelectHash
      def infer_type(checker, current)
        if current == Types::NULL
          Types::NULL
        else
          fields = {}
          @kv_pairs.each do |pair|
            fields[pair.key] = pair.value.infer_type(checker, current)
          end
          object = Types::ObjectType.new(fields, [])
          Types.maybe_null?(current) ? Types.union(object, Types::NULL) : object
        end
      end
    end

    class Function
      def infer_type(checker, current)
        name = FUNCTIONS.key(self.class)
        spec = name && JMESPath::TypeChecker::FUNCTION_SPECS[name]
        if spec
          checker.check_function(self, name, spec, @children, current)
        else
          Types::ANY
        end
      end

      def describe
        name = FUNCTIONS.key(self.class) || 'unknown'
        "#{name}(#{@children.map(&:describe).join(', ')})"
      end
    end
  end
end
