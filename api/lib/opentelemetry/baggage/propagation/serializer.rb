# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'uri'

module OpenTelemetry
  module Baggage
    module Propagation
      # Reads and writes the W3C Baggage header format, including the limits the
      # specification puts on a baggage-string.
      #
      # Lists are parsed member by member: a member that cannot be decoded is
      # dropped without affecting the rest of the header, and an oversized
      # header is truncated at a member separator so that neither a member nor
      # a percent-encoded octet is ever split.
      #
      # @api private
      class Serializer
        # Maximums according to W3C Baggage spec
        MAX_ENTRIES = 180
        MAX_ENTRY_LENGTH = 4096
        MAX_TOTAL_LENGTH = 8192

        # Leading and trailing optional whitespace, as defined by RFC 7230.
        OWS = /\A[ \t]+|[ \t]+\z/n
        # Bytes that are not baggage-octets, and are therefore percent-encoded
        # in a property. The percent sign is a baggage-octet and is left alone,
        # so that escapes received from a peer are not encoded a second time.
        ESCAPE_PROPERTY = /[^\x21\x23-\x2B\x2D-\x3A\x3C-\x5B\x5D-\x7E]/n

        private_constant :MAX_ENTRIES, :MAX_ENTRY_LENGTH, :MAX_TOTAL_LENGTH, :OWS, :ESCAPE_PROPERTY

        # Decodes a baggage-string into key and {Entry} pairs.
        #
        # @param [String] header The baggage-string
        # @return [Array<Array(String, Entry)>]
        def decode(header)
          entries = []
          members(header).each do |member|
            break if entries.size >= MAX_ENTRIES

            entry = decode_member(member)
            entries << entry if entry
          end
          entries
        end

        # Encodes entries into a baggage-string, dropping any entry that would
        # exceed an entry or total limit.
        #
        # @param [Hash{String => Entry}] baggage The entries to encode
        # @return [String]
        def encode(baggage)
          result = +''
          count = 0

          candidates(baggage).each_pair do |encoded_key, entry|
            break if count >= MAX_ENTRIES

            encoded_entry = encode_entry(encoded_key, entry)
            next if encoded_entry.bytesize > MAX_ENTRY_LENGTH
            next if result.bytesize + encoded_entry.bytesize > MAX_TOTAL_LENGTH

            result << encoded_entry << ','
            count += 1
          end

          result.chop
        end

        private

        # Splits the header into members without cutting one in half: a header
        # that exceeds the total limit is truncated at the last separator that
        # fits.
        def members(header)
          return header.split(',') if header.bytesize <= MAX_TOTAL_LENGTH

          window = header.byteslice(0, MAX_TOTAL_LENGTH + 1)
          last_separator = window.byterindex(',')
          return [] unless last_separator

          window.byteslice(0, last_separator).split(',')
        end

        # Returns the decoded key and entry for a member, or nil when the member
        # is not a valid key-value pair. Malformed percent-encoding only
        # invalidates the member that contains it.
        def decode_member(member)
          member = trim(member)
          return unless member.bytesize.between?(1, MAX_ENTRY_LENGTH)

          key_value, metadata = member.split(';', 2)
          key, value = key_value.split('=', 2)
          return unless value

          key = decode_value(key)
          return if key.nil? || key.empty?

          value = decode_value(value)
          return unless value

          [key, Entry.new(value, normalize_metadata(metadata))]
        end

        # Percent-decodes a key or value. Octet sequences that are not valid
        # UTF-8 are replaced with the replacement character, as required by the
        # W3C specification.
        def decode_value(string)
          URI.decode_uri_component(trim(string)).scrub
        rescue ArgumentError
          nil
        end

        # Properties are opaque to OpenTelemetry, but they are preserved so that
        # they can be forwarded. Whitespace around a property and around its
        # equal sign is dropped, and bytes that cannot appear in a property are
        # percent-encoded, so that what is stored in the context is exactly what
        # is written back on inject.
        def normalize_metadata(metadata)
          return if metadata.nil?

          properties = metadata.to_s.b.split(';').filter_map do |property|
            normalize_property(property)
          end
          return if properties.empty?

          properties.join(';').force_encoding(Encoding::UTF_8)
        end

        # A property is either a key or a key-value pair.
        def normalize_property(property)
          key, value = trim(property).split('=', 2).map! { |part| trim(part) }
          return if key.nil? || key.empty?

          normalized = escape_property(key)
          normalized << '=' << escape_property(value) if value
          normalized
        end

        def escape_property(string)
          string.gsub(ESCAPE_PROPERTY) { |byte| format('%%%02X', byte.ord) }
        end

        def trim(string)
          string.b.gsub(OWS, '')
        end

        # Keys that differ only in their Ruby representation, such as a Symbol
        # and a String, encode to the same name. Keeping the last value for each
        # encoded name avoids emitting duplicate members.
        def candidates(baggage)
          baggage.each_pair.with_object({}) do |(key, entry), result|
            encoded_key = escape(key.to_s)
            result[encoded_key] = entry unless encoded_key.empty?
          end
        end

        def encode_entry(encoded_key, entry)
          result = +''
          result << encoded_key << '=' << escape(entry.value.to_s)

          metadata = normalize_metadata(entry.metadata)
          result << ';' << metadata if metadata
          result
        end

        # Percent-encodes a key or value. The percent sign itself is encoded, so
        # that a value always decodes back to exactly the string it was set with.
        def escape(string)
          URI.encode_uri_component(string.to_s)
        end
      end
    end
  end
end
