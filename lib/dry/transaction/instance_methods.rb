# frozen_string_literal: true

require "dry/transaction/result_matcher"
require "dry/transaction/stack"

module Dry
  module Transaction
    module InstanceMethods
      include Dry::Monads[:result]

      attr_reader :steps
      attr_reader :operations
      attr_reader :listeners
      attr_reader :stack

      def initialize(steps: self.class.steps, listeners: nil, **operations)
        @steps = steps.map { |step|
          operation = resolve_operation(step, **operations)
          step.with(operation: operation)
        }
        @operations = operations
        @stack = Stack.new(@steps)
        subscribe(listeners) unless listeners.nil?
      end

      def call(input = nil, &block)
        assert_step_args

        result = stack.(Success(input))

        if block
          ResultMatcher.(result, &block)
        else
          result.or { |step_failure|
            # Unwrap the value from the StepFailure and return it directly
            Failure(step_failure.value)
          }
        end
      end

      def subscribe(listeners)
        @listeners = listeners

        if listeners.is_a?(Hash)
          listeners.each do |step_name, listener|
            steps.detect { |step| step.name == step_name }.subscribe(listener)
          end
        else
          steps.each do |step|
            step.subscribe(listeners)
          end
        end
      end

      def with_step_args(**step_args)
        assert_valid_step_args(step_args)

        new_steps = steps.map { |step|
          if step_args.key?(step.name)
            call_args, call_kwargs = extract_step_args(step.name, step_args[step.name])
            step.with(call_args: call_args, call_kwargs: call_kwargs)
          else
            step
          end
        }

        self.class.new(steps: new_steps, listeners: listeners, **operations)
      end

      private

      def respond_to_missing?(name, _include_private = false)
        steps.any? { |step| step.name == name }
      end

      def method_missing(name, *args, &block)
        step = steps.detect { |s| s.name == name }
        super unless step

        operation = operations[step.name]
        unless operation
          raise NotImplementedError,
            "no operation +#{step.operation_name}+ defined for step +#{step.name}+"
        end

        operation.(*args, &block)
      end

      def resolve_operation(step, **operations)
        if step.internal? && operations[step.name]
          operations[step.name]
        elsif methods.include?(step.name) || private_methods.include?(step.name)
          method(step.name)
        elsif operations[step.name].respond_to?(:call)
          operations[step.name]
        elsif operations[step.name]
          raise InvalidStepError, step.name
        else
          raise MissingStepError, step.name
        end
      end

      def assert_valid_step_args(step_args)
        step_args.each_key do |step_name|
          unless steps.any? { |step| step.name == step_name }
            raise ArgumentError, "+#{step_name}+ is not a valid step name"
          end
        end
      end

      # Extracts positional and keyword arguments from a `with_step_args` value:
      #
      # - an Array is a list of positional arguments
      # - a Hash is a set of keyword arguments
      # - a two-element `[Array, Hash]` pair provides both positional and
      #   keyword arguments
      def extract_step_args(step_name, value)
        case value
        when Hash
          [[], value]
        when Array
          if value.size == 2 && value[0].is_a?(Array) && value[1].is_a?(Hash)
            value
          else
            [value, {}]
          end
        else
          raise ArgumentError,
            "step arguments for +#{step_name}+ must be an Array of positional " \
            "arguments or a Hash of keyword arguments"
        end
      end

      # Checks every step's supplied arguments against its operation's actual
      # signature before any step is run, so argument errors are raised up
      # front rather than partway through the transaction.
      def assert_step_args
        steps.each do |step|
          parameters = step.parameters

          num_args_required = parameters.count { |type, _| type == :req }
          num_args_supplied = step.call_args.length + 1 # add 1 for main `input`

          if num_args_required > num_args_supplied
            raise ArgumentError, "not enough arguments supplied for step +#{step.name}+"
          end

          assert_step_kwargs(step, parameters)
        end
      end

      def assert_step_kwargs(step, parameters)
        keyreq = parameters.select { |type, _| type == :keyreq }.map(&:last)
        missing = keyreq - step.call_kwargs.keys

        unless missing.empty?
          raise ArgumentError,
            "missing keyword arguments #{format_kwargs(missing)} for step +#{step.name}+"
        end

        return if step.call_kwargs.empty?
        return if parameters.any? { |type, _| type == :keyrest }

        accepted = parameters.select { |type, _| type == :key || type == :keyreq }.map(&:last)
        unknown = step.call_kwargs.keys - accepted

        unless unknown.empty?
          raise ArgumentError,
            "unknown keyword arguments #{format_kwargs(unknown)} for step +#{step.name}+"
        end
      end

      def format_kwargs(names)
        names.map { |name| ":#{name}" }.join(", ")
      end
    end
  end
end
