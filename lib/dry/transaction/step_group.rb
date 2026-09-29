# frozen_string_literal: true

require "dry/transaction/errors"
require "dry/transaction/step"

module Dry
  module Transaction
    # A group of independent checks sharing the same input.
    #
    # Every check in the group runs, even when others fail, and all their
    # failure values are collected into a single `Failure` wrapping an array.
    # Only when every check succeeds are their declared outputs (hashes
    # returned via `Success`) merged and passed on as the group's output.
    # Merging duplicate output keys raises {DuplicateOutputKeyError}.
    #
    # Each check publishes its own step events under its own name, while the
    # group publishes separate events under the group's step name.
    #
    # @api private
    class StepGroup < Step
      register_event(:step)
      register_event(:step_succeeded)
      register_event(:step_failed)

      attr_reader :steps

      # rubocop:disable Metrics/ParameterLists
      def initialize(adapter:, name:, operation_name:, options:, operation: nil, call_args: [], steps: [])
        super(
          adapter: adapter,
          name: name,
          operation_name: operation_name,
          options: options,
          operation: operation,
          call_args: call_args
        )
        @steps = steps
      end
      # rubocop:enable Metrics/ParameterLists

      def with(operation: UNDEFINED, call_args: UNDEFINED)
        return self if operation == UNDEFINED && call_args == UNDEFINED

        new_call_args = call_args == UNDEFINED ? self.call_args : Array(call_args)
        new_steps = steps

        unless operation == UNDEFINED
          operations = Array(operation)

          if operations.size != steps.size
            raise ArgumentError,
              "check group +#{name}+ expects #{steps.size} operations, got #{operations.size}"
          end

          new_steps = steps.zip(operations).map { |step, op| step.with(operation: op) }
        end

        unless call_args == UNDEFINED
          new_steps = new_steps.map { |step| step.with(call_args: new_call_args) }
        end

        self.class.new(
          adapter: adapter,
          name: name,
          operation_name: operation_name,
          operation: nil,
          options: adapter.options,
          call_args: new_call_args,
          steps: new_steps
        )
      end

      def call(input, continue = RETURN)
        args = [input, *call_args]

        continue.(with_broadcast(args) { run_checks(input) })
      end

      def subscribe(listener)
        super
        steps.each { |step| step.subscribe(listener) }
      end

      def substeps
        steps
      end

      def arity
        steps.map(&:arity).max || 1
      end

      private

      def run_checks(input)
        failures = []
        outputs = {}

        steps.each do |step|
          result = step.(input)

          if result.success?
            merge_outputs(outputs, result.value!)
          else
            failures << failure_value(result.failure)
          end
        end

        failures.empty? ? Success(outputs) : Failure(failures)
      end

      def merge_outputs(outputs, value)
        return unless value.is_a?(Hash)

        value.each do |key, output|
          raise DuplicateOutputKeyError.new(name, key) if outputs.key?(key)

          outputs[key] = output
        end
      end

      def failure_value(failure)
        failure.is_a?(StepFailure) ? failure.value : failure
      end
    end
  end
end
