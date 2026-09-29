# frozen_string_literal: true

module Dry
  module Transaction
    # A wrapper for storing together the step that failed
    # and value describing the failure.
    class StepFailure
      attr_reader :step
      attr_reader :value
      attr_reader :path

      # @api private
      def self.call(step, value)
        # rubocop:disable Style/CaseEquality
        if self === value
          value
        else
          yield
          new(step, value)
        end
        # rubocop:enable Style/CaseEquality
      end

      def initialize(step, value, path: [step.name])
        @step = step
        @value = value
        @path = path
      end

      # Returns a copy of the failure with the given step name prepended to
      # the path, preserving the full branch path of a nested failure.
      #
      # @api private
      def through(step_name)
        self.class.new(step, value, path: [step_name, *path])
      end
    end
  end
end
