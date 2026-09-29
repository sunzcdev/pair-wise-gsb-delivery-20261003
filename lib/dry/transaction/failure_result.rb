# frozen_string_literal: true

require "dry/monads/result"

module Dry
  module Transaction
    # The Failure result returned by Transaction#call.
    #
    # Behaves like a regular Dry::Monads::Result::Failure (#failure is
    # the original failure value), but also carries the StepFailure
    # describing where the failure occurred, including its structured
    # address across nested transactions.
    class FailureResult < Dry::Monads::Result::Failure
      # @return [StepFailure]
      attr_reader :step_failure

      # @api private
      def initialize(step_failure)
        @step_failure = step_failure
        super(step_failure.value)
      end

      # The structured address of the failure: the names of the steps
      # from the outermost step down to the one that actually failed.
      #
      # @return [Array<Symbol>]
      def address
        step_failure.address
      end

      # Failure results compare equal to any other Failure wrapping the
      # same value, regardless of the step failure metadata they carry.
      def ==(other)
        other.is_a?(Dry::Monads::Result::Failure) && failure == other.failure
      end
    end
  end
end
