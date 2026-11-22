# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

module OpenTelemetry
  module SDK
    module Trace
      # Class that holds global trace parameters.
      class SpanLimits
        # The global default max number of attributes per {Span}.
        attr_reader :attribute_count_limit

        # The global default max length of attribute value per {Span}.
        attr_reader :attribute_length_limit

        # The global default max number of {OpenTelemetry::SDK::Trace::Event}s per {Span}.
        attr_reader :event_count_limit

        # The global default max number of {OpenTelemetry::Trace::Link} entries per {Span}.
        attr_reader :link_count_limit

        # The global default max number of attributes per {OpenTelemetry::SDK::Trace::Event}.
        attr_reader :event_attribute_count_limit

        # The global default max length of attribute value per {OpenTelemetry::SDK::Trace::Event}.
        attr_reader :event_attribute_length_limit

        # The global default max number of attributes per {OpenTelemetry::Trace::Link}.
        attr_reader :link_attribute_count_limit

        # The retention policy applied when the {#event_count_limit} or
        # {#link_count_limit} is reached.
        #
        # - +nil+ keeps the historical behaviour: events discard the oldest
        #   entries (keeping the newest) and links discard the newest entries
        #   (keeping the oldest). Preferred markers are ignored.
        # - +:keep_newest+ discards the oldest entries, keeping the newest.
        # - +:keep_oldest+ discards the newest entries, keeping the oldest.
        # - +:keep_preferred+ retains entries marked as preferred first,
        #   regardless of age, and fills any remaining capacity with unmarked
        #   entries using the historical per-type rule (newest events, oldest
        #   links). Typically produced by
        #   {OpenTelemetry::SDK::Trace::Span#add_event} with
        #   <tt>preferred: true</tt> and
        #   {OpenTelemetry::Trace::Link.new} with <tt>preferred: true</tt>.
        #   When the preferred entries alone exceed the limit, the newest
        #   preferred entries are retained.
        #
        # Every policy honours the existing count limits, and the retained
        # entries stay in the order they were recorded.
        #
        # @return [Symbol, nil]
        attr_reader :retention_policy

        # The values accepted by the +retention_policy+ option.
        RETENTION_POLICIES = [nil, :keep_newest, :keep_oldest, :keep_preferred].freeze

        # Returns a {SpanLimits} with the desired values.
        #
        # @return [SpanLimits] with the desired values.
        # @raise [ArgumentError] if any of the max numbers are not positive.
        def initialize(attribute_count_limit: Integer(OpenTelemetry::Common::Utilities.config_opt('OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT', 'OTEL_ATTRIBUTE_COUNT_LIMIT', default: 128)), # rubocop:disable Metrics/CyclomaticComplexity, Metrics/PerceivedComplexity
                       attribute_length_limit: OpenTelemetry::Common::Utilities.config_opt('OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT', 'OTEL_RUBY_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT', 'OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT'),
                       event_count_limit: Integer(OpenTelemetry::Common::Utilities.config_opt('OTEL_SPAN_EVENT_COUNT_LIMIT', default: 128)),
                       link_count_limit: Integer(OpenTelemetry::Common::Utilities.config_opt('OTEL_SPAN_LINK_COUNT_LIMIT', default: 128)),
                       event_attribute_count_limit: Integer(OpenTelemetry::Common::Utilities.config_opt('OTEL_EVENT_ATTRIBUTE_COUNT_LIMIT', default: 128)),
                       event_attribute_length_limit: OpenTelemetry::Common::Utilities.config_opt('OTEL_EVENT_ATTRIBUTE_VALUE_LENGTH_LIMIT', 'OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT'),
                       link_attribute_count_limit: Integer(OpenTelemetry::Common::Utilities.config_opt('OTEL_LINK_ATTRIBUTE_COUNT_LIMIT', default: 128)),
                       retention_policy: nil)
          raise ArgumentError, 'attribute_count_limit must be positive' unless attribute_count_limit.positive?
          raise ArgumentError, 'attribute_length_limit must not be less than 32' unless attribute_length_limit.nil? || Integer(attribute_length_limit) >= 32
          raise ArgumentError, 'event_count_limit must be positive' unless event_count_limit.positive?
          raise ArgumentError, 'link_count_limit must be positive' unless link_count_limit.positive?
          raise ArgumentError, 'event_attribute_count_limit must be positive' unless event_attribute_count_limit.positive?
          raise ArgumentError, 'event_attribute_length_limit must not be less than 32' unless event_attribute_length_limit.nil? || Integer(event_attribute_length_limit) >= 32
          raise ArgumentError, 'link_attribute_count_limit must be positive' unless link_attribute_count_limit.positive?
          raise ArgumentError, 'retention_policy must be nil, :keep_newest, :keep_oldest, or :keep_preferred' unless RETENTION_POLICIES.include?(retention_policy)

          @attribute_count_limit = attribute_count_limit
          @attribute_length_limit = attribute_length_limit.nil? ? nil : Integer(attribute_length_limit)
          @event_count_limit = event_count_limit
          @link_count_limit = link_count_limit
          @event_attribute_count_limit = event_attribute_count_limit
          @event_attribute_length_limit = event_attribute_length_limit.nil? ? nil : Integer(event_attribute_length_limit)
          @link_attribute_count_limit = link_attribute_count_limit
          @retention_policy = retention_policy
        end

        # The default {SpanLimits}.
        DEFAULT = new
      end
    end
  end
end
