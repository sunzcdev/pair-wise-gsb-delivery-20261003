# frozen_string_literal: true

module Dry
  module Transaction
    # A wrapper for storing together the step that failed
    # and value describing the failure.
    #
    # When a transaction is used as an operation of another
    # transaction, failures are chained: each wrapping StepFailure
    # keeps the inner one as its #cause, so the full #address of the
    # failure (from the outermost step down to the step that actually
    # failed) is preserved alongside the original failure value.
    class StepFailure
      attr_reader :step
      attr_reader :value
      attr_reader :cause

      # @api private
      def self.call(step, value, cause: nil)
        # rubocop:disable Style/CaseEquality
        if self === value
          yield
          new(step, value.value, cause: value)
        else
          yield
          new(step, value, cause: cause)
        end
        # rubocop:enable Style/CaseEquality
      end

      def initialize(step, value, cause: nil)
        @step = step
        @value = value
        @cause = cause
      end

      # The structured address of the failure: the names of the steps
      # from the outermost step down to the one that actually failed.
      #
      # @return [Array<Symbol>]
      def address
        [step.name, *cause&.address]
      end

      # The innermost failure in the chain: the one raised by the step
      # that actually failed.
      #
      # @return [StepFailure]
      def leaf
        cause ? cause.leaf : self
      end
    end
  end
end
