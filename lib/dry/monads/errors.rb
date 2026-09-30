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

    # Raised when a tail_rec step produces something
    # that is not a Continue/Done control marker.
    class InvalidTailRecStepError < ::TypeError
      def initialize(step)
        super(
          "tail_rec steps must return Continue(state) to keep looping " \
          "or Done(value) to stop, got: #{step.inspect}"
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
