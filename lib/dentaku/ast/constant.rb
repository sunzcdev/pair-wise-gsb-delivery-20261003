require_relative './node'

module Dentaku
  module AST
    # A value computed during partial evaluation. Carries an ordinary Ruby
    # value (including nil) so it is never confused with an unbound
    # identifier or an evaluation error.
    class Constant < Node
      def initialize(value)
        @value = value
      end

      def value(*)
        @value
      end

      def type
        case @value
        when ::Numeric then :numeric
        when ::String  then :string
        when true, false then :logical
        when ::Array   then :array
        end
      end

      def dependencies(*)
        []
      end

      def accept(visitor)
        visitor.visit_constant(self)
      end
    end
  end
end
