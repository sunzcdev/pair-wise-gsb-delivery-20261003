# frozen_string_literal: true

require "dry/core/equalizer"
require "dry/types/options"
require "dry/types/meta"

module Dry
  module Types
    # Intersection type
    #
    # @api public
    class Intersection
      include Composition

      def self.operator = :&

      # @param [Object] input
      # @param [Object] context per-call context
      #
      # @return [Object]
      #
      # @api private
      def call_unsafe(input, context = Undefined)
        merge_results(
          left.call_unsafe(input, context),
          right.call_unsafe(input, context)
        )
      end

      # @param [Object] input
      # @param [Object] context per-call context
      #
      # @return [Object]
      #
      # @api private
      def call_safe(input, context = Undefined, &) =
        try_sides(input, context, &).input

      # @param [Object] input
      # @param [Object] context per-call context
      #
      # @api public
      def try(input, context = Undefined)
        try_sides(input, context) do |failure|
          if block_given?
            yield(failure)
          else
            failure
          end
        end
      end

      # @param [Object] value
      #
      # @return [Boolean]
      #
      # @api private
      def primitive?(value)
        left.primitive?(value) && right.primitive?(value)
      end

      private

      # @api private
      def try_sides(input, context, &block)
        results = []

        [left, right].each do |side|
          result = try_side(side, input, context, &block)
          return result if result.failure?

          results << result
        end

        Result::Success.new(merge_results(*results.map(&:input)))
      end

      # @api private
      def try_side(side, input, context)
        failure = nil

        result = side.try(input, context) do |f|
          failure = f
          yield(f)
        end

        if result.is_a?(Result)
          result
        elsif failure
          Result::Failure.new(result, failure)
        else
          Result::Success.new(result)
        end
      end

      # @api private
      def merge_results(left_result, right_result)
        case left_result
        when ::Array
          left_result.zip(right_result).map { merge_results(_1, _2) }
        when ::Hash
          left_result.merge(right_result)
        else
          left_result
        end
      end
    end
  end
end
