# frozen_string_literal: true

module Datadog
  class Statsd
    class MessageBuffer
      PAYLOAD_SIZE_TOLERANCE = 0.05

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
        # stores the serialized lines currently held in `@buffer` so that
        # they can be re-grouped if the payload limit changes at runtime
        @messages = []
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

        append_message(message)

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

      # Switches the underlying connection and/or the maximum payload size.
      #
      # Lines already buffered but not sent yet are kept and re-grouped
      # according to the new payload limit: packets that were packed for the
      # previous (larger) limit are split as needed so that every packet sent
      # through the new connection is compliant. Lines that do not fit in a
      # single packet of the new limit are explicitly dropped.
      #
      # Packets that have already been flushed before the switch are left
      # untouched (they remain on the previous connection's account).
      def switch_connection(connection, max_payload_size: nil)
        raise ArgumentError, 'max_payload_size cannot be <= 0' if max_payload_size && max_payload_size <= 0

        # take a copy: clear_buffer mutates @messages in place
        pending_messages = @messages.dup

        @connection = connection
        @max_payload_size = max_payload_size if max_payload_size
        @bytesize_threshold = nil

        clear_buffer

        pending_messages.each do |message|
          add(message)
        end
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
        @messages.clear
        @message_count = 0
      end

      def append_message(message)
        buffer << "\n" unless buffer.empty?
        buffer << message
        @messages << message
        @message_count += 1
      end

      def preemptive_flush?
        @message_count == max_pool_size || buffer.bytesize > bytesize_threshold
      end

      def ensure_sendable!(message_size)
        return true if message_size <= max_payload_size

        if overflowing_stategy == :raise
          raise Error, 'Message too big for payload limit'
        end

        log_dropped_message(message_size)
        false
      end

      def log_dropped_message(message_size)
        return unless connection.respond_to?(:logger)

        logger = connection.logger
        return unless logger

        logger.error do
          "Statsd: dropping message of #{message_size} bytes, " \
            "it exceeds the max payload size of #{max_payload_size} bytes"
        end
      end

      def bytesize_threshold
        @bytesize_threshold ||= (max_payload_size - PAYLOAD_SIZE_TOLERANCE * max_payload_size).to_i
      end
    end
  end
end
