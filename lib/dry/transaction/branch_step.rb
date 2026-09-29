# frozen_string_literal: true

require "dry/transaction/branch"
require "dry/transaction/callable"
require "dry/transaction/errors"
require "dry/transaction/step_failure"

module Dry
  module Transaction
    # A declarative branching node in a transaction. A pure selector
    # operation maps the input to the name of one of the defined branches,
    # whose sub-steps are then run as their own step stack. On success,
    # execution joins the step named by the branch's explicit join mapping
    # (or simply continues with the next step when no mapping is given).
    #
    # @api private
    class BranchStep
      UNDEFINED = Object.new.freeze
      RETURN = -> x { x }

      include Dry::Events::Publisher[name || object_id]
      include Dry::Monads[:result]

      register_event(:step)
      register_event(:step_succeeded)
      register_event(:step_failed)

      attr_reader :name
      attr_reader :selector_name
      attr_reader :branches
      attr_reader :selector

      def initialize(name:, selector_name:, branches:, selector: nil)
        @name = name
        @selector_name = selector_name
        @branches = branches
        @selector = Callable[selector]
      end

      def with(operation: UNDEFINED, branches: UNDEFINED, call_args: UNDEFINED)
        return self if operation == UNDEFINED && branches == UNDEFINED

        self.class.new(
          name: name,
          selector_name: selector_name,
          selector: operation == UNDEFINED ? selector : operation,
          branches: branches == UNDEFINED ? self.branches : branches
        )
      end

      def call(input, continue = RETURN, entrypoints = {})
        branch = select_branch(input)

        publish(:step, step_name: name, branch: branch.name, args: [input])

        result = branch.stack.(Success(input)).fmap { |value|
          publish(:step_succeeded, step_name: name, branch: branch.name, args: [input], value: value)
          value
        }.or { |step_failure|
          step_failure = step_failure.prepend_path(name, branch.name)
          publish(:step_failed, step_name: name, branch: branch.name, args: [input],
                  value: step_failure.value)
          Failure(step_failure)
        }

        if result.success?
          continuation(branch, continue, entrypoints).(result)
        else
          result
        end
      end

      def sub_steps
        branches.flat_map { |branch|
          branch.steps.flat_map { |step| [step, *step.sub_steps] }
        }
      end

      def operation_name
        selector_name
      end

      def internal?
        !external?
      end

      def external?
        !!selector_name
      end

      def arity
        selector.arity
      end

      def call_args
        []
      end

      private

      def select_branch(input)
        selected = selector.(input)
        key = selected.respond_to?(:to_sym) ? selected.to_sym : selected

        branches.detect { |branch| branch.name == key } ||
          raise(UnknownBranchError.new(name, selected))
      end

      def continuation(branch, continue, entrypoints)
        return continue unless branch.join_to

        entrypoints.fetch(branch.join_to)
      end
    end
  end
end
