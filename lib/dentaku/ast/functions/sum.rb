require_relative './aggregate'

module Dentaku
  module AST
    class Sum < Aggregate
      def self.min_param_count
        0
      end

      private

      def fold_values(values)
        values.reduce(0, :+)
      end

      def fold(memo, value)
        memo + value
      end

      def initial_value
        0
      end

      def raise_empty_args!
        raise Dentaku::ArgumentError.for(
          :too_few_arguments,
          function_name: 'SUM', expected: 1.., actual: 0
        )
      end
    end
  end
end

Dentaku::AST::Function.register_class(:sum, Dentaku::AST::Sum)
