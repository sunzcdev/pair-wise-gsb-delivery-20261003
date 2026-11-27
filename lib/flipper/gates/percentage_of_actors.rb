require 'zlib'

module Flipper
  module Gates
    class PercentageOfActors < Gate
      # Internal: The name of the gate. Used for instrumentation, etc.
      def name
        :percentage_of_actors
      end

      # Internal: Name converted to value safe for adapter.
      def key
        :percentage_of_actors
      end

      def data_type
        :integer
      end

      def enabled?(value)
        value > 0
      end

      # Private: this constant is used to support up to 3 decimal places
      # in percentages.
      SCALING_FACTOR = 1_000
      private_constant :SCALING_FACTOR

      # Internal: Checks if the gate is open for one or more actors.
      #
      # Returns true if gate open for any actors, false if not.
      def open?(context)
        unless context.actors?
          context.report&.skip_gate(name, :no_actors)
          return false
        end

        percentage = context.values.percentage_of_actors

        if percentage <= 0
          context.report&.record_gate(name, false, percentage: percentage)
          return false
        end

        if percentage >= 100
          context.report&.record_gate(name, true, percentage: percentage)
          return true
        end

        actors = context.actors
        id = if actors.size == 1
          "#{context.feature_name}#{actors.first.value}"
        else
          "#{context.feature_name}#{actors.map(&:value).sort.join}"
        end
        # NOTE: `crc32 % 100_000` has a tiny modulo bias (2**32 is not a
        # multiple of 100_000, so buckets 0..67_295 are ~0.0023% over-
        # represented). This is intentionally left as-is: the bias is
        # negligible, and any "fix" changes the hash-to-bucket mapping,
        # which would re-bucket essentially every actor on upgrade and
        # silently flip who is enabled. Stable, deterministic bucketing
        # matters far more here than perfect uniformity. Do not change.
        bucket = Zlib.crc32(id) % (100 * SCALING_FACTOR)
        result = bucket < percentage * SCALING_FACTOR

        context.report&.record_gate(
          name,
          result,
          percentage: percentage,
          id: id,
          bucket: bucket,
          threshold: percentage * SCALING_FACTOR
        )
        result
      end

      def protects?(thing)
        thing.is_a?(Types::PercentageOfActors)
      end
    end
  end
end
