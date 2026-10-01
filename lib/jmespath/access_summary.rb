# frozen_string_literal: true
module JMESPath
  # Statically analyzes a compiled JMESPath expression and summarizes which
  # data fields it may read. This is intended for consumers such as data
  # masking layers that need to know, before executing a query, which fields
  # the query can touch.
  #
  # The analysis is purely static: it walks the expression AST and never
  # evaluates it against data. Because the actual data is unknown, the
  # summary distinguishes:
  #
  # * required accesses - traversed unconditionally whenever the expression
  #   is evaluated (assuming the data has the expected structure),
  # * possible accesses - traversed only for some data, e.g. filter
  #   conditions, short-circuited `and`/`or` branches, or projection
  #   elements,
  # * wildcard ranges - positions where the expression reads every key or
  #   index of a value (projections, flatten, slices) so the exact fields
  #   cannot be enumerated,
  # * conservative unknowns - positions that cannot be inferred precisely;
  #   these always carry a reason and must be treated as "may read
  #   anything".
  #
  # The summary covers every field reference in the expression, including
  # filter conditions, `sort_by`/`map`/`min_by`/`max_by` expression
  # references and multi-select structures - not only the fields that
  # appear in the final result.
  #
  #     summary = JMESPath.access_summary("items[?price > `10`].name")
  #     summary.required_paths
  #     #=> ["items", "items[*]"]
  #     summary.possible_paths
  #     #=> ["items[*].name", "items[*].price"]
  #
  class AccessSummary
    # Path segment marker meaning "any array index", rendered as `[*]`.
    ANY_INDEX = :*

    # Path segment marker meaning "any object key", rendered as `.*`.
    ANY_KEY = :**

    # A single recorded access.
    #
    # * `path` - array of path segments (String keys, Integer indices,
    #   {ANY_INDEX} or {ANY_KEY}); `nil` when the location cannot be
    #   determined statically.
    # * `certainty` - `:required` or `:possible`.
    # * `kind` - `:field` (precise key/index), `:range` (wildcard range
    #   over unknown keys/indices) or `:unknown` (conservative fallback).
    # * `reason` - for `:range` and `:unknown` entries, why the access
    #   cannot be narrowed down further.
    Entry = Struct.new(:path, :certainty, :kind, :reason)

    # @api private
    # Tracks where the analyzer currently is inside the document. A `nil`
    # path means the location could not be determined; `reason` explains
    # why. When `mapping` is set, the context describes a synthetic
    # structure built by a multi-select: it maps keys/indices to the
    # contexts of the document values behind them.
    Context = Struct.new(:path, :certainty, :reason, :mapping)

    # The result of an {AccessSummary} analysis.
    class Summary
      # @return [Array<Entry>]
      attr_reader :entries

      def initialize(entries)
        @entries = entries
      end

      # @return [Array<Entry>] accesses traversed unconditionally.
      def required
        @entries.select { |entry| entry.certainty == :required }
      end

      # @return [Array<Entry>] accesses traversed only for some data.
      def possible
        @entries.select { |entry| entry.certainty == :possible }
      end

      # @return [Array<Entry>] wildcard ranges (`:range` entries), each
      #   with a reason.
      def wildcard_ranges
        @entries.select { |entry| entry.kind == :range }
      end

      # @return [Array<Entry>] conservative `:unknown` entries, each with
      #   a reason. When non-empty, the expression may read fields that
      #   could not be localized statically.
      def conservative
        @entries.select { |entry| entry.kind == :unknown }
      end

      # @return [Array<String>] rendered paths of all required accesses.
      def required_paths
        required.map { |entry| self.class.render_path(entry.path) }.uniq.sort
      end

      # @return [Array<String>] rendered paths of all possible accesses.
      def possible_paths
        possible.map { |entry| self.class.render_path(entry.path) }.uniq.sort
      end

      # Returns true when the given concrete access path (an array of
      # String/Symbol keys and Integer indices) is covered by this
      # summary, i.e. the expression may perform that access. Used to
      # verify the summary does not under-report actual accesses.
      #
      # @param [Array<String,Symbol,Integer>] segments
      # @return [Boolean]
      def cover?(segments)
        return true if @entries.any? { |entry| entry.kind == :unknown }

        segs = segments.map { |seg| seg.is_a?(Integer) ? seg : seg.to_s }
        @entries.any? do |entry|
          next false if entry.path.nil? || entry.path.length < segs.length

          segs.each_with_index.all? do |seg, index|
            segment_match?(entry.path[index], seg)
          end
        end
      end

      # @return [Hash] a plain-data representation of the summary.
      def to_h
        {
          'required' => required_paths,
          'possible' => possible_paths,
          'wildcard_ranges' => wildcard_ranges.map do |entry|
            { 'path' => self.class.render_path(entry.path), 'reason' => entry.reason }
          end,
          'conservative' => conservative.map do |entry|
            { 'path' => self.class.render_path(entry.path), 'reason' => entry.reason }
          end
        }
      end

      # @api private
      def self.render_path(path)
        return '<unknown>' if path.nil?
        return '@' if path.empty?

        path.each_with_object(+'') do |segment, out|
          case segment
          when Integer then out << "[#{segment}]"
          when ANY_INDEX then out << '[*]'
          when ANY_KEY then out << '.*'
          else
            out << '.' unless out.empty?
            out << segment.to_s
          end
        end
      end

      private

      def segment_match?(entry_segment, segment)
        case entry_segment
        when ANY_INDEX then segment.is_a?(Integer)
        when ANY_KEY then !segment.is_a?(Integer)
        when Integer then entry_segment == segment
        else entry_segment.to_s == segment
        end
      end
    end

    class << self
      # Analyzes a JMESPath expression without evaluating it against data.
      #
      # @param [String, Nodes::Node] expression a JMESPath expression
      #   string or an already parsed (compiled) expression AST.
      # @param [Hash] options options passed to {Parser} when `expression`
      #   is a String.
      # @return [Summary]
      def analyze(expression, options = {})
        ast = case expression
              when String then Parser.new(options).parse(expression)
              when Nodes::Node then expression
              else
                raise ArgumentError,
                      'expected a JMESPath expression String or parsed AST'
              end
        new.analyze(ast.optimize)
      end
    end

    # @api private
    def analyze(ast)
      @entries = Set.new
      transfer(ast, [Context.new([], :required, nil, nil)])
      Summary.new(@entries.to_a)
    end

    private

    # Walks `node`, recording every field it may read when evaluated in
    # `contexts`. Returns the contexts describing the value produced by
    # `node`, so chained expressions can be analyzed further.
    def transfer(node, contexts)
      return contexts unless node.is_a?(Nodes::Node) || node.is_a?(Nodes::Chain)

      case node
      when Nodes::ChainedField
        transfer_keys(node.instance_variable_get(:@keys), contexts)
      when Nodes::Field
        transfer_keys([node.instance_variable_get(:@key)], contexts)
      when Nodes::Chain
        node.instance_variable_get(:@children).reduce(contexts) do |ctxs, child|
          transfer(child, ctxs)
        end
      when Nodes::Subexpression
        transfer(node.instance_variable_get(:@right),
                 transfer(node.instance_variable_get(:@left), contexts))
      when Nodes::And, Nodes::Or
        left = transfer(node.instance_variable_get(:@left), contexts)
        right = transfer(node.instance_variable_get(:@right), downgrade(contexts))
        left + right
      when Nodes::Not
        transfer(node.instance_variable_get(:@expression), contexts)
        []
      when Nodes::ComparatorCondition
        transfer(node.instance_variable_get(:@left), contexts)
        transfer(node.instance_variable_get(:@right), contexts)
        transfer(node.instance_variable_get(:@child), downgrade(contexts))
      when Nodes::Comparator
        transfer(node.instance_variable_get(:@left), contexts)
        transfer(node.instance_variable_get(:@right), contexts)
        []
      when Nodes::Condition
        transfer(node.instance_variable_get(:@test), contexts)
        transfer(node.instance_variable_get(:@child), downgrade(contexts))
      when Nodes::Current
        contexts
      when Nodes::Flatten
        child = transfer(node.instance_variable_get(:@child), contexts)
        nested = element_contexts(child, 'flatten reads every element')
        element_contexts(nested, 'flatten reads every element of nested arrays')
        child + nested
      when Nodes::ArrayProjection
        targets = transfer(node.instance_variable_get(:@target), contexts)
        transfer(node.instance_variable_get(:@projection),
                 element_contexts(targets, 'array projection reads every element'))
      when Nodes::ObjectProjection
        targets = transfer(node.instance_variable_get(:@target), contexts)
        transfer(node.instance_variable_get(:@projection),
                 element_contexts(targets, 'object projection reads every value', ANY_KEY))
      when Nodes::Slice
        start = node.instance_variable_get(:@start)
        stop = node.instance_variable_get(:@stop)
        step = node.instance_variable_get(:@step)
        record_range(contexts, "array slice [#{start}:#{stop}:#{step}] reads a range of indices")
        contexts
      when Nodes::Literal
        []
      when Nodes::MultiSelectHash
        mapping = {}
        node.instance_variable_get(:@kv_pairs).each do |pair|
          mapping[pair.key] = transfer(pair.value, contexts)
        end
        [Context.new(nil, nil, nil, mapping)]
      when Nodes::MultiSelectList
        mapping = {}
        node.instance_variable_get(:@children).each_with_index do |child, index|
          mapping[index] = transfer(child, contexts)
        end
        [Context.new(nil, nil, nil, mapping)]
      when Nodes::Function
        transfer_function(node, contexts)
      when Nodes::Expression
        transfer(node.expression, contexts)
        []
      else
        unknown_contexts("unhandled node type #{node.class}")
      end
    end

    def transfer_keys(keys, contexts)
      keys.reduce(contexts) do |current, key|
        current.flat_map do |ctx|
          if ctx.mapping
            # selecting a key of a synthetic multi-select structure
            # unwraps the document values behind it
            ctx.mapping.fetch(key, [])
          elsif ctx.path.nil?
            record(nil, :possible, :unknown,
                   "reads #{key.inspect} at an unknown location (#{ctx.reason})")
            ctx
          else
            path = ctx.path
            # indexing into "any element" still yields an unknown element
            path += [key] unless key.is_a?(Integer) && path.last == ANY_INDEX
            record(path, ctx.certainty, :field, nil)
            Context.new(path, ctx.certainty, nil, nil)
          end
        end
      end
    end

    def transfer_function(node, contexts)
      name = Nodes::Function::FUNCTIONS.key(node.class)
      children = node.instance_variable_get(:@children)
      arg_contexts = children.map do |child|
        child.is_a?(Nodes::Expression) ? nil : transfer(child, contexts)
      end
      case name
      when 'map'
        elements = element_contexts(arg_contexts[1] || contexts,
                                    'map() applies its expression to every element')
        transfer_expression_arg(children[0], elements)
      when 'sort_by', 'min_by', 'max_by'
        elements = element_contexts(arg_contexts[0] || contexts,
                                    "#{name}() reads every element and evaluates its expression reference")
        transfer_expression_arg(children[1], elements)
        elements
      when 'sort', 'reverse'
        element_contexts(arg_contexts[0] || contexts, "#{name}() reads every element")
      when 'to_array'
        (arg_contexts[0] || []) +
          element_contexts(arg_contexts[0] || contexts, 'to_array() may read every element')
      when 'values'
        element_contexts(arg_contexts[0] || contexts, 'values() reads every value', ANY_KEY)
      when 'max', 'min'
        element_contexts(arg_contexts[0] || contexts, "#{name}() reads every element")
      when 'not_null', 'merge'
        arg_contexts.compact.inject(:+) || []
      when 'abs', 'avg', 'ceil', 'contains', 'ends_with', 'floor', 'join',
           'keys', 'length', 'starts_with', 'sum', 'to_number', 'to_string',
           'type'
        []
      else
        # functions registered at runtime cannot be analyzed precisely
        children.each do |child|
          next unless child.is_a?(Nodes::Expression)

          scope = element_contexts(contexts, "function #{name || 'unknown'}() applies " \
                                           'an expression reference to values from an unknown scope')
          transfer_expression_arg(child, scope)
        end
        unknown_contexts("result of function #{name || 'unknown'}() may alias document data")
      end
    end

    def transfer_expression_arg(child, contexts)
      transfer(child.expression, contexts) if child.is_a?(Nodes::Expression)
    end

    # Records a wildcard range at each context and returns contexts for
    # the elements/values behind the wildcard.
    def element_contexts(contexts, reason, wildcard = ANY_INDEX)
      contexts.flat_map do |ctx|
        if ctx.mapping
          # the elements of a synthetic structure are the document
          # values behind it
          concretize([ctx])
        elsif ctx.path.nil?
          record(nil, :possible, :unknown, reason)
          [Context.new(nil, :possible, ctx.reason || reason, nil)]
        else
          record(ctx.path + [wildcard], ctx.certainty, :range, reason)
          [Context.new(ctx.path + [wildcard], :possible, nil, nil)]
        end
      end
    end

    def record_range(contexts, reason, wildcard = ANY_INDEX)
      contexts.each do |ctx|
        next if ctx.mapping

        if ctx.path.nil?
          record(nil, :possible, :unknown, reason)
        else
          record(ctx.path + [wildcard], ctx.certainty, :range, reason)
        end
      end
    end

    # Resolves synthetic multi-select contexts to the concrete document
    # contexts of the values behind them.
    def concretize(contexts)
      contexts.flat_map do |ctx|
        if ctx.mapping
          concretize(ctx.mapping.values.inject(:+) || [])
        else
          [ctx]
        end
      end
    end

    def unknown_contexts(reason)
      record(nil, :possible, :unknown, reason)
      [Context.new(nil, :possible, reason, nil)]
    end

    def downgrade(contexts)
      contexts.map { |ctx| Context.new(ctx.path, :possible, ctx.reason, ctx.mapping) }
    end

    def record(path, certainty, kind, reason)
      @entries << Entry.new(path, certainty, kind, reason)
    end
  end
end
