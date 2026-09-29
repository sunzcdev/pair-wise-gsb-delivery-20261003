# frozen_string_literal: true

require "dry/monads/result"
require "dry/transaction/errors"
require "dry/transaction/stack"
require "dry/transaction/step_failure"

module Dry
  module Transaction
    class StepAdapters
      # Runs the named sub-step stack selected by the step's (pure) selector
      # operation, then joins the branch's result back into the main step
      # flow via the branch's explicit `join` mapping.
      #
      # @api private
      class Branch
        include Dry::Monads[:result]

        def call(operation, options, args)
          branches = options.fetch(:branches)
          branch_name = operation.(*args)
          definition = branches[branch_name]

          unless definition
            raise UnknownBranchError.new(options[:step_name], branch_name)
          end

          result = Stack.new(definition.steps).(Success(args[0]))

          result
            .fmap { |value| definition.join.(value) }
            .or { |failure|
              if failure.is_a?(StepFailure)
                Failure(failure.through(options[:step_name]))
              else
                Failure(failure)
              end
            }
        end
      end

      register :branch, Branch.new
    end
  end
end
