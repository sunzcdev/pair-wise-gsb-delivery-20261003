# frozen_string_literal: true

module Dry
  module Types
    # Shared types apply the wrapped type using structural sharing:
    # whenever a container's keys, element identities, order and
    # retention are left untouched by the coercion, the very same
    # container object is returned instead of a freshly allocated copy.
    #
    # Identity is always checked with {Object#equal?}. Values that are
    # merely `==` (for example 1 and 1.0) are never considered shared.
    #
    # Inserted default values, discarded unknown keys and filtered out
    # Undefined elements all break sharing for the affected container.
    #
    # @api public
    class Shared
      include Type
      include Decorator
      include Builder
      include Printable
      include ::Dry::Equalizer(:type, inspect: false, immutable: true)

      # @param [Object] input
      #
      # @return [Object]
      #
      # @api public
      def call(input = Undefined, options = EMPTY_HASH, &block)
        block ? call_safe(input, options, &block) : call_unsafe(input, options)
      end
      alias_method :[], :call

      # @api private
      def call_unsafe(input = Undefined, options = EMPTY_HASH)
        type.call_unsafe(input, SHARED_OPTIONS.merge(options))
      end

      # @api private
      def call_safe(input = Undefined, options = EMPTY_HASH, &block)
        type.call_safe(input, SHARED_OPTIONS.merge(options), &block)
      end

      # Apply the type with additional call options, see {Schema#apply}
      #
      # @param [Object] input
      # @param [Hash] options
      #
      # @return [Object]
      #
      # @api public
      def apply(input, options = EMPTY_HASH)
        type.apply(input, SHARED_OPTIONS.merge(options))
      end

      # @param [Object] input
      # @param [#call,nil] block
      #
      # @yieldparam [Failure] failure
      # @yieldreturn [Result]
      #
      # @return [Result,Logic::Result]
      # @return [Object] if coercion fails and a block is given
      #
      # @api public
      def try(input, &) = type.try(input, &)

      # Structural sharing is an allocation optimization, so a shared
      # type does not add another wrapping layer
      #
      # @return [Shared]
      #
      # @api public
      def shared = self

      # @see Nominal#to_ast
      #
      # @api public
      def to_ast(meta: true)
        [:shared, [type.to_ast(meta: meta), meta ? self.meta : EMPTY_HASH]]
      end

      private

      # @param [Object] response
      #
      # @return [Boolean]
      #
      # @api private
      def decorate?(response)
        super || response.is_a?(type.constructor_type)
      end
    end
  end
end
