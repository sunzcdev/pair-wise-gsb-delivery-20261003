require_relative './node'

module Dentaku
  module AST
    class Operation < Node
      attr_reader :left, :right

      def self.min_param_count
        arity
      end

      def self.max_param_count
        arity
      end

      def initialize(left, right)
        @left  = left
        @right = right
      end

      def dependencies(context = {})
        (left.dependencies(context) + right.dependencies(context)).uniq
      end

      def self.right_associative?
        false
      end

      def accept(visitor)
        visitor.visit_operation(self)
      end

      def partial(evaluator, guarded)
        reduced_left = evaluator.reduce(left, guarded: guarded)
        reduced_right = evaluator.reduce(right, guarded: guarded)

        if evaluator.constant?(reduced_left) && evaluator.constant?(reduced_right)
          rebuilt = evaluator.rebuild(self.class, reduced_left, reduced_right)
          return self if rebuilt.nil?

          # arithmetic on already-folded constants is a local,
          # side-effect-free computation, even inside a guarded subtree
          value = evaluator.evaluate_node(rebuilt)
          return rebuilt if evaluator.error_result?(value)

          return evaluator.constant_for(value)
        end

        return self if reduced_left.equal?(left) && reduced_right.equal?(right)

        evaluator.rebuild(self.class, reduced_left, reduced_right) || self
      end

      def display_operator
        operator.to_s
      end

      alias_method :to_s, :display_operator

      def operator_spacing
        " "
      end

      private

      # compact: unary operations (e.g. Percentage) only populate left
      def compute_pure?
        [left, right].compact.all?(&:pure?)
      end
    end
  end
end
