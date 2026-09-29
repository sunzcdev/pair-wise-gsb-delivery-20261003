module Dentaku
  module AST
    class Literal < Node
      attr_reader :type

      def initialize(token)
        @token = token
        @value = token.value
        @type  = token.category
        @location = token.location
      end

      def evaluate(*)
        @value
      end

      def dependencies(*)
        []
      end

      def accept(visitor)
        visitor.visit_literal(self)
      end

      def quoted
        @token.raw_value || value.to_s
      end
      alias_method :to_s, :quoted
    end
  end
end
