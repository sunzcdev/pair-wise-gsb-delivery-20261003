# frozen_string_literal: true

module Dry
  module Types
    # Implication type
    #
    # @api public
    class Implication
      include Composition

      def self.operator = :>

      # @param [Object] input
      # @param [Object] context per-call context
      #
      # @return [Object]
      #
      # @api private
      def call_unsafe(input, context = Undefined)
        if left.try(input, context).success?
          right.call_unsafe(input, context)
        else
          input
        end
      end

      # @param [Object] input
      # @param [Object] context per-call context
      #
      # @return [Object]
      #
      # @api private
      def call_safe(input, context = Undefined, &)
        if left.try(input, context).success?
          right.call_safe(input, context, &)
        else
          input
        end
      end

      # @param [Object] input
      # @param [Object] context per-call context
      #
      # @api public
      def try(input, context = Undefined, &)
        if left.try(input, context).success?
          right.try(input, context, &)
        else
          Result::Success.new(input)
        end
      end

      # @param [Object] value
      #
      # @return [Boolean]
      #
      # @api private
      def primitive?(value)
        if left.primitive?(value)
          right.primitive?(value)
        else
          true
        end
      end
    end
  end
end
