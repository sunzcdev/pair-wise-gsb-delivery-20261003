# frozen_string_literal: true

module Dry
  module Types
    # Common Type module denoting an object is a Type
    #
    # @api public
    module Type
      extend ::Dry::Core::Deprecations[:"dry-types"]

      deprecate(:safe, :lax)

      # Whether a value is a valid member of the type
      #
      # @return [Boolean]
      #
      # @api private
      def valid?(input = Undefined)
        call_safe(input) { return false }
        true
      end
      # Anything can be coerced matches
      alias_method :===, :valid?

      # Apply type to a value
      #
      # @overload call(input = Undefined, context = Undefined)
      #   Possibly unsafe coercion attempt. If a value doesn't
      #   match the type, an exception will be raised.
      #
      #   @param [Object] input
      #   @param [Object] context Optional per-call context. It is
      #     threaded through the whole coercion (nested schemas, arrays,
      #     defaults and sum branches) and is only passed to constructors
      #     that explicitly declare they accept it (+context: true+).
      #     It is never stored on the type, so it cannot leak between
      #     calls, threads or Fibers.
      #   @return [Object]
      #
      # @overload call(input = Undefined, context = Undefined)
      #   When a block is passed, {#call} will never throw an exception on
      #   failed coercion, instead it will call the block.
      #
      #   @param [Object] input
      #   @param [Object] context see above
      #   @yieldparam [Object] output Partially coerced value
      #   @return [Object]
      #
      # @api public
      def call(input = Undefined, context = Undefined, &)
        if block_given?
          call_safe(input, context, &)
        else
          call_unsafe(input, context)
        end
      end
      alias_method :[], :call
    end
  end
end
