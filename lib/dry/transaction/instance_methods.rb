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
        assert_step_args(input)

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
          if (step_options = step_args[step.name])
            step.with(**step_call_args(step.name, step_options))
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

      # Accepts either an Array of positional arguments (legacy form) or a
      # Hash with :args and/or :kwargs keys describing positional and keyword
      # arguments separately
      def step_call_args(step_name, step_options)
        case step_options
        when Array
          {call_args: step_options}
        when Hash
          unknown_keys = step_options.keys - %i[args kwargs]

          unless unknown_keys.empty?
            raise ArgumentError,
              "invalid keys #{unknown_keys.map(&:inspect).join(", ")} in arguments for step " \
              "+#{step_name}+; use an Array of positional arguments or a Hash with " \
              ":args and/or :kwargs keys"
          end

          {call_args: Array(step_options[:args]), call_kwargs: step_options.fetch(:kwargs, {})}
        else
          raise ArgumentError,
            "invalid arguments for step +#{step_name}+; use an Array of positional arguments " \
            "or a Hash with :args and/or :kwargs keys"
        end
      end

      def assert_step_args(input)
        steps.each do |step|
          assert_step_signature(step, *step.effective_call_args(input))
        end
      end

      def assert_step_signature(step, args, kwargs)
        parameters = step.parameters

        # Operations without parameters are called without any arguments, and
        # non-lambda procs accept any arguments without raising, so there is
        # nothing to check in either case
        return if parameters.empty? || !step.strict_signature?

        num_required = parameters.count { |type, _| type == :req }
        num_optional = parameters.count { |type, _| type == :opt }
        rest = parameters.any? { |type, _| type == :rest }

        required_keys = parameters.select { |type, _| type == :keyreq }.map(&:last)
        optional_keys = parameters.select { |type, _| type == :key }.map(&:last)
        keyrest = parameters.any? { |type, _| type == :keyrest }
        accepts_keywords = keyrest || required_keys.any? || optional_keys.any?

        # When the operation accepts no keyword arguments, Ruby passes keyword
        # arguments as a single trailing positional Hash instead
        num_args_supplied = args.length
        num_args_supplied += 1 if !accepts_keywords && !kwargs.empty?

        if num_args_supplied < num_required
          raise ArgumentError, "not enough arguments supplied for step +#{step.name}+"
        end

        if !rest && num_args_supplied > num_required + num_optional
          raise ArgumentError, "too many arguments supplied for step +#{step.name}+"
        end

        return unless accepts_keywords

        missing_keys = required_keys - kwargs.keys

        unless missing_keys.empty?
          raise ArgumentError,
            "missing keyword arguments #{missing_keys.map { |key| ":#{key}" }.join(", ")} " \
            "for step +#{step.name}+"
        end

        return if keyrest

        unknown_keys = kwargs.keys - required_keys - optional_keys

        unless unknown_keys.empty?
          raise ArgumentError,
            "unknown keyword arguments #{unknown_keys.map { |key| ":#{key}" }.join(", ")} " \
            "for step +#{step.name}+"
        end
      end
    end
  end
end
