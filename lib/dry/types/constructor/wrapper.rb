# frozen_string_literal: true

module Dry
  module Types
    # @api public
    class Constructor < Nominal
      module Wrapper
        # @return [Object]
        #
        # @api private
        def call_safe(input, context = Undefined, &block)
          if fn.contextual?
            fn.(input, contextual_type(context), context, &block)
          else
            fn.(input, contextual_type(context), &block)
          end
        end

        # @return [Object]
        #
        # @api private
        def call_unsafe(input, context = Undefined)
          if fn.contextual?
            fn.(input, contextual_type(context), context)
          else
            fn.(input, contextual_type(context))
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
        def try(input, context = Undefined, &block)
          value =
            if fn.contextual?
              fn.(input, contextual_type(context), context)
            else
              fn.(input, contextual_type(context))
            end
        rescue CoercionError => exception
          failure = failure(input, exception)
          block_given? ? yield(failure) : failure
        else
          type.try(value, context, &block)
        end

        # Define a constructor for the type
        #
        # @param [#call,nil] constructor
        # @param [Hash] options
        # @param [Boolean] context
        # @param [#call,nil] block
        #
        # @return [Constructor]
        #
        # @api public
        define_method(:constructor, Builder.instance_method(:constructor))
        alias_method :append, :constructor
        alias_method :>>, :constructor

        # Build a new constructor by prepending a block to the coercion function
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
          prep_fn =
            if context
              Function[new_fn || block, contextual: true]
            else
              Function[new_fn || block]
            end

          decorated =
            if prep_fn.wrapper?
              type.constructor(prep_fn, **options)
            else
              type.prepend(prep_fn, **options)
            end

          __new__(decorated)
        end
        alias_method :<<, :prepend

        # @return [Constructor]
        #
        # @api public
        def lax
          # return self back because wrapping function
          # can handle failed type check
          self
        end

        private

        # Replace underlying type
        #
        # @api private
        def __new__(type) = self.class.new(type, *@__args__.drop(1), **@options)

        # A thin view over {#type} that implicitly carries the current call's
        # context, so a wrapper block calling +type.call(input)+ keeps
        # threading the context into contextual constructors below.
        #
        # @param [Object] context
        # @return [Type, ContextBoundType]
        #
        # @api private
        def contextual_type(context)
          if Undefined.equal?(context)
            type
          else
            ContextBoundType.new(type, context)
          end
        end
      end

      # Delegates every call to a wrapped type while forwarding the context
      # of the current call. Instances are short-lived and per-call, so they
      # never store context on the shared, frozen types.
      #
      # @api private
      class ContextBoundType < ::BasicObject
        # @param [Type] type
        # @param [Object] context
        def initialize(type, context)
          @type = type
          @context = context
        end

        # @return [Object]
        def call(input = ::Dry::Types::Undefined, &block)
          @type.call(input, @context, &block)
        end
        alias_method :[], :call

        # @return [Object]
        def call_unsafe(input, _context = ::Dry::Types::Undefined)
          @type.call_unsafe(input, @context)
        end

        # @return [Object]
        def call_safe(input, _context = ::Dry::Types::Undefined, &block)
          @type.call_safe(input, @context, &block)
        end

        # @return [Result]
        def try(input, _context = ::Dry::Types::Undefined, &block)
          @type.try(input, @context, &block)
        end

        # Everything else behaves exactly like the wrapped type
        def method_missing(...) = @type.public_send(...)

        def respond_to_missing?(name, include_private = false)
          @type.respond_to?(name, include_private)
        end
      end
    end
  end
end
