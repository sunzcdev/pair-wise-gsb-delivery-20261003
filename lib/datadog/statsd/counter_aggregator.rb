# frozen_string_literal: true

module Datadog
  class Statsd
    # CounterAggregator merges, for the duration of a flush window, the
    # DogStatsd counter (`|c`) messages that are equivalent: same metric name,
    # same (equivalent) tag set and same sample rate. Their deltas are simply
    # added together and a single counter message is emitted per group when
    # the buffer is flushed.
    #
    # Any message that is not a counter (gauges, histograms, timings, sets,
    # distributions, events, service checks, ...) is ignored by the aggregator
    # and keeps being sent as-is by the message buffer.
    class CounterAggregator
      # "metric.name:delta|c" optionally followed by rate/tags/fields sections
      COUNTER_MESSAGE_REGEXP = %r{\A([^:]+):([^|]+)\|c(?:\||\z)}

      def initialize
        # key   = [metric_name, rate_section, tags_section, fields_section]
        # value = aggregated delta
        @deltas = {}
        # insertion order of the keys, so flushing keeps call order stable
        @keys = []
        # cache of the last rendered message for each key
        @rendered = {}
        # bytesize of all the rendered counter messages, without any
        # separator between them, cached for O(1) buffer sizing
        @messages_bytesize = 0
      end

      # Merges a serialized counter message.
      #
      # @return [Array, nil] the aggregation key when the message was a
      #   counter that has been merged, nil when the message must be sent
      #   untouched (not a counter or unparseable).
      def add(message)
        match = COUNTER_MESSAGE_REGEXP.match(message)
        return nil unless match

        metric_name = match[1]
        delta = parse_delta(match[2])
        return nil unless delta

        sections = message.split('|')
        # sections[0] is "metric_name:delta", sections[1] is "c"
        rate_section, tags_section, fields_section = partition_sections(sections[2..-1])

        key = [metric_name, rate_section, tags_section, fields_section]

        if @deltas.key?(key)
          @messages_bytesize -= @rendered[key].bytesize
          update_group(key, @deltas[key] + delta)
          @messages_bytesize += @rendered[key].bytesize
        else
          @keys << key
          update_group(key, delta)
          @messages_bytesize += @rendered[key].bytesize
        end

        key
      end

      # Cancels the effect of the last #add for a key. Used when the buffer
      # has to flush before the newly merged message can be registered, so
      # the message can be re-added to a fresh aggregation window.
      #
      # @param [Array] key the aggregation key returned by #add
      # @param [Object] delta the delta of the message being rolled back
      # @param [Boolean] new_group whether the key was unknown before #add
      def rollback(key, delta, new_group)
        if new_group
          @keys.delete(key)
          @deltas.delete(key)
          @messages_bytesize -= @rendered.delete(key).bytesize
        else
          @messages_bytesize -= @rendered[key].bytesize
          update_group(key, @deltas[key] - delta)
          @messages_bytesize += @rendered[key].bytesize
        end
      end

      # Tells whether an aggregation group is already tracked.
      def key?(key)
        @deltas.key?(key)
      end

      # Number of currently tracked aggregation groups.
      def size
        @keys.size
      end

      # Bytesize of all the rendered counter messages, without counting the
      # "\n" separators used to join them.
      def messages_bytesize
        @messages_bytesize
      end

      def empty?
        @keys.empty?
      end

      # Returns the current rendered message for an aggregation key.
      def rendered_message(key)
        @rendered[key]
      end

      # Renders all the merged counter messages, in the order in which each
      # aggregation group was first seen.
      def render
        @keys.map { |key| @rendered[key] }
      end

      def reset
        @deltas.clear
        @keys.clear
        @rendered.clear
        @messages_bytesize = 0
      end

      private

      def update_group(key, delta)
        @deltas[key] = delta
        @rendered[key] = render_message(key, delta)
      end

      def partition_sections(sections)
        rate_section = nil
        tags_section = nil
        fields = []

        sections.each do |section|
          if section.start_with?('@')
            rate_section = section
          elsif section.start_with?('#')
            tags_section = section
          else
            fields << section
          end
        end

        [rate_section, tags_section, fields]
      end

      def render_message(key, delta)
        metric_name, rate_section, tags_section, fields_section = key

        message = String.new
        message << metric_name
        message << ':'
        message << delta.to_s
        message << '|c'
        message << '|' << rate_section if rate_section
        message << '|' << tags_section if tags_section
        fields_section.each { |field| message << '|' << field }
        message
      end

      def parse_delta(raw_delta)
        Integer(raw_delta, exception: false) || Float(raw_delta, exception: false)
      end
    end
  end
end
