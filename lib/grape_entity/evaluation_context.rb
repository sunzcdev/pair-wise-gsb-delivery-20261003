# frozen_string_literal: true

module Grape
  class Entity
    # Per-representation memoization boundary for exposure evaluation.
    #
    # The cache lives on the options of one serializable_hash call instead of
    # being attached to represented objects. This keeps representations with
    # different options isolated and allows nil/false values to be cached.
    class EvaluationContext
      def initialize
        @raw_values = {}
        @values = {}
      end

      def value(exposure, entity, options)
        return exposure.value(entity, options) unless exposure

        @values.fetch(exposure) do
          @values[exposure] = exposure.value_from_raw(raw_value(exposure, entity, options), entity, options)
        end
      end

      def raw_value(exposure, entity, options)
        return exposure.evaluate_raw_value(entity, options) unless exposure

        @raw_values.fetch(exposure) do
          @raw_values[exposure] = exposure.evaluate_raw_value(entity, options)
        end
      end

      def evaluating(exposure)
        previous = @current_exposure
        @current_exposure = exposure
        yield
      ensure
        @current_exposure = previous
      end

      def condition_value(entity, options)
        @current_exposure.condition_raw_value(entity, options)
      end
    end
  end
end
