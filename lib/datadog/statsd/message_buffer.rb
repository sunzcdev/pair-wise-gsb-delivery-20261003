# frozen_string_literal: true

require_relative 'counter_aggregator'

module Datadog
  class Statsd
    class MessageBuffer
      PAYLOAD_SIZE_TOLERANCE = 0.05

      def initialize(connection,
        max_payload_size: nil,
        max_pool_size: DEFAULT_BUFFER_POOL_SIZE,
        overflowing_stategy: :drop,
        serializer:,
        counter_aggregation: false
      )
        raise ArgumentError, 'max_payload_size keyword argument must be provided' unless max_payload_size
        raise ArgumentError, 'max_pool_size keyword argument must be provided' unless max_pool_size

        @connection = connection
        @max_payload_size = max_payload_size
        @max_pool_size = max_pool_size
        @overflowing_stategy = overflowing_stategy
        @serializer = serializer
        @counter_aggregation = counter_aggregation
        @counter_aggregator = counter_aggregation ? CounterAggregator.new : nil

        @buffer = String.new
        # ordered list of entries, either a plain (non-aggregated) message
        # String or an aggregated counter key (Array)
        @entries = []
        clear_buffer
      end

      def add(message)
        # Serializes the message if it hasn't been already. Part of the
        # delay_serialization feature.
        if message.is_a?(Array)
          stat, delta, type, tags, sample_rate, cardinality = message
          message = @serializer.to_stat(stat, delta, type,
            tags: tags, sample_rate: sample_rate, cardinality: cardinality)
        end

        message_size = message.bytesize

        return nil unless message_size > 0 # to avoid adding empty messages to the buffer
        return nil unless ensure_sendable!(message_size)

        aggregation_key = @counter_aggregator.add(message) if @counter_aggregation

        if aggregation_key
          new_group = !@entries.include?(aggregation_key)

          if would_overflow?(new_group)
            # the current window is full: undo the tentative merge, flush
            # the window, then re-apply the message on the fresh window
            @counter_aggregator.rollback(aggregation_key, parsed_delta(message), new_group)
            flush
            aggregation_key = @counter_aggregator.add(message)
            new_group = true
          end

          if new_group
            # only register the entry the first time this group is seen, the
            # aggregator itself holds the running delta
            @entries << aggregation_key
            @message_count += 1
          end
        else
          flush if would_overflow_plain?(message_size)

          buffer << "\n" unless buffer.empty?
          buffer << message
          @entries << message
          @message_count += 1
        end

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
        return if buffer.empty? && (!@counter_aggregation || @counter_aggregator.empty?)

        connection.write(render_payload)
        clear_buffer
      end

      private

      attr :max_payload_size
      attr :max_pool_size

      attr :overflowing_stategy

      attr :connection
      attr :buffer

      # Exact bytesize of the currently buffered payload.
      def current_bytesize
        if @counter_aggregation
          # every entry but the first is joined to the previous one with a
          # "\n" separator
          buffer.bytesize + @counter_aggregator.messages_bytesize + (@entries.empty? ? 0 : @entries.size - 1)
        else
          buffer.bytesize
        end
      end

      # Whether appending a plain (non-aggregated) message would overflow.
      def would_overflow_plain?(message_size)
        separator = @entries.empty? ? 0 : 1
        current_bytesize + separator + message_size >= max_payload_size
      end

      # Whether the (already updated) aggregated group makes the payload
      # overflow. A brand new group also needs a "\n" separator.
      def would_overflow?(new_group)
        extra_separator = new_group && !@entries.empty? ? 1 : 0
        current_bytesize + extra_separator >= max_payload_size
      end

      def parsed_delta(message)
        match = CounterAggregator::COUNTER_MESSAGE_REGEXP.match(message)
        return nil unless match

        Integer(match[2], exception: false) || Float(match[2], exception: false)
      end

      def render_payload
        return buffer unless @counter_aggregation

        @entries.map do |entry|
          entry.is_a?(Array) ? @counter_aggregator.rendered_message(entry) : entry
        end.join("\n")
      end

      def clear_buffer
        buffer.clear
        @entries.clear
        @counter_aggregator.reset if @counter_aggregation
        @message_count = 0
      end

      def preemptive_flush?
        @message_count == max_pool_size || current_bytesize > bytesize_threshold
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
