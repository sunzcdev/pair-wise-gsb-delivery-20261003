module Dentaku
  module AST
    class CaseElse < Node
      attr_reader :node

      def initialize(node)
        @node = node
      end

      def value(context = {})
        @node.value(context)
      end

      def dependencies(context = {})
        @node.dependencies(context)
      end

      def self.arity
        1
      end

      def self.min_param_count
        1
      end

      def self.max_param_count
        1
      end

      def accept(visitor)
        visitor.visit_else(self)
      end

      def partial(evaluator, guarded)
        reduced = evaluator.reduce(@node, guarded: true)
        reduced.equal?(@node) ? self : (evaluator.rebuild(self.class, reduced) || self)
      end

      private

      def compute_pure?
        @node.pure?
      end
    end
  end
end
