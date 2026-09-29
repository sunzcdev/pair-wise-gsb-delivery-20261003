# frozen_string_literal: true

require "dry/monads/result"
require "dry/transaction/errors"

module Dry
  module Transaction
    class StepAdapters
      # Runs a group of independent validation checks against the same input.
      #
      # Every check in the group is called with the step input, even when other
      # checks fail. All check failures are collected and returned together as
      # a Failure wrapping a hash of check names to their failure values.
      #
      # Each check must return a Success wrapping a hash of named outputs. When
      # every check succeeds, these hashes are merged into a single hash. A
      # DuplicateCheckOutputError is raised if two checks declare the same
      # output key, so outputs are never silently overwritten.
      #
      # @api private
      class Validate
        include Dry::Monads[:result]

        def call(_operation, options, args)
          checks = options[:checks]

          unless checks.is_a?(Hash) && checks.any?
            raise MissingCheckListError, options[:step_name]
          end

          errors = {}
          outputs = {}

          checks.each do |check_name, check|
            publish_check_event(options, :step, check_name, args)

            result = check.(*args)

            unless result.is_a?(Dry::Monads::Result)
              raise InvalidResultError, check_name
            end

            if result.success?
              outputs[check_name] = check_output(options[:step_name], check_name, result.value!)
              publish_check_event(options, :step_succeeded, check_name, args, value: result.value!)
            else
              errors[check_name] = result.failure
              publish_check_event(options, :step_failed, check_name, args, value: result.failure)
            end
          end

          return Failure(errors) unless errors.empty?

          Success(merge_outputs(options[:step_name], outputs))
        end

        private

        def check_output(step_name, check_name, value)
          unless value.is_a?(Hash)
            raise InvalidCheckOutputError.new(step_name, check_name)
          end

          value
        end

        def merge_outputs(step_name, outputs)
          outputs.each_with_object({}) do |(check_name, output), merged|
            output.each do |key, value|
              if merged.key?(key)
                raise DuplicateCheckOutputError.new(step_name, check_name, key)
              end

              merged[key] = value
            end
          end
        end

        def publish_check_event(options, event, check_name, args, payload = {})
          publisher = options[:publisher]
          return unless publisher

          publisher.publish(
            event,
            **{step_name: check_name, group: options[:step_name], args: args}.merge(payload)
          )
        end
      end

      register :validate, Validate.new
    end
  end
end
