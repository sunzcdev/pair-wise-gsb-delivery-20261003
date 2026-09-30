require_relative "./node"

module Dentaku
  module AST
    class Array < Node
      def self.arity
      end

      def self.min_param_count
        0
      end

      def self.max_param_count
        Float::INFINITY
      end

      def initialize(*elements)
        @elements = *elements
      end

      def value(context = {})
        @elements.map { |el| el.value(context) }
      end

      def dependencies(context = {})
        @elements.flat_map { |el| el.dependencies(context) }
      end

      def type
        nil
      end

      def accept(visitor)
        visitor.visit_array(self)
      end

      def partial(evaluator, guarded)
        reduced_elements = @elements.map { |element| evaluator.reduce(element, guarded: guarded) }

        if reduced_elements.all? { |element| evaluator.constant?(element) }
          rebuilt = evaluator.rebuild(self.class, *reduced_elements)
          return self if rebuilt.nil?

          return evaluator.constant_for(rebuilt.value)
        end

        return self if reduced_elements.each_with_index.all? { |element, i| element.equal?(@elements[i]) }

        evaluator.rebuild(self.class, *reduced_elements) || self
      end

      private

      def compute_pure?
        @elements.all?(&:pure?)
      end
    end
  end
end
