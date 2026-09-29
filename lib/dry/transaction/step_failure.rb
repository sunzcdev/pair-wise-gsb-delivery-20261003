# frozen_string_literal: true

module Dry
  module Transaction
    # A wrapper for storing together the step that failed
    # and value describing the failure.
    #
    # When transactions are nested (a transaction used as another
    # transaction's operation), the wrapper also carries a structured
    # failure address in +path+: the ordered list of step names from the
    # outermost parent step down to the leaf step that raised the failure.
    # The original failure +value+ (including any original exception) is
    # preserved unchanged across all levels of nesting.
    class StepFailure
      attr_reader :step
      attr_reader :value
      attr_reader :path

      # @api private
      def self.call(step, value)
        yield

        # rubocop:disable Style/CaseEquality
        if self === value
          value.prepend(step)
        else
          new(step, value)
        end
        # rubocop:enable Style/CaseEquality
      end

      def initialize(step, value, path: [step.name])
        @step = step
        @value = value
        @path = path
      end

      # Returns a copy of this failure with the given parent step prepended
      # to the failure address, preserving the leaf step and original value.
      #
      # @api private
      def prepend(parent_step)
        self.class.new(step, value, path: [parent_step.name, *path])
      end

      # Returns true when the failure matches the given address.
      #
      # An Array address matches the full failure path, or any parent-level
      # prefix of it, so +[:create_user]+ matches a failure addressed
      # +[:create_user, :validate]+. Any other address is compared against
      # the leaf step name only, preserving the original single-level
      # matching behaviour.
      def matches?(address)
        case address
        when Array
          path.first(address.length) == address
        else
          step.name == address
        end
      end
    end
  end
end
