require_relative "./node"

module Dentaku
  module AST
    class Grouping < Node
      attr_reader :node

      def initialize(node)
        @node = node
      end

      def value(context = {})
        @node.value(context)
      end

      def type
        @node.type
      end

      def dependencies(context = {})
        @node.dependencies(context)
      end

      def accept(visitor)
        visitor.visit_grouping(self)
      end

      private

      def compute_pure?
        @node.pure?
      end
    end
  end
end
