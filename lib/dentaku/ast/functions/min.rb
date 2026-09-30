require_relative './aggregate'

module Dentaku
  module AST
    class Min < Aggregate
      private

      def fold_values(values)
        values.min
      end

      def fold(memo, value)
        memo.nil? ? value : [memo, value].min
      end
    end
  end
end

Dentaku::AST::Function.register_class(:min, Dentaku::AST::Min)
