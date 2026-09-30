require_relative "../exceptions"
require_relative "./node"

module Dentaku
  module AST
    class Access < Node
      attr_reader :structure, :index

      def self.arity
        2
      end

      def self.min_param_count
        arity
      end

      def self.max_param_count
        arity
      end

      def initialize(data_structure, index)
        @structure = data_structure
        @index = index
      end

      def value(context = {})
        structure = @structure.value(context)
        index = @index.value(context)

        unless structure.respond_to?(:[]) && !structure.is_a?(::Numeric)
          raise Dentaku::ArgumentError.for(:incompatible_type, actual: structure),
                "#{self.class} requires an indexable structure, but got #{structure.class}"
        end

        begin
          structure[index]
        rescue ::TypeError => e
          raise Dentaku::ArgumentError.for(:incompatible_type, actual: index), e.message
        end
      end

      def dependencies(context = {})
        @structure.dependencies(context) + @index.dependencies(context)
      end

      def type
        nil
      end

      def accept(visitor)
        visitor.visit_access(self)
      end

      def partial(evaluator, guarded)
        reduced_structure = evaluator.reduce(@structure, guarded: guarded)
        reduced_index = evaluator.reduce(@index, guarded: guarded)

        if evaluator.constant?(reduced_structure) && evaluator.constant?(reduced_index)
          rebuilt = evaluator.rebuild(self.class, reduced_structure, reduced_index)
          return self if rebuilt.nil?
          value = evaluator.evaluate_node(rebuilt)
          return rebuilt if evaluator.error_result?(value)

          return evaluator.constant_for(value)
        end

        return self if reduced_structure.equal?(@structure) && reduced_index.equal?(@index)

        evaluator.rebuild(self.class, reduced_structure, reduced_index) || self
      end

      private

      def compute_pure?
        @structure.pure? && @index.pure?
      end
    end
  end
end
