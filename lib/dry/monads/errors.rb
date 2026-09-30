# frozen_string_literal: true

module Dry
  module Monads
    # An unsuccessful result of extracting a value from a monad.
    class UnwrapError < ::StandardError
      attr_reader :receiver

      def initialize(receiver)
        @receiver = receiver
        super("value! was called on #{receiver.inspect}")
      end
    end

    # An error thrown on returning a Failure of unknown type.
    class InvalidFailureTypeError < ::StandardError
      def initialize(failure)
        super("Cannot create Failure from #{failure.inspect}, it doesn't meet the constraints")
      end
    end

    # Raised when two Validated values carrying different error merge rules
    # are combined (e.g. via Validated#apply or List#traverse)
    class MergeRuleMismatchError < ::StandardError
      def initialize(left, right)
        super(
          "Cannot combine Validated results with different error merge rules: " \
          "#{left.inspect} vs #{right.inspect}"
        )
      end
    end

    # Improper use of None
    class ConstructorNotAppliedError < ::NoMethodError
      def initialize(method_name, constructor_name)
        super(
          "For calling .#{method_name} on #{constructor_name}() build a value " \
          "by appending parens: #{constructor_name}()"
        )
      end
    end
  end
end
