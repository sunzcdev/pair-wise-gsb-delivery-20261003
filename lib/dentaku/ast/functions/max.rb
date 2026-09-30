require_relative './aggregate'

module Dentaku
  module AST
    class Max < Aggregate
      private

      def fold_values(values)
        values.max
      end

      def fold(memo, value)
        memo.nil? ? value : [memo, value].max
      end
    end
  end
end

Dentaku::AST::Function.register_class(:max, Dentaku::AST::Max)
