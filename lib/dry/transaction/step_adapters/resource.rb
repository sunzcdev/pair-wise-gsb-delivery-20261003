# frozen_string_literal: true

require "dry/monads/result"
require "dry/transaction/errors"

module Dry
  module Transaction
    class StepAdapters
      # Provides scoped resource management for a transaction.
      #
      # The operation acquires a resource and returns a handle responding to
      # #release. The handle is handed to the remaining steps, and released
      # when they finish, no matter how they exit (Success, Failure, an
      # exception, or an around step short-circuiting the rest of the stack).
      # Nested resource steps release their handles in reverse order of
      # acquisition.
      #
      # When a release fails, remaining releases still run, and a
      # ResourceReleaseError is raised carrying the main result and all
      # release errors separately.
      #
      # @api private
      class Resource
        include Dry::Monads[:result]

        def call(operation, options, args, &block)
          handle = operation.(*args)
          result = nil

          begin
            result = block.(Success(handle))
          ensure
            release(handle, result, options[:step_name], $!)
          end

          unless result.is_a?(Dry::Monads::Result)
            raise InvalidResultError, options[:step_name]
          end

          result
        end

        private

        def release(handle, result, step_name, in_flight)
          handle.release
        rescue StandardError => release_error
          if in_flight.is_a?(ResourceReleaseError)
            # An inner resource already failed to release: aggregate so all
            # release errors and the main result stay available at the top.
            raise ResourceReleaseError.new(
              step_name,
              in_flight.result,
              [*in_flight.release_errors, release_error]
            )
          else
            raise ResourceReleaseError.new(step_name, result, [release_error])
          end
        end
      end

      register :resource, Resource.new
    end
  end
end
