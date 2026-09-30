require 'bigdecimal'
require_relative './aggregate'

module Dentaku
  module AST
    class Avg < Aggregate
      def self.min_param_count
        0
      end

      private

      def fold_values(values)
        raise_too_few! if values.empty?

        values.reduce(0, :+) / BigDecimal(values.length)
      end

      def fold(memo, value)
        memo + value
      end

      def initial_value
        0
      end

      def finalize_stream(memo, count)
        raise_too_few! if count.zero?

        memo / BigDecimal(count)
      end

      def raise_too_few!
        raise Dentaku::ArgumentError.for(
          :too_few_arguments,
          function_name: 'AVG', expected: 1.., actual: 0
        )
      end

      def raise_empty_args!
        raise_too_few!
      end
    end
  end
end

Dentaku::AST::Function.register_class(:avg, Dentaku::AST::Avg)
