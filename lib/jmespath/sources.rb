# frozen_string_literal: true

module JMESPath
  # Provenance information returned by {JMESPath.search_with_sources}.
  #
  # The {#sources} tree has the same shape as {#value}: arrays mirror arrays
  # and hashes mirror hashes. Each leaf is either:
  #
  # * {Sources::Origin} - the value was copied verbatim from the original
  #   document at the given RFC 6901 JSON Pointer; or
  # * {Sources::Derived} - the value was computed by the expression;
  #   {Sources::Derived#inputs} lists the JSON Pointers of the direct inputs
  #   used for the computation.
  #
  # Positions are propagated from values that actually flowed through the
  # evaluation; matching values in the original document are never located by
  # value equality.
  module Sources
    # A value copied straight from the original document.
    class Origin
      attr_reader :pointer

      def initialize(pointer)
        @pointer = pointer
      end

      def to_h
        { 'type' => 'origin', 'pointer' => @pointer }
      end

      def ==(other)
        other.is_a?(Origin) && other.pointer == @pointer
      end
      alias eql? ==

      def hash
        [Origin, @pointer].hash
      end
    end

    # A value computed by the expression. `sources` (when present) mirrors the
    # structure of the computed value so nested provenance stays available;
    # `inputs` lists the JSON Pointers of the direct inputs.
    class Derived
      attr_reader :inputs, :sources

      def initialize(inputs, sources = nil)
        @inputs = inputs.dup
        @sources = sources
      end

      def to_h
        hash = { 'type' => 'derived', 'inputs' => @inputs.dup }
        hash['sources'] = Sources.to_h(@sources) unless @sources.nil?
        hash
      end

      def ==(other)
        other.is_a?(Derived) && other.inputs == @inputs && other.sources == @sources
      end
      alias eql? ==

      def hash
        [Derived, @inputs, @sources].hash
      end
    end

    class << self
      # Recursively converts a sources tree into plain hashes.
      def to_h(sources)
        case sources
        when Origin, Derived then sources.to_h
        when Array then sources.map { |entry| to_h(entry) }
        when Hash then sources.each_with_object({}) { |(key, value), hash| hash[key.to_s] = to_h(value) }
        else sources
        end
      end
    end
  end

  # @api private
  #
  # Evaluates an expression tree while carrying a {Sources} entry alongside
  # every value. Values that flow through the tree are wrapped in a Located
  # value (value + source); the sources tree is assembled structurally rather
  # than reconstructed from result values.
  class SourceVisitor
    include Nodes::TypeChecker

    def initialize(options = {})
      @disable_visit_errors = options[:disable_visit_errors]
    end

    def visit(expression, data)
      root = Located.new(data, Sources::Origin.new(''))
      located = trace(expression, root)
      SearchResult.new(located.value, located.source)
    end

    # A value together with its provenance.
    class Located
      attr_reader :value, :source

      def initialize(value, source)
        @value = value
        @source = source
      end

      def inputs
        SourceVisitor.inputs_of(@source)
      end
    end

    # An expression reference that evaluates to Located values; stands in for
    # Nodes::Expression when invoking functions.
    class TraceExpression
      attr_reader :expression

      def initialize(visitor, expression)
        @visitor = visitor
        @expression = expression
      end

      def eval(located)
        @visitor.trace(@expression, located)
      end
    end

    def trace(node, ctx)
      case node
      when Nodes::Current
        ctx
      when Nodes::Field
        trace_field(node.instance_variable_get(:@key), ctx)
      when Nodes::Subexpression
        trace_subexpression(node, ctx)
      when Nodes::Slice
        trace_slice(node, ctx)
      when Nodes::Flatten
        trace_flatten(node, ctx)
      when Nodes::ArrayProjection, Nodes::ObjectProjection
        trace_projection(node, ctx, node.is_a?(Nodes::ObjectProjection))
      when Nodes::Condition
        trace_condition(node, ctx)
      when Nodes::Comparator
        trace_comparator(node, ctx)
      when Nodes::MultiSelectHash
        trace_multi_select_hash(node, ctx)
      when Nodes::MultiSelectList
        trace_multi_select_list(node, ctx)
      when Nodes::Function
        trace_function(node, ctx)
      when Nodes::And
        left = trace(node.instance_variable_get(:@left), ctx)
        Util.falsey?(left.value) ? left : trace(node.instance_variable_get(:@right), ctx)
      when Nodes::Or
        left = trace(node.instance_variable_get(:@left), ctx)
        Util.falsey?(left.value) ? trace(node.instance_variable_get(:@right), ctx) : left
      when Nodes::Not
        operand = trace(node.instance_variable_get(:@expression), ctx)
        Located.new(Util.falsey?(operand.value), Sources::Derived.new(operand.inputs))
      when Nodes::Literal
        Located.new(node.value, Sources::Derived.new([]))
      when Nodes::Expression
        expression = TraceExpression.new(self, node.instance_variable_get(:@expression))
        Located.new(expression, Sources::Derived.new([]))
      else
        raise Errors::RuntimeError, "unable to trace node #{node.class}"
      end
    end

    private

    def self.inputs_of(source)
      case source
      when Sources::Origin then [source.pointer]
      when Sources::Derived then source.inputs
      else []
      end
    end

    def collect_inputs(located_values)
      located_values.grep(Located).flat_map(&:inputs).uniq
    end

    def derived(value, sources, located_values)
      Located.new(value, Sources::Derived.new(collect_inputs(Array(located_values)), sources))
    end

    def derived_array(pairs, located_values)
      derived(pairs.map(&:value), pairs.map(&:source), located_values)
    end

    def pointer_join(pointer, token)
      "#{pointer}/#{pointer_escape(token)}"
    end

    def pointer_escape(token)
      token.to_s.gsub('~', '~0').gsub('/', '~1')
    end

    def child_source(source, token)
      case source
      when Sources::Origin
        Sources::Origin.new(pointer_join(source.pointer, token))
      when Sources::Derived
        nested = source.sources
        key = token.is_a?(Integer) ? token : token.to_s
        nested[key] if nested.is_a?(Array) || nested.is_a?(Hash)
      end
    end

    # Mirrors Nodes::Field#visit while keeping the position of the child.
    def trace_field(key, ctx)
      value = ctx.value
      if value.respond_to?(:to_ary) && key.is_a?(Integer)
        array = value.to_ary
        index = key < 0 ? key + array.size : key
        return Located.new(nil, nil) unless index >= 0 && index < array.size

        Located.new(array[index], child_source(ctx.source, index))
      elsif value.respond_to?(:to_hash)
        hash = value.to_hash
        field_value =
          if !(v = hash[key]).nil?
            v
          elsif key.respond_to?(:to_sym) && !(v = hash[key.to_sym]).nil?
            v
          elsif hash.key?(key)
            hash[key]
          elsif key.respond_to?(:to_sym) && hash.key?(key.to_sym)
            hash[key.to_sym]
          else
            return Located.new(nil, nil)
          end
        Located.new(field_value, child_source(ctx.source, key.to_s))
      elsif value.is_a?(Struct) && value.respond_to?(key)
        Located.new(value[key], child_source(ctx.source, key.to_s))
      else
        Located.new(nil, nil)
      end
    end

    def trace_subexpression(node, ctx)
      [node.instance_variable_get(:@left), node.instance_variable_get(:@right)].reduce(ctx) do |current, child|
        traced = trace(child, current)
        return Located.new(nil, nil) if traced.value.nil? && traced.source.nil?

        traced
      end
    end

    def trace_slice(node, ctx)
      value = ctx.value
      if value.respond_to?(:to_str)
        sliced = node.visit(value.to_str)
        Located.new(sliced, Sources::Derived.new(ctx.inputs))
      elsif value.respond_to?(:to_ary)
        array = value.to_ary
        start, stop, step = node.send(:adjust_slice, array.size,
                                     node.instance_variable_get(:@start),
                                     node.instance_variable_get(:@stop),
                                     node.instance_variable_get(:@step))
        pairs = []
        if step > 0
          i = start
          while i < stop
            pairs << Located.new(array[i], child_source(ctx.source, i)) if i.between?(0, array.size - 1)
            i += step
          end
        else
          i = start
          while i > stop
            pairs << Located.new(array[i], child_source(ctx.source, i)) if i.between?(0, array.size - 1)
            i += step
          end
        end
        derived_array(pairs, [ctx])
      else
        Located.new(nil, nil)
      end
    end

    def trace_flatten(node, ctx)
      target = trace(node.instance_variable_get(:@child), ctx)
      return Located.new(nil, nil) unless target.value.respond_to?(:to_ary)

      nested_sources = target.source.is_a?(Sources::Derived) ? target.source.sources : nil
      pairs = []
      target.value.to_ary.each_with_index do |item, index|
        item_source =
          if target.source.is_a?(Sources::Origin)
            Sources::Origin.new(pointer_join(target.source.pointer, index))
          elsif nested_sources.is_a?(Array)
            nested_sources[index]
          end
        if item.respond_to?(:to_ary)
          item.to_ary.each_with_index do |element, inner_index|
            pairs << Located.new(element, child_source(item_source, inner_index))
          end
        else
          pairs << Located.new(item, item_source)
        end
      end
      derived_array(pairs, [target])
    end

    def trace_projection(node, ctx, object)
      target = trace(node.instance_variable_get(:@target), ctx)
      elements =
        if object
          object_elements(target)
        elsif target.value.respond_to?(:to_ary)
          array_elements(target)
        end
      return Located.new(nil, nil) unless elements

      projection_node = node.instance_variable_get(:@projection)
      pairs = []
      elements.each do |element|
        projected = trace(projection_node, element)
        pairs << projected unless projected.value.nil?
      end
      derived_array(pairs, [target])
    end

    def array_elements(target)
      array = target.value.to_ary
      nested = target.source.is_a?(Sources::Derived) ? target.source.sources : nil
      array.each_with_index.map do |value, index|
        source =
          if target.source.is_a?(Sources::Origin)
            Sources::Origin.new(pointer_join(target.source.pointer, index))
          elsif nested.is_a?(Array)
            nested[index]
          end
        Located.new(value, source)
      end
    end

    def object_elements(target)
      value = target.value
      hash =
        if value.respond_to?(:to_hash)
          value.to_hash
        elsif value.is_a?(Struct)
          value.each_pair.to_h
        end
      return nil unless hash

      nested = target.source.is_a?(Sources::Derived) ? target.source.sources : nil
      hash.map do |key, field_value|
        key_name = key.to_s
        source =
          if target.source.is_a?(Sources::Origin)
            Sources::Origin.new(pointer_join(target.source.pointer, key_name))
          elsif nested.is_a?(Hash)
            nested[key_name]
          end
        Located.new(field_value, source)
      end
    end

    def trace_condition(node, ctx)
      test = trace(node.instance_variable_get(:@test), ctx)
      if Util.falsey?(test.value)
        Located.new(nil, nil)
      else
        trace(node.instance_variable_get(:@child), ctx)
      end
    end

    def trace_comparator(node, ctx)
      left = trace(node.instance_variable_get(:@left), ctx)
      right = trace(node.instance_variable_get(:@right), ctx)
      result = node.send(:check, left.value, right.value)
      derived(result, nil, [left, right])
    end

    def trace_multi_select_hash(node, ctx)
      return Located.new(nil, nil) if ctx.value.nil?

      value = {}
      sources = {}
      inputs = []
      node.instance_variable_get(:@kv_pairs).each do |pair|
        traced = trace(pair.value, ctx)
        key = pair.key.to_s
        value[key] = traced.value
        sources[key] = traced.source
        inputs.concat(traced.inputs)
      end
      Located.new(value, Sources::Derived.new(inputs.uniq, sources))
    end

    def trace_multi_select_list(node, ctx)
      return Located.new(nil, nil) if ctx.value.nil?

      pairs = node.instance_variable_get(:@children).map { |child| trace(child, ctx) }
      derived_array(pairs, pairs)
    end

    FUNCTION_HANDLERS = {
      Nodes::Map => :fn_map,
      Nodes::SortFunction => :fn_sort,
      Nodes::SortByFunction => :fn_sort_by,
      Nodes::MaxFunction => :fn_max,
      Nodes::MinFunction => :fn_min,
      Nodes::MaxByFunction => :fn_max_by,
      Nodes::MinByFunction => :fn_min_by,
      Nodes::ReverseFunction => :fn_reverse,
      Nodes::ToArrayFunction => :fn_to_array,
      Nodes::KeysFunction => :fn_keys,
      Nodes::ValuesFunction => :fn_values,
      Nodes::JoinFunction => :fn_join,
      Nodes::MergeFunction => :fn_merge,
      Nodes::NotNullFunction => :fn_not_null
    }.freeze

    def trace_function(node, ctx)
      arguments = node.instance_variable_get(:@children).map do |child|
        traced = trace(child, ctx)
        traced.value.is_a?(TraceExpression) ? traced.value : traced
      end

      handler = FUNCTION_HANDLERS[node.class]
      handler ? send(handler, node, arguments) : invoke_generic(node, arguments)
    end

    def function_error(_node, error_type, message, arguments)
      unless @disable_visit_errors
        raise error_type, message
      end

      Located.new(nil, Sources::Derived.new(collect_inputs(arguments.grep(Located))))
    end

    def array_pairs(located)
      located.value.to_ary.each_with_index.map do |value, index|
        Located.new(value, child_source(located.source, index))
      end
    end

    def fn_map(node, arguments)
      return function_error(node, Errors::InvalidArityError, 'function map() expects two arguments', arguments) unless arguments.count == 2

      expression, list = arguments[0], arguments[1]
      unless expression.is_a?(TraceExpression)
        return function_error(node, Errors::InvalidTypeError, 'function map() expects the first argument to be an expression', arguments)
      end
      unless list.value.respond_to?(:to_ary)
        return function_error(node, Errors::InvalidTypeError, 'function map() expects the second argument to be a list', arguments)
      end

      pairs = array_pairs(list).map { |element| expression.eval(element) }
      derived_array(pairs, [list])
    end

    def fn_sort(node, arguments)
      return function_error(node, Errors::InvalidArityError, 'function sort() expects one argument', arguments) unless arguments.count == 1

      list = arguments.first
      unless list.value.respond_to?(:to_ary)
        return function_error(node, Errors::InvalidTypeError,
                              'function sort() expects values to be an array of numbers or integers', arguments)
      end

      pairs = array_pairs(list)
      return derived_array([], [list]) if pairs.empty?

      array_type = get_type(pairs.first.value)
      unless array_type == STRING_TYPE || array_type == NUMBER_TYPE
        return function_error(node, Errors::InvalidTypeError,
                              'function sort() expects values to be an array of numbers or integers', arguments)
      end

      n = 0
      sorted = pairs.sort_by do |pair|
        unless get_type(pair.value) == array_type
          return function_error(node, Errors::InvalidTypeError,
                                'function sort() expects values to be an array of only numbers, or only integers',
                                arguments)
        end
        n += 1
        [pair.value, n]
      end
      derived_array(sorted, [list])
    end

    def fn_sort_by(node, arguments)
      unless arguments.count == 2
        return function_error(node, Errors::InvalidArityError, 'function sort_by() expects two arguments', arguments)
      end

      list, expression = arguments
      unless get_type(list&.value) == ARRAY_TYPE && expression.is_a?(TraceExpression)
        return function_error(node, Errors::InvalidTypeError,
                              'function sort_by() expects an array and an expression', arguments)
      end

      pairs = array_pairs(list)
      return derived_array([], [list]) if pairs.empty?

      array_type = get_type(expression.eval(pairs.first).value)
      unless array_type == STRING_TYPE || array_type == NUMBER_TYPE
        return function_error(node, Errors::InvalidTypeError,
                              'function sort() expects values to be an array of numbers or integers', arguments)
      end

      n = 0
      sorted = pairs.sort_by do |pair|
        keyed = expression.eval(pair)
        unless get_type(keyed.value) == array_type
          return function_error(node, Errors::InvalidTypeError,
                                'function sort() expects values to be an array of only numbers, or only integers',
                                arguments)
        end
        n += 1
        [keyed.value, n]
      end
      derived_array(sorted, [list, expression])
    end

    def pick_extreme(node, arguments, greater)
      return function_error(node, Errors::InvalidArityError, "function #{greater ? 'max' : 'min'}() expects one argument", arguments) unless arguments.count == 1

      list = arguments.first
      unless list.value.respond_to?(:to_ary)
        return function_error(node, Errors::InvalidTypeError, "function #{greater ? 'max' : 'min'}() expects an array", arguments)
      end

      pairs = array_pairs(list)
      return Located.new(nil, Sources::Derived.new(list.inputs)) if pairs.empty?

      type = get_type(pairs.first.value)
      unless type == NUMBER_TYPE || type == STRING_TYPE
        return function_error(node, Errors::InvalidTypeError,
                              "function #{greater ? 'max' : 'min'}() expects numeric or string values", arguments)
      end

      chosen = pairs.first
      pairs.each do |pair|
        unless get_type(pair.value) == type
          return function_error(node, Errors::InvalidTypeError,
                                "function #{greater ? 'max' : 'min'}() encountered a type mismatch in sequence: #{type}, #{get_type(pair.value)}",
                                arguments)
        end
        chosen = pair if greater ? pair.value > chosen.value : pair.value < chosen.value
      end
      chosen
    end

    def fn_max(node, arguments)
      pick_extreme(node, arguments, true)
    end

    def fn_min(node, arguments)
      pick_extreme(node, arguments, false)
    end

    def pick_by_extreme(node, arguments, greater)
      list, expression = arguments
      unless list&.value&.respond_to?(:to_ary) && expression.is_a?(TraceExpression)
        return function_error(node, Errors::InvalidTypeError,
                              "function #{greater ? 'max_by' : 'min_by'}() expects an array and an expression",
                              arguments)
      end

      pairs = array_pairs(list)
      return Located.new(nil, Sources::Derived.new(list.inputs)) if pairs.empty?

      type = get_type(expression.eval(pairs.first).value)
      unless type == NUMBER_TYPE || type == STRING_TYPE
        return function_error(node, Errors::InvalidTypeError,
                              "function #{greater ? 'max_by' : 'min_by'}() expects values to be strings or numbers",
                              arguments)
      end

      chosen = pairs.first
      chosen_key = expression.eval(chosen)
      pairs.each do |pair|
        keyed = expression.eval(pair)
        unless get_type(keyed.value) == type
          return function_error(node, Errors::InvalidTypeError,
                                "function #{greater ? 'max_by' : 'min_by'}() encountered a type mismatch in sequence: #{type}, #{get_type(keyed.value)}",
                                arguments)
        end
        if greater ? keyed.value > chosen_key.value : keyed.value < chosen_key.value
          chosen = pair
          chosen_key = keyed
        end
      end
      chosen
    end

    def fn_max_by(node, arguments)
      pick_by_extreme(node, arguments, true)
    end

    def fn_min_by(node, arguments)
      pick_by_extreme(node, arguments, false)
    end

    def fn_reverse(node, arguments)
      value = arguments.first.value
      if value.respond_to?(:to_ary)
        derived_array(array_pairs(arguments.first).reverse, [arguments.first])
      elsif value.respond_to?(:to_str)
        derived(value.to_str.reverse, nil, [arguments.first])
      else
        function_error(node, Errors::InvalidTypeError, 'function reverse() expects an array or string', arguments)
      end
    end

    def fn_to_array(node, arguments)
      located = arguments.first
      if located.value.respond_to?(:to_ary)
        derived_array(array_pairs(located), [located])
      else
        derived([located.value], [located.source], [located])
      end
    end

    def fn_keys(node, arguments)
      located = arguments.first
      value = located.value
      keys =
        if value.respond_to?(:to_hash)
          value.to_hash.keys.map(&:to_s)
        elsif value.is_a?(Struct)
          value.members.map(&:to_s)
        end
      return Located.new(keys, Sources::Derived.new(located.inputs)) if keys

      function_error(node, Errors::InvalidTypeError, 'function keys() expects a hash', arguments)
    end

    def fn_values(node, arguments)
      located = arguments.first
      pairs = object_elements(located) || array_pairs(located)
      if located.value.respond_to?(:to_hash) || located.value.is_a?(Struct) || located.value.respond_to?(:to_ary)
        derived_array(pairs, [located])
      else
        function_error(node, Errors::InvalidTypeError, 'function values() expects an array or a hash', arguments)
      end
    end

    def fn_join(node, arguments)
      glue, values = arguments[0], arguments[1]
      if !glue.value.respond_to?(:to_str)
        function_error(node, Errors::InvalidTypeError,
                       'function join() expects the first argument to be a string', arguments)
      elsif values.value.respond_to?(:to_ary) && values.value.to_ary.all? { |v| v.respond_to?(:to_str) }
        derived(values.value.to_ary.join(glue.value.to_str), nil, [glue, values])
      else
        function_error(node, Errors::InvalidTypeError,
                       'function join() expects values to be an array of strings', arguments)
      end
    end

    def fn_merge(node, arguments)
      plain = arguments.map(&:value)
      merged = plain.inject({}) { |hash, value| hash.merge(value) }
      sources = {}
      arguments.each do |located|
        located.value.to_hash.each_pair do |key, value|
          key_name = key.to_s
          sources[key_name] = child_source(located.source, key_name)
        end
      end
      derived(merged, sources, arguments)
    end

    def fn_not_null(node, arguments)
      found = arguments.find { |located| located.is_a?(Located) && !located.value.nil? }
      found || Located.new(nil, Sources::Derived.new(collect_inputs(arguments.grep(Located))))
    end

    def invoke_generic(node, arguments)
      plain_arguments = arguments.map do |argument|
        argument.is_a?(TraceExpression) ? argument.expression : argument.value
      end
      result = node.send(:call, plain_arguments)
      located_arguments = arguments.grep(Located)
      Located.new(result, Sources::Derived.new(collect_inputs(located_arguments)))
    end
  end

  # Returned by {JMESPath.search_with_sources}.
  class SearchResult
    attr_reader :value, :sources

    def initialize(value, sources)
      @value = value
      @sources = sources
    end

    def to_h
      { 'value' => @value, 'sources' => JMESPath::Sources.to_h(@sources) }
    end

    def ==(other)
      other.is_a?(SearchResult) && other.value == @value && other.sources == @sources
    end
    alias eql? ==

    def hash
      [SearchResult, @value, @sources].hash
    end
  end
end
