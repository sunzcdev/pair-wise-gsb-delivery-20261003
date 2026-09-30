# frozen_string_literal: true

module Dry
  module Types
    # Default types are useful when a missing value should be replaced by a default one
    #
    # @api public
    class Default
      # @api private
      class Callable < Default
        include ::Dry::Equalizer(:type, inspect: false, immutable: true)

        # Evaluates given callable. A callable accepting two arguments
        # also receives the context of the current call.
        #
        # @param [Object] context per-call context
        # @return [Object]
        def evaluate(context = Undefined)
          if @value.respond_to?(:arity) && @value.arity.equal?(2)
            value.call(type, context)
          else
            value.call(type)
          end
        end

        # @return [true]
        def callable?
          true
        end
      end

      include Type
      include Decorator
      include Builder
      include Printable
      include ::Dry::Equalizer(:type, :value, inspect: false, immutable: true)

      # @return [Object]
      attr_reader :value

      # @param [Object] context per-call context, ignored for plain values
      #
      # @return [Object] the default value
      def evaluate(_context = Undefined)
        value
      end

      # @param [Object, #call] value
      #
      # @return [Class] {Default} or {Default::Callable}
      #
      # @api private
      def self.[](value)
        if value.respond_to?(:call)
          Callable
        else
          self
        end
      end

      # @param [Type] type
      # @param [Object] value
      #
      # @api private
      def initialize(type, value, **)
        super
        @value = value
      end

      # Build a constrained type
      #
      # @param [Array] args see {Dry::Types::Builder#constrained}
      #
      # @return [Default]
      #
      # @api public
      def constrained(...) = type.constrained(...).default(value)

      # @return [true]
      #
      # @api public
      def default? = true

      # @param [Object] input
      # @param [Object] context per-call context
      #
      # @return [Result::Success]
      #
      # @api public
      def try(input = Undefined, context = Undefined, &)
        if input.equal?(Undefined)
          success(evaluate(context))
        else
          type.try(input, context)
        end
      end

      # @return [Boolean]
      #
      # @api public
      def valid?(value = Undefined) = Undefined.equal?(value) || super

      # @param [Object] input
      # @param [Object] context per-call context
      #
      # @return [Object] value passed through {#type} or {#default} value
      #
      # @api private
      def call_unsafe(input = Undefined, context = Undefined)
        if input.equal?(Undefined)
          evaluate(context)
        else
          Undefined.default(type.call_unsafe(input, context)) { evaluate(context) }
        end
      end

      # @param [Object] input
      # @param [Object] context per-call context
      #
      # @return [Object] value passed through {#type} or {#default} value
      #
      # @api private
      def call_safe(input = Undefined, context = Undefined, &)
        if input.equal?(Undefined)
          evaluate(context)
        else
          Undefined.default(type.call_safe(input, context, &)) { evaluate(context) }
        end
      end

      # @return [false]
      #
      # @api private
      def callable? = false
    end
  end
end
