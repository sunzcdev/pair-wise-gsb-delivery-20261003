# frozen_string_literal: true

module Datadog
  class Statsd
    class MessageBuffer
      PAYLOAD_SIZE_TOLERANCE = 0.05

      # Collects packed payloads in memory instead of writing them to a real
      # StatsD connection. This is used by isolated/nested batches so that
      # messages can be discarded if their scope fails.
      class CaptureConnection
        attr_reader :packets

        def initialize
          @packets = []
        end

        def write(packet)
          # The owning MessageBuffer mutates and clears the string it passes to
          # write, so capture an independent copy.
          @packets << packet.dup
        end

        def reset_telemetry
          # no telemetry is owned by the capture connection
        end
      end

      def initialize(connection,
        max_payload_size: nil,
        max_pool_size: DEFAULT_BUFFER_POOL_SIZE,
        overflowing_stategy: :drop,
        serializer:
      )
        raise ArgumentError, 'max_payload_size keyword argument must be provided' unless max_payload_size
        raise ArgumentError, 'max_pool_size keyword argument must be provided' unless max_pool_size

        @connection = connection
        @max_payload_size = max_payload_size
        @max_pool_size = max_pool_size
        @overflowing_stategy = overflowing_stategy
        @serializer = serializer

        @buffer = String.new
        clear_buffer
      end

      def add(message)
        # Serializes the message if it hasn't been already. Part of the
        # delay_serialization feature.
        if message.is_a?(Array)
          stat, delta, type, tags, sample_rate, cardinality = message
          message = @serializer.to_stat(stat, delta, type, tags: tags, sample_rate: sample_rate, cardinality: cardinality)
        end

        message_size = message.bytesize

        return nil unless message_size > 0 # to avoid adding empty messages to the buffer
        return nil unless ensure_sendable!(message_size)

        flush if should_flush?(message_size)

        buffer << "\n" unless buffer.empty?
        buffer << message

        @message_count += 1

        # flush when we're pretty sure that we won't be able
        # to add another message to the buffer
        flush if preemptive_flush?

        true
      end

      def reset
        clear_buffer
        connection.reset_telemetry
      end

      def flush
        return if buffer.empty?

        connection.write(buffer)
        clear_buffer
      end

      # Flushes the current packet into the capture connection and returns all
      # captured packets. Only meaningful for buffers initialized with
      # {CaptureConnection}.
      def captured_packets
        flush
        connection.packets.dup
      end

      private

      attr :max_payload_size
      attr :max_pool_size

      attr :overflowing_stategy

      attr :connection
      attr :buffer

      def should_flush?(message_size)
        return true if buffer.bytesize + 1 + message_size >= max_payload_size

        false
      end

      def clear_buffer
        buffer.clear
        @message_count = 0
      end

      def preemptive_flush?
        @message_count == max_pool_size || buffer.bytesize > bytesize_threshold
      end

      def ensure_sendable!(message_size)
        return true if message_size <= max_payload_size

        if overflowing_stategy == :raise
          raise Error, 'Message too big for payload limit'
        end

        false
      end

      def bytesize_threshold
        @bytesize_threshold ||= (max_payload_size - PAYLOAD_SIZE_TOLERANCE * max_payload_size).to_i
      end
    end
  end
end
