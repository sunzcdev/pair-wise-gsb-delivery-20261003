# frozen_string_literal: true
module JMESPath
  # Statically analyzes a compiled JMESPath expression and reports which
  # data fields it may read. This is intended for data-masking / auditing
  # layers that need to know, before executing a query, which fields the
  # query can touch.
  #
  # The analysis never evaluates the expression against real data. When an
  # access pattern cannot be determined precisely (wildcards, slices,
  # indices into projected lists, functions that consume whole values,
  # etc.) it falls back to a conservative, wider range and documents the
  # reason on the access entry.
  #
  #     summary = JMESPath.access_summary('users[?age > `20`].name')
  #     summary.required  # accesses read on every code path (incl. filters)
  #     summary.possible  # accesses guarded by data-dependent conditions
  #     summary.wildcards # accesses that fan out over a range of keys
  #
  module AccessSummary
    # Path segment used when any key or index at a level may be read.
    WILDCARD = :*

    # Marker used by access recorders to indicate that every element or
    # value of a container was read (iteration).
    ITERATE = :__iterate__

    # A single field access that a compiled expression may perform.
    #
    # * `:path` - array of segments (String keys, Integer indices, or
    #   {WILDCARD}) from the document root.
    # * `:certainty` - `:required` (read on every code path that reaches
    #   the node) or `:possible` (guarded by a data-dependent condition
    #   such as a filter test, `&&`/`||` short-circuit, or a function
    #   callback).
    # * `:kind` - `:field`, `:index`, `:wildcard` (every element, value,
    #   or key of a container) or `:subtree` (the whole value, to an
    #   unknown depth).
    # * `:context` - `:output`, `:filter`, `:sort_key`, or `:function`.
    # * `:detail` - for wildcards: `:array_elements`, `:object_values`,
    #   `:object_keys`, or `:array_range`.
    # * `:reason` - explanation for conservative (wildcard/subtree)
    #   entries.
    Access = Struct.new(:path, :certainty, :kind, :context, :detail, :reason) do
      def to_s
        flags = [certainty, kind, context].join(', ')
        string = "#{AccessSummary.format_path(path)} (#{flags})"
        reason ? "#{string} - #{reason}" : string
      end
    end

    class << self
      # @param [Nodes::Node] node a compiled (and optimized) expression
      # @return [Summary]
      def analyze(node)
        Analyzer.new.analyze(node)
      end

      # @param [Array] segments
      # @return [String] e.g. "users[*].name"
      def format_path(segments)
        string = String.new
        segments.each do |segment|
          case segment
          when Integer
            string << "[#{segment}]"
          when WILDCARD
            string << '[*]'
          else
            string << '.' unless string.empty?
            string << segment.to_s
          end
        end
        string.empty? ? '@' : string
      end
    end

    # The result of statically analyzing a compiled expression.
    class Summary
      def initialize(accesses)
        @accesses = accesses
      end

      # @return [Array<Access>] every access the expression may perform
      attr_reader :accesses

      # @return [Array<Access>] accesses read on every code path that
      #   reaches them, including filter tests and function arguments
      def required
        @accesses.select { |access| access.certainty == :required }
      end

      # @return [Array<Access>] accesses guarded by a data-dependent
      #   condition (filter results, `&&`/`||` short-circuits, etc.)
      def possible
        @accesses.select { |access| access.certainty == :possible }
      end

      # @return [Array<Access>] accesses that fan out over a range of
      #   keys or consume a whole subtree; each carries a `:reason`
      def wildcards
        @accesses.select { |access| access.kind == :wildcard || access.kind == :subtree }
      end

      # @return [Array<Access>] precise field and index accesses
      def fields
        @accesses.select { |access| access.kind == :field || access.kind == :index }
      end

      # Returns `true` when the summary accounts for an actual read of
      # `path`. `path` is an array of concrete segments (String/Symbol
      # keys, Integer indices), optionally terminated by {ITERATE} to
      # indicate that the whole container at the path was iterated.
      #
      # @param [Array] path
      # @return [Boolean]
      def covers?(path)
        iterate = path.last == ITERATE
        segments = iterate ? path[0..-2] : path
        segments = segments.map { |segment| segment.is_a?(Symbol) ? segment.to_s : segment }
        @accesses.any? { |access| access_covers?(access, segments, iterate) }
      end

      # @return [Hash] a plain-data representation of the summary
      def to_h
        {
          'required' => required.map { |access| access_to_h(access) },
          'possible' => possible.map { |access| access_to_h(access) },
          'wildcards' => wildcards.map { |access| access_to_h(access) }
        }
      end

      private

      def access_to_h(access)
        {
          'path' => AccessSummary.format_path(access.path),
          'certainty' => access.certainty.to_s,
          'kind' => access.kind.to_s,
          'context' => access.context.to_s,
          'detail' => access.detail&.to_s,
          'reason' => access.reason
        }
      end

      def access_covers?(access, segments, iterate)
        case access.kind
        when :subtree
          # reading the whole value covers any read at or below the path
          segments.length >= access.path.length &&
            match_segments?(access.path, segments.first(access.path.length))
        when :wildcard
          # covers the container itself (iteration) and any single
          # element directly below it
          return false unless match_segments?(access.path, segments.first(access.path.length))
          iterate ? segments.length == access.path.length : segments.length <= access.path.length + 1
        when :field, :index
          # covers the exact read and reads of intermediate containers
          # along the way (reading `a.b.c` implies reading `a` and `a.b`)
          return false if iterate
          segments.length <= access.path.length &&
            match_segments?(access.path.first(segments.length), segments)
        else
          false
        end
      end

      def match_segments?(pattern, concrete)
        pattern.length == concrete.length &&
          pattern.each_with_index.all? { |segment, i| segment == WILDCARD || segment == concrete[i] }
      end
    end

    # @api private
    #
    # Describes where the value produced by a sub-expression lives,
    # relative to the source document. A *value* base points at a source
    # location (`segments`). A *list* base is a synthesized array (e.g.
    # the result of a projection); only its `elements` refer back to
    # source locations.
    class Base
      attr_reader :segments, :elements, :certainty, :context

      def initialize(segments: nil, elements: nil, certainty: :required, context: :output)
        @segments = segments
        @elements = elements
        @certainty = certainty
        @context = context
      end

      def self.value(segments, certainty: :required, context: :output)
        new(segments: segments, certainty: certainty, context: context)
      end

      def self.list(elements, certainty: :required, context: :output)
        new(elements: elements, certainty: certainty, context: context)
      end

      def list?
        !@elements.nil?
      end

      def possible
        Base.new(segments: @segments, elements: @elements, certainty: :possible, context: @context)
      end

      def in_context(context)
        Base.new(segments: @segments, elements: @elements, certainty: @certainty, context: context)
      end
    end

    # @api private
    #
    # Walks an optimized expression tree without touching any data and
    # records the field accesses the expression may perform. Nodes that
    # cannot be modeled precisely produce conservative wildcard/subtree
    # accesses annotated with a reason.
    class Analyzer
      def analyze(node)
        @accesses = []
        walk(node, [Base.value([])])
        Summary.new(@accesses.uniq)
      end

      private

      def walk(node, bases)
        case node
        when Nodes::Chain then walk_chain(node, bases)
        when Nodes::ChainedField then walk_field(chained_field_keys(node), bases)
        when Nodes::Field then walk_field([node.instance_variable_get(:@key)], bases)
        when Nodes::Projection then walk_projection(node, bases)
        when Nodes::ComparatorCondition then walk_comparator_condition(node, bases)
        when Nodes::Condition then walk_condition(node, bases)
        when Nodes::Comparator then walk_comparator(node, bases)
        when Nodes::And then walk_and_or(node, bases)
        when Nodes::Or then walk_and_or(node, bases)
        when Nodes::Not then walk_not(node, bases)
        when Nodes::MultiSelectHash then walk_multi_select_hash(node, bases)
        when Nodes::MultiSelectList then walk_multi_select_list(node, bases)
        when Nodes::Function then walk_function(node, bases)
        when Nodes::Expression then walk(node.expression, bases)
        when Nodes::Flatten then walk_flatten(node, bases)
        when Nodes::Slice then walk_slice(node, bases)
        when Nodes::Subexpression then walk_subexpression(node, bases)
        when Nodes::Current then bases
        when Nodes::Literal then []
        else
          record_bases(bases, :subtree,
                       reason: "unhandled node #{node.class}; conservatively assumes " \
                               'the current value is read in full')
          bases
        end
      end

      def walk_chain(node, bases)
        node.instance_variable_get(:@children).each do |child|
          bases = walk(child, bases)
        end
        bases
      end

      def walk_subexpression(node, bases)
        # only reachable for un-optimized trees; optimized expressions
        # flatten sub-expressions into chains
        left = node.instance_variable_get(:@left)
        right = node.instance_variable_get(:@right)
        walk(right, walk(left, bases))
      end

      def walk_field(keys, bases)
        keys.each do |key|
          bases = step_field(key, bases)
        end
        bases
      end

      def step_field(key, bases)
        stepped = []
        bases.each do |base|
          if base.list?
            # a string key reads nothing from a synthesized list, but an
            # index selects one of its elements; which source element
            # that is depends on the data
            next unless key.is_a?(Integer)
            base.elements.each do |element|
              next unless element.segments
              record(element.segments, :wildcard, element.certainty, element.context,
                     detail: :array_elements,
                     reason: "index [#{key}] is applied to a projected or computed list; " \
                             'which source element it selects depends on the data')
            end
            stepped.concat(base.elements)
          else
            kind = key.is_a?(Integer) ? :index : :field
            record(base.segments + [key], kind, base.certainty, base.context)
            stepped << Base.value(base.segments + [key],
                                  certainty: base.certainty, context: base.context)
          end
        end
        stepped
      end

      def walk_projection(node, bases)
        detail = node.is_a?(Nodes::ObjectProjection) ? :object_values : :array_elements
        reason = if detail == :object_values
                   'object wildcard * reads every value of the object'
                 else
                   'array projection reads every element of the array'
                 end
        target_bases = walk(node.instance_variable_get(:@target), bases)
        elements = []
        target_bases.each do |base|
          if base.list?
            # the list-producing node already accounted for its reads
            elements.concat(base.elements)
          else
            record(base.segments, :wildcard, base.certainty, base.context,
                   detail: detail, reason: reason)
            elements << Base.value(base.segments + [WILDCARD],
                                   certainty: base.certainty, context: base.context)
          end
        end
        projected = walk(node.instance_variable_get(:@projection), elements)
        [Base.list(projected, certainty: certainty_of(bases), context: context_of(bases))]
      end

      def walk_condition(node, bases)
        walk(node.instance_variable_get(:@test), in_context(bases, :filter))
        walk(node.instance_variable_get(:@child), possible(bases))
      end

      def walk_comparator_condition(node, bases)
        filter_bases = in_context(bases, :filter)
        operand_bases = []
        [:@left, :@right].each do |ivar|
          operand = node.instance_variable_get(ivar)
          # optimized conditions may hold raw literal values (e.g.
          # LiteralRightEqCondition) instead of nodes
          operand_bases.concat(walk(operand, filter_bases)) if expression_node?(operand)
        end
        if node.is_a?(Nodes::EqCondition) || node.is_a?(Nodes::NeqCondition)
          record_bases(operand_bases, :subtree,
                       reason: 'equality filters deep-read the operand to compare JSON values')
        end
        walk(node.instance_variable_get(:@child), possible(bases))
      end

      def walk_comparator(node, bases)
        operand_bases = walk(node.left, bases) + walk(node.right, bases)
        if node.is_a?(Nodes::Comparators::Eq) || node.is_a?(Nodes::Comparators::Neq)
          record_bases(operand_bases, :subtree,
                       reason: '==/!= deep-read both operands to compare JSON values')
        end
        []
      end

      def walk_and_or(node, bases)
        left = walk(node.instance_variable_get(:@left), bases)
        right = walk(node.instance_variable_get(:@right), possible(bases))
        # the result is one of the operand values; which one depends on
        # the data, so downstream reads become conditional
        possible(left + right)
      end

      def walk_not(node, bases)
        walk(node.instance_variable_get(:@expression), bases)
        []
      end

      def walk_multi_select_hash(node, bases)
        node.instance_variable_get(:@kv_pairs).flat_map do |pair|
          walk(pair.value, bases)
        end
      end

      def walk_multi_select_list(node, bases)
        node.instance_variable_get(:@children).flat_map do |child|
          walk(child, bases)
        end
      end

      def walk_flatten(node, bases)
        child_bases = walk(node.instance_variable_get(:@child), bases)
        elements = []
        child_bases.each do |base|
          if base.list?
            base.elements.each do |element|
              next unless element.segments
              record(element.segments, :wildcard, element.certainty, element.context,
                     detail: :array_elements,
                     reason: 'flatten [] reads every element, including one level of nested arrays')
              elements << element
              elements << Base.value(element.segments + [WILDCARD],
                                     certainty: element.certainty, context: element.context)
            end
          else
            record(base.segments, :wildcard, base.certainty, base.context,
                   detail: :array_elements,
                   reason: 'flatten [] reads every element, including one level of nested arrays')
            elements << Base.value(base.segments + [WILDCARD],
                                   certainty: base.certainty, context: base.context)
            elements << Base.value(base.segments + [WILDCARD, WILDCARD],
                                   certainty: base.certainty, context: base.context)
          end
        end
        [Base.list(elements, certainty: certainty_of(bases), context: context_of(bases))]
      end

      def walk_slice(node, bases)
        start = node.instance_variable_get(:@start)
        stop = node.instance_variable_get(:@stop)
        step = node.instance_variable_get(:@step)
        slice = "[#{start}:#{stop}#{step.nil? ? '' : ":#{step}"}]"
        elements = []
        bases.each do |base|
          next if base.list?
          record(base.segments, :wildcard, base.certainty, base.context,
                 detail: :array_range,
                 reason: "slice #{slice} selects a data-dependent range of elements")
          elements << Base.value(base.segments + [WILDCARD],
                                 certainty: base.certainty, context: base.context)
        end
        [Base.list(elements, certainty: certainty_of(bases), context: context_of(bases))]
      end

      def walk_function(node, bases)
        name = function_name(node)
        children = node.instance_variable_get(:@children)
        arg_bases = in_context(bases, :function)
        case name
        when 'sort_by', 'max_by', 'min_by'
          list_bases = walk(children[0], arg_bases)
          elements = collect_elements(list_bases, detail: :array_elements,
                                      reason: "#{name}() reads every element of the array " \
                                              'to compare sort keys')
          walk_expression_arg(children[1], in_context(elements, :sort_key), arg_bases)
          if name == 'sort_by'
            [Base.list(elements, certainty: certainty_of(bases), context: context_of(bases))]
          else
            # max_by/min_by return a single element of the array
            elements
          end
        when 'map'
          list_bases = walk(children[1], arg_bases)
          elements = collect_elements(list_bases, detail: :array_elements,
                                      reason: 'map() reads every element of the array')
          mapped = walk_expression_arg(children[0], elements, arg_bases)
          [Base.list(mapped, certainty: certainty_of(bases), context: context_of(bases))]
        when 'keys'
          record_bases(walk(children[0], arg_bases), :wildcard, detail: :object_keys,
                       reason: 'keys() reads every key of the object')
          []
        when 'values'
          value_bases = walk(children[0], arg_bases)
          record_bases(value_bases, :wildcard, detail: :object_values,
                       reason: 'values() reads every value of the object or array')
          [Base.list(element_bases(value_bases),
                     certainty: certainty_of(bases), context: context_of(bases))]
        when 'not_null'
          children.flat_map { |child| walk(child, arg_bases) }
        when 'to_array'
          elements = children.empty? ? [] : walk(children[0], arg_bases)
          [Base.list(elements, certainty: certainty_of(bases), context: context_of(bases))]
        when 'sort', 'reverse'
          elements = collect_elements(walk(children[0], arg_bases), detail: :array_elements,
                                      reason: "#{name}() reads every element of the array")
          [Base.list(elements, certainty: certainty_of(bases), context: context_of(bases))]
        when 'sum', 'avg', 'min', 'max'
          collect_elements(walk(children[0], arg_bases), detail: :array_elements,
                           reason: "#{name}() reads every element of the array")
          []
        when 'join'
          walk(children[0], arg_bases)
          collect_elements(walk(children[1], arg_bases), detail: :array_elements,
                           reason: 'join() reads every element of the array')
          []
        when 'contains'
          haystack = walk(children[0], arg_bases)
          walk(children[1], arg_bases)
          collect_elements(haystack, detail: :array_elements,
                           reason: 'contains() may inspect every element of the array')
          []
        when 'merge'
          children.flat_map do |child|
            child_bases = walk(child, arg_bases)
            record_bases(child_bases, :subtree,
                         reason: 'merge() copies every key/value pair of its arguments')
            element_bases(child_bases)
          end
        when 'to_string'
          record_bases(walk(children[0], arg_bases), :subtree,
                       reason: 'to_string() serializes the entire value when it is an ' \
                               'object or array')
          []
        when 'abs', 'ceil', 'floor', 'type', 'length', 'to_number',
             'ends_with', 'starts_with'
          children.each { |child| walk(child, arg_bases) }
          []
        else
          children.each { |child| walk(child, arg_bases) }
          record_bases(bases, :subtree,
                       reason: "function #{name}() is not modeled; conservatively assumes " \
                               'it reads the current value in full')
          []
        end
      end

      def walk_expression_arg(node, expression_bases, fallback_bases)
        if node.is_a?(Nodes::Expression)
          walk(node.expression, expression_bases)
        else
          walk(node, fallback_bases)
        end
      end

      def function_name(node)
        @function_names ||=
          Nodes::Function::FUNCTIONS.each_with_object({}) do |(name, type), names|
            names[type] = name
          end
        @function_names[node.class] || 'unknown'
      end

      def collect_elements(bases, detail:, reason:)
        bases.each do |base|
          if base.list?
            base.elements.each do |element|
              next unless element.segments
              record(element.segments, :subtree, element.certainty, element.context,
                     reason: reason)
            end
          else
            record(base.segments, :wildcard, base.certainty, base.context,
                   detail: detail, reason: reason)
          end
        end
        element_bases(bases)
      end

      def element_bases(bases)
        bases.flat_map do |base|
          if base.list?
            base.elements
          else
            [Base.value(base.segments + [WILDCARD],
                        certainty: base.certainty, context: base.context)]
          end
        end
      end

      def record_bases(bases, kind, detail: nil, reason: nil)
        bases.each do |base|
          if base.list?
            base.elements.each do |element|
              next unless element.segments
              record(element.segments, kind, element.certainty, element.context,
                     detail: detail, reason: reason)
            end
          else
            record(base.segments, kind, base.certainty, base.context,
                   detail: detail, reason: reason)
          end
        end
      end

      def record(segments, kind, certainty, context, detail: nil, reason: nil)
        @accesses << Access.new(segments, certainty, kind, context, detail, reason)
      end

      def possible(bases)
        bases.map(&:possible)
      end

      def in_context(bases, context)
        bases.map { |base| base.in_context(context) }
      end

      def certainty_of(bases)
        bases.first ? bases.first.certainty : :required
      end

      def context_of(bases)
        bases.first ? bases.first.context : :output
      end

      def chained_field_keys(node)
        node.instance_variable_get(:@keys)
      end

      def expression_node?(value)
        # Chain does not inherit from Nodes::Node
        value.is_a?(Nodes::Node) || value.is_a?(Nodes::Chain)
      end
    end
  end
end
