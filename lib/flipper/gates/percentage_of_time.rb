module Flipper
  module Gates
    class PercentageOfTime < Gate
      # Internal: The name of the gate. Used for instrumentation, etc.
      def name
        :percentage_of_time
      end

      # Internal: Name converted to value safe for adapter.
      def key
        :percentage_of_time
      end

      def data_type
        :integer
      end

      def enabled?(value)
        value > 0
      end

      # Internal: Checks if the gate is open for a thing.
      #
      # Returns true if gate open for thing, false if not.
      def open?(context)
        percentage = context.values.percentage_of_time

        if percentage <= 0
          context.report&.record_gate(name, false, percentage: percentage)
          return false
        end

        if percentage >= 100
          context.report&.record_gate(name, true, percentage: percentage)
          return true
        end

        threshold = percentage / 100.0
        number = context.report ? context.report.read_random : rand
        result = number < threshold

        context.report&.record_gate(
          name,
          result,
          percentage: percentage,
          random: number,
          threshold: threshold
        )
        result
      end

      def protects?(thing)
        thing.is_a?(Flipper::Types::PercentageOfTime)
      end
    end
  end
end
