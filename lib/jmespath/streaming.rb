# frozen_string_literal: true
module JMESPath
  # Streaming evaluation of JMESPath expressions over read-only array
  # sources. Streaming lets filters and projections produce results while
  # data is being pulled from the source, instead of requiring the full
  # array up front.
  #
  # ## Source protocol
  #
  # A source is any read-only object that yields its elements in order:
  #
  # * Pull sources respond to `next`, returning the next element and
  #   raising `StopIteration` when exhausted (like `Enumerator`).
  # * Iterable sources respond to `each`, yielding every element in order.
  #
  # Sources may optionally respond to `close`. When present, `close` is
  # called exactly once: when the stream is fully consumed, when the
  # consumer stops early (e.g. `first(3)`), or when evaluation raises.
  #
  # ## Streamable nodes
  #
  # The following nodes are evaluated incrementally, element by element:
  #
  # * `Current` (`@`)
  # * `ArrayProjection` (including filter projections like `[?a > \`1\`].b`
  #   and their optimized fast variants)
  # * `Flatten` (`[]`)
  # * `Slice` with a non-negative start/stop and a positive step
  # * `Chain`s composed solely of the nodes above
  #
  # Every other node requires materializing the source into an array
  # before evaluation. This includes functions such as `sort`, `sort_by`,
  # `reverse` and `length`, negative array indices, object projections,
  # field access, comparators, boolean operators, literals and
  # multi-selects. When `:allow_materialization` is `false`, expressions
  # containing such nodes are rejected with
  # {Errors::MaterializationError} before the source is consumed.
  module Streaming
    class << self
      # @api private
      # @param [Nodes::Node] node An optimized expression tree.
      # @param [#next,#each] source A read-only array source.
      # @param [Boolean] allow_materialization
      # @return [Stream]
      def stream(node, source, allow_materialization: true)
        if (transform = plan(node))
          Stream.new(source) { |src| transform.call(each_element(src)) }
        elsif allow_materialization
          Stream.new(source) { |src| materialize(node, src) }
        else
          raise Errors::MaterializationError,
                'expression requires materializing the source into an ' \
                'array, but :allow_materialization is false'
        end
      end

      private

      # Returns a callable transforming an element enumerator into a
      # result enumerator, or nil when the node requires the source to
      # be materialized.
      # @api private
      def plan(node)
        case node
        when Nodes::Current
          ->(enum) { enum }
        when Nodes::ArrayProjection
          projection_plan(node)
        when Nodes::Flatten
          flatten_plan(node)
        when Nodes::Slice
          slice_plan(node)
        when Nodes::Chain
          chain_plan(node)
        end
      end

      def projection_plan(node)
        target_plan = plan(node.target)
        return nil unless target_plan

        projection = node.projection
        lambda do |enum|
          Enumerator.new do |yielder|
            target_plan.call(enum).each do |element|
              value = projection.visit(element)
              yielder << value unless value.nil?
            end
          end
        end
      end

      def flatten_plan(node)
        child_plan = plan(node.child)
        return nil unless child_plan

        lambda do |enum|
          Enumerator.new do |yielder|
            child_plan.call(enum).each do |element|
              if element.respond_to?(:to_ary)
                element.to_ary.each { |value| yielder << value }
              else
                yielder << element
              end
            end
          end
        end
      end

      def slice_plan(node)
        step = node.step || 1
        start = node.start || 0
        stop = node.stop
        return nil if step.negative? || start.negative? || (stop && stop.negative?)

        lambda do |enum|
          Enumerator.new do |yielder|
            index = 0
            enum.each do |element|
              break if stop && index >= stop

              yielder << element if index >= start && ((index - start) % step).zero?
              index += 1
            end
          end
        end
      end

      def chain_plan(node)
        plans = node.children.map { |child| plan(child) }
        return nil if plans.any?(&:nil?)

        ->(enum) { plans.reduce(enum) { |e, child_plan| child_plan.call(e) } }
      end

      # @api private
      def each_element(source)
        Enumerator.new do |yielder|
          if source.respond_to?(:next)
            loop { yielder << source.next }
          else
            source.each { |element| yielder << element }
          end
        end
      end

      def materialize(node, source)
        array = []
        each_element(source).each { |element| array << element }
        result = node.visit(array)
        if result.respond_to?(:to_ary)
          result.to_ary.each
        elsif result.nil?
          [].each
        else
          [result].each
        end
      end
    end

    # A lazy, single-pass sequence of streamed results. Includes
    # {Enumerable}; results are produced on demand as the source is
    # pulled. The source is closed exactly once, when iteration
    # completes, is abandoned early, or raises.
    class Stream
      include Enumerable

      # @api private
      def initialize(source, &plan)
        @source = source
        @plan = plan
        @closed = false
      end

      def each
        return enum_for(:each) unless block_given?

        begin
          @plan.call(@source).each { |value| yield value }
        ensure
          close
        end
      end

      # Closes the underlying source. Safe to call multiple times; the
      # source is closed at most once.
      def close
        unless @closed
          @closed = true
          @source.close if @source.respond_to?(:close)
        end
        nil
      end
    end
  end
end
