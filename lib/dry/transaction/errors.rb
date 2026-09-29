# frozen_string_literal: true

module Dry
  module Transaction
    class InvalidStepError < ArgumentError
      def initialize(step_name)
        super("step +`#{step_name}`+ must respond to `#call`")
      end
    end

    class MissingStepError < ArgumentError
      def initialize(step_name)
        super("Definition for step +`#{step_name}`+ is missing")
      end
    end

    class InvalidResultError < ArgumentError
      def initialize(step_name)
        super("step +#{step_name}+ must return a Result object")
      end
    end

    class MissingCatchListError < ArgumentError
      def initialize(step_name)
        super("step +#{step_name}+ requires one or more exception classes provided via +catch:+")
      end
    end

    # Raised when one or more scoped resources fail to release.
    #
    # The main outcome of the step stack is preserved separately from the
    # release failures: #result holds the stack's Result when the steps
    # completed, and #cause holds the original exception when the steps
    # raised one.
    class CleanupError < StandardError
      # The release failures, in the order the releases ran (reverse order
      # of acquisition).
      attr_reader :errors

      # The step stack's Result, or nil when the stack raised an exception.
      attr_reader :result

      def initialize(errors, result: nil)
        @errors = errors
        @result = result

        super(
          "failed to release #{errors.length} scoped " \
          "resource#{"s" unless errors.length == 1}: " \
          "#{errors.map(&:message).join(", ")}"
        )
      end
    end
  end
end
