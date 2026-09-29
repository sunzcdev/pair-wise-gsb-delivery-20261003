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

      # Returns a copy of the failure with the given step names prepended to
      # the path of steps leading to the failure (e.g. the enclosing branch
      # step and branch names for failures raised inside a branch).
      #
      # @api private
      def prepend_path(*names)
        self.class.new(step, value, path: names + path)
      end
    end
  end
end
