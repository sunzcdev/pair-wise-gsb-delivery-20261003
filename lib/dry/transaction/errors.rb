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

    # Raised when releasing a scoped resource (see StepAdapters::Resource)
    # fails. The main result of the remaining steps and the release errors
    # are preserved separately so neither is lost.
    class ResourceReleaseError < StandardError
      # The result returned by the remaining steps (nil when they raised)
      attr_reader :result

      # Release errors in the order the releases were attempted
      attr_reader :release_errors

      def initialize(step_name, result, release_errors)
        @result = result
        @release_errors = release_errors

        super(
          "step +#{step_name}+ failed to release its resource: " \
          "#{release_errors.map(&:message).join(", ")}"
        )
      end
    end
  end
end
