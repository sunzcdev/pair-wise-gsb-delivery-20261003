# frozen_string_literal: true

require "dry/transaction/step_failure"
require "dry/transaction/step_adapter"

module Dry
  module Transaction
    # @api private
    class Step
      UNDEFINED = Object.new.freeze
      RETURN = -> x { x }

      include Dry::Events::Publisher[name || object_id]
      include Dry::Monads[:result]

      register_event(:step)
      register_event(:step_succeeded)
      register_event(:step_failed)

      attr_reader :adapter
      attr_reader :name
      attr_reader :operation_name
      attr_reader :call_args
      attr_reader :call_kwargs

      # rubocop:disable Metrics/ParameterLists
      def initialize(adapter:, name:, operation_name:, options:, operation: nil, call_args: [], call_kwargs: {})
        @adapter = StepAdapter[adapter, operation,
          {**options, step_name: name, operation_name: operation_name}]
        @name = name
        @operation_name = operation_name
        @call_args = call_args
        @call_kwargs = call_kwargs
      end
      # rubocop:enable Metrics/ParameterLists

      def with(operation: UNDEFINED, call_args: UNDEFINED, call_kwargs: UNDEFINED)
        return self if operation == UNDEFINED && call_args == UNDEFINED && call_kwargs == UNDEFINED

        new_operation = operation == UNDEFINED ? adapter.operation : operation
        new_call_args = call_args == UNDEFINED ? self.call_args : Array(call_args)
        new_call_kwargs = call_kwargs == UNDEFINED ? self.call_kwargs : call_kwargs

        self.class.new(
          adapter: adapter,
          name: name,
          operation_name: operation_name,
          operation: new_operation,
          options: adapter.options,
          call_args: new_call_args,
          call_kwargs: new_call_kwargs
        )
      end

      def call(input, continue = RETURN)
        args = [input, *call_args]
        kwargs = call_kwargs

        if adapter.yields?
          with_broadcast(args, kwargs) { adapter.(args, kwargs, &continue) }
        else
          continue.(with_broadcast(args, kwargs) { adapter.(args, kwargs) })
        end
      end

      def with_broadcast(args, kwargs)
        publish(:step, step_name: name, args: args, kwargs: kwargs)

        yield.fmap { |value|
          publish(:step_succeeded, step_name: name, args: args, kwargs: kwargs, value: value)
          value
        }.or { |value|
          Failure(
            StepFailure.(self, value) {
              publish(:step_failed, step_name: name, args: args, kwargs: kwargs, value: value)
            }
          )
        }
      end

      def internal?
        !external?
      end

      def external?
        !!operation_name
      end

      def arity
        adapter.operation.arity
      end

      def parameters
        adapter.operation.parameters
      end

      def operation
        adapter.operation
      end
    end
  end
end
