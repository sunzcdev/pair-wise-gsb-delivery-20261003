# frozen_string_literal: true

require "dry/transaction/errors"
require "dry/transaction/scoped_resource"

module Dry
  module Transaction
    class StepAdapters
      # Acquires a scoped resource and hands its handle to the rest of the
      # step stack, releasing the resource when the scope exits, no matter
      # how it exits: Success, Failure, a raised exception, or an around
      # step short-circuiting the remaining steps.
      #
      # Nested scope steps release their resources in reverse order of
      # acquisition, since each scope wraps the steps that follow it.
      #
      # Release failures never replace the main outcome and never prevent
      # the remaining releases from running. If any release fails, a
      # {Dry::Transaction::CleanupError} is raised once the scope has fully
      # exited, carrying the release failures (#errors) as well as the main
      # outcome: the step stack's Result (#result), or the raised exception
      # (#cause).
      #
      # Operations return a {Dry::Transaction::ScopedResource} (or a bare
      # handle, released via +close+). They may instead return a
      # Success(resource) or Failure(error) Result to short-circuit the
      # remaining steps when the resource cannot be acquired.
      #
      # @api private
      class Scope
        include Dry::Monads[:result]

        def call(operation, options, args, &block)
          acquired = operation.(*args)

          if acquired.is_a?(Dry::Monads::Result)
            return acquired if acquired.failure?

            acquired = acquired.value!
          end

          resource = ScopedResource[acquired]

          result = nil
          flow_error = nil
          cleanup_errors = []

          begin
            result = block.(Success(resource.handle))
          rescue CleanupError => cleanup
            # An inner scope already aggregated release failures. Merge them
            # with our own (released below) and preserve the main outcome.
            cleanup_errors = cleanup.errors
            result = cleanup.result
            flow_error = cleanup.cause
          rescue Exception => error # rubocop:disable Lint/RescueException
            flow_error = error
          ensure
            begin
              resource.release.()
            rescue StandardError => release_error
              cleanup_errors << release_error
            end
          end

          unless cleanup_errors.empty?
            raise CleanupError.new(cleanup_errors, result: result), cause: flow_error
          end

          raise flow_error if flow_error

          result
        end
      end

      register :scope, Scope.new
    end
  end
end
