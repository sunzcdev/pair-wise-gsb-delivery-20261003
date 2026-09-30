# frozen_string_literal: true

module Dry
  module Types
    # Constructor types apply a function to the input that is supposed to return
    # a new value. Coercion is a common use case for constructor types.
    #
    # @api public
    class Constructor < Nominal
      include ::Dry::Equalizer(:type, :options, inspect: false, immutable: true)

      # @return [#call]
      attr_reader :fn

      # @return [Type]
      attr_reader :type

      undef :constrained?, :meta, :optional?, :primitive, :primitive?, :default?, :name, :namespace

      # @param [Builder, Object] input
      # @param [Hash] options
      # @param [#call, nil] block
      # @option options [Boolean] :context Whether the constructor block
      #   explicitly receives the context of a call
      #
      # @api public
      def self.new(input, fn: Undefined, context: false, **options, &block)
        type = input.is_a?(Builder) ? input : Nominal.new(input)
        function = Undefined.default(fn, block)

        function =
          if context && !function.is_a?(Function)
            Function[function, contextual: true]
          elsif function.is_a?(Function)
            function
          else
            Function[function]
          end

        super(type, **options, fn: function)
      end

      # @param [Builder, Object] input
      # @param [Hash] options
      # @param [#call, nil] block
      #
      # @api public
      def self.[](type, fn:, context: false, **options)
        function = context ? Function[fn, contextual: true] : Function[fn]

        if function.wrapper?
          wrapper_type.new(type, fn: function, **options)
        else
          new(type, fn: function, **options)
        end
      end

      # @api private
      def self.wrapper_type
        @wrapper_type ||=
          if self < Wrapper
            self
          else
            const_set(:Wrapping, ::Class.new(self).include(Wrapper))
          end
      end

      # Instantiate a new constructor type instance
      #
      # @param [Type] type
      # @option [Function] fn
      # @param [Hash] options
      #
      # @api private
      def initialize(type, fn: nil, **options)
        @type = type
        @fn = fn

        super(type, **options, fn: fn)
      end

      # @return [Object]
      #
      # @api private
      def call_safe(input, context = Undefined)
        if fn.contextual?
          coerced = fn.(input, context) { |output = input| return yield(output) }
          type.call_safe(coerced, context) { |output = coerced| yield(output) }
        else
          coerced = fn.(input) { |output = input| return yield(output) }
          type.call_safe(coerced) { |output = coerced| yield(output) }
        end
      end

      # @return [Object]
      #
      # @api private
      def call_unsafe(input, context = Undefined)
        if fn.contextual?
          type.call_unsafe(fn.(input, context), context)
        else
          type.call_unsafe(fn.(input))
        end
      end

      # @param [Object] input
      # @param [Object] context per-call context
      # @param [#call,nil] block
      #
      # @return [Logic::Result, Types::Result]
      # @return [Object] if block given and try fails
      #
      # @api public
      def try(input, context = Undefined, &)
        value =
          if fn.contextual?
            fn.(input, context)
          else
            fn.(input)
          end
      rescue CoercionError => exception
        failure = failure(input, exception)
        block_given? ? yield(failure) : failure
      else
        type.try(value, context, &)
      end

      # Build a new constructor by appending a block to the coercion function
      #
      # @param [#call, nil] new_fn
      # @param [Boolean] context Whether the appended block receives the
      #   per-call context
      # @param [Hash] options
      # @param [#call, nil] block
      #
      # @return [Constructor]
      #
      # @api public
      def constructor(new_fn = nil, context: false, **options, &block)
        next_fn =
          if context
            Function[new_fn || block, contextual: true]
          else
            Function[new_fn || block]
          end

        if next_fn.wrapper?
          self.class.wrapper_type.new(with(**options), fn: next_fn)
        else
          with(**options, fn: fn >> next_fn)
        end
      end
      alias_method :append, :constructor
      alias_method :>>, :constructor

      # @return [Class]
      #
      # @api private
      def constrained_type = Constrained::Coercible

      # @see Nominal#to_ast
      #
      # @api public
      def to_ast(meta: true)
        [:constructor, [type.to_ast(meta: meta), fn.to_ast]]
      end

      # Build a new constructor by prepending a block to the coercion
      # function
      #
      # @param [#call, nil] new_fn
      # @param [Boolean] context Whether the prepended block receives the
      #   per-call context
      # @param [Hash] options
      # @param [#call, nil] block
      #
      # @return [Constructor]
      #
      # @api public
      def prepend(new_fn = nil, context: false, **options, &block)
        added = Function[new_fn || block, contextual: true] if context
        added ||= Function[new_fn || block]

        with(**options, fn: fn << added)
      end
      alias_method :<<, :prepend

      # @api private
      def lax
        if fn.wrapper?
          # Wrapper constructors handle type failures themselves, rebuilding
          # one keeps that behavior without an outer Lax layer
          constructor_type[type.lax, fn: fn, **options]
        else
          Lax.new(constructor_type[type.lax, fn: fn, **options])
        end
      end

      # Wrap the type with a proc
      #
      # @return [Proc]
      #
      # @api public
      def to_proc = proc { self.(_1) }

      private

      # @param [Symbol] meth
      # @param [Boolean] include_private
      # @return [Boolean]
      #
      # @api private
      def respond_to_missing?(meth, include_private = false)
        super || type.respond_to?(meth)
      end

      # Delegates missing methods to {#type}
      #
      # @param [Symbol] method
      # @param [Array] args
      # @param [#call, nil] block
      #
      # @api private
      def method_missing(method, ...)
        if type.respond_to?(method)
          response = type.public_send(method, ...)

          if response.is_a?(Type) && response.instance_of?(type.class)
            response.constructor_type[response, **options]
          else
            response
          end
        else
          super
        end
      end
    end
  end
end
