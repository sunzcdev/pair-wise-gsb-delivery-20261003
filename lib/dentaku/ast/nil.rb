module Dentaku
  module AST
    class Nil < Node
      def evaluate(*)
        nil
      end

      def accept(visitor)
        visitor.visit_nil(self)
      end
    end
  end
end
