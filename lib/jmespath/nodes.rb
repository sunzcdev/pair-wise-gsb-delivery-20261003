# frozen_string_literal: true
module JMESPath
  # @api private
  module Nodes
    class Node
      def visit(value)
      end

      def optimize
        self
      end

      def chains_with?(_other)
        false
      end

      # Whether this node can be evaluated incrementally over a
      # streaming array source, producing results as elements are read.
      # Nodes that require the full input up front (e.g. sort, negative
      # indices) return `false` and must be materialized before
      # evaluation.
      # @api private
      def streamable?
        false
      end

      # Whether a streaming node emits the elements of an array (`true`)
      # or a single scalar result (`false`). Only meaningful when
      # {#streamable?} returns `true`.
      # @api private
      def streams_array?
        false
      end
    end

    require  'jmespath/nodes/subexpression'
    require  'jmespath/nodes/and'
    require  'jmespath/nodes/comparator'
    require  'jmespath/nodes/comparator'
    require  'jmespath/nodes/condition'
    require  'jmespath/nodes/current'
    require  'jmespath/nodes/expression'
    require  'jmespath/nodes/field'
    require  'jmespath/nodes/flatten'
    require  'jmespath/nodes/function'
    require  'jmespath/nodes/index'
    require  'jmespath/nodes/literal'
    require  'jmespath/nodes/multi_select_hash'
    require  'jmespath/nodes/multi_select_list'
    require  'jmespath/nodes/not'
    require  'jmespath/nodes/or'
    require  'jmespath/nodes/pipe'
    require  'jmespath/nodes/projection'
    require  'jmespath/nodes/projection'
    require  'jmespath/nodes/projection'
    require  'jmespath/nodes/slice'
  end
end
