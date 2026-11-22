# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'opentelemetry/baggage/propagation/serializer'

module OpenTelemetry
  module Baggage
    module Propagation
      # Propagates baggage using the W3C Baggage format
      class TextMapPropagator
        BAGGAGE_KEY = 'baggage'
        FIELDS = [BAGGAGE_KEY].freeze

        private_constant :BAGGAGE_KEY, :FIELDS

        # Inject in-process baggage into the supplied carrier.
        #
        # @param [Carrier] carrier The mutable carrier to inject baggage into
        # @param [Context] context The context to read baggage from
        # @param [optional Setter] setter If the optional setter is provided, it
        #   will be used to write context into the carrier, otherwise the default
        #   text map setter will be used.
        def inject(carrier, context: Context.current, setter: Context::Propagation.text_map_setter)
          baggage = OpenTelemetry::Baggage.raw_entries(context: context)

          return if baggage.nil? || baggage.empty?

          encoded_baggage = SERIALIZER.encode(baggage)
          setter.set(carrier, BAGGAGE_KEY, encoded_baggage) unless encoded_baggage.empty?
          nil
        end

        # Extract remote baggage from the supplied carrier.
        # If extraction fails or there is no baggage to extract,
        # then the original context will be returned.
        #
        # All +baggage+ header fields in the carrier are combined into a single
        # list, because the W3C limits apply to their combination. Invalid
        # list-members are skipped individually rather than discarding the whole
        # header. When a key occurs more than once, the last occurrence wins, as
        # baggage names map to exactly one value. Entries already in +context+
        # are kept unless the header carries the same key.
        #
        # @param [Carrier] carrier The carrier to get the header from
        # @param [optional Context] context Context to be updated with the baggage
        #   extracted from the carrier. Defaults to +Context.current+.
        # @param [optional Getter] getter If the optional getter is provided, it
        #   will be used to read the header from the carrier, otherwise the default
        #   text map getter will be used.
        #
        # @return [Context] context updated with extracted baggage, or the original context
        #   if extraction fails
        def extract(carrier, context: Context.current, getter: Context::Propagation.text_map_getter)
          header = join_fields(carrier, getter)
          return context if header.nil?

          entries = SERIALIZER.decode(header)

          OpenTelemetry::Baggage.build(context: context) do |builder|
            entries.each { |key, entry| builder.set_value(key, entry.value, metadata: entry.metadata) }
          end
        rescue StandardError => e
          OpenTelemetry.logger.debug "Error extracting W3C baggage: #{e.message}"
          context
        end

        # Returns the predefined propagation fields. If your carrier is reused, you
        # should delete the fields returned by this method before calling +inject+.
        #
        # @return [Array<String>] a list of fields that will be used by this propagator.
        def fields
          FIELDS
        end

        private

        SERIALIZER = Serializer.new
        private_constant :SERIALIZER

        # Joins every value of the baggage field into a single list, so that the
        # limits apply to the combination of all fields. Returns nil when the
        # carrier holds no baggage.
        def join_fields(carrier, getter)
          values = field_values(carrier, getter).map(&:to_s).reject(&:empty?)
          return if values.empty?

          values.map(&:b).join(',')
        end

        # Carriers may hold more than one value for the field, either because
        # the getter exposes them all or because the carrier stores them as an
        # array.
        def field_values(carrier, getter)
          values = []
          values = Array(getter.get_all(carrier, BAGGAGE_KEY)).compact if getter.respond_to?(:get_all)
          values = Array(getter.get(carrier, BAGGAGE_KEY)).compact if values.empty?
          values
        end
      end
    end
  end
end
