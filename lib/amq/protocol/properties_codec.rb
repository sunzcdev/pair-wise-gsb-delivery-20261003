# encoding: binary
# frozen_string_literal: true

require "amq/pack"

require "amq/protocol/table"
require "amq/protocol/constants"
require "amq/protocol/exceptions"

module AMQ
  module Protocol
    # Description-driven encoder/decoder for content header properties.
    #
    # A property description is an ordered list of [name, type] pairs.
    # Properties are announced by 16-bit flags words: bits 15 down to 1 of
    # each word announce up to 15 properties (in description order) and
    # bit 0 is the continuation bit. When the continuation bit is set,
    # another flags word follows. Only properties whose flag is set carry
    # a value on the wire, in description order.
    module PropertiesCodec
      CONTINUATION_BIT = 0x0001
      PROPERTIES_PER_FLAGS_WORD = 15

      SUPPORTED_TYPES = [:shortstr, :longstr, :octet, :timestamp, :table].freeze

      # @param descriptions [Array<Array(Symbol, Symbol)>] ordered [name, type] pairs
      # @param properties [Hash] property name => value, nil values are skipped
      # @return [String] flags words followed by the encoded property values
      def self.encode(descriptions, properties)
        present = properties.reject { |_key, value| value.nil? }
        present.each_key do |key|
          raise UnknownProperty.new(key) unless descriptions.any? { |name, _| name == key }
        end

        words = []
        values = +''
        descriptions.each_with_index do |(name, type), index|
          next unless present.key?(name)

          word_index = index / PROPERTIES_PER_FLAGS_WORD
          words[word_index] = (words[word_index] || 0) | flag_bit(index)
          values << encode_value(type, present[name])
        end
        words = [0] if words.empty?

        result = +''
        last_word = words.length - 1
        words.each_with_index do |word, word_index|
          word ||= 0
          word |= CONTINUATION_BIT if word_index < last_word
          result << [word].pack(PACK_UINT16)
        end
        result << values
        result
      end

      # @param descriptions [Array<Array(Symbol, Symbol)>] ordered [name, type] pairs
      # @param data [String] flags words followed by property values, exactly
      #   one frame worth of bytes: decoding never reads past data.bytesize
      # @return [Hash] property name => decoded value
      def self.decode(descriptions, data)
        offset = 0
        data_length = data.bytesize

        words = []
        loop do
          raise MissingPropertyFlagsContinuation.new(words.length) if data_length - offset < 2

          word = data.byteslice(offset, 2).unpack1(PACK_UINT16)
          offset += 2
          words << word
          break if (word & CONTINUATION_BIT).zero?
        end

        check_for_unknown_flags(descriptions, words)

        properties = {}
        descriptions.each_with_index do |(name, type), index|
          word = words[index / PROPERTIES_PER_FLAGS_WORD]
          next if word.nil? || (word & flag_bit(index)).zero?

          value, offset = decode_value(type, name, data, offset, data_length)
          properties[name] = value
        end

        raise UnexpectedPropertyBytes.new(data_length - offset) if offset < data_length

        properties
      end

      # Bit (within its flags word) announcing the property at the given
      # description index: bit 15 for the first property of a word, bit 1
      # for the fifteenth. Bit 0 is always the continuation bit.
      def self.flag_bit(index)
        1 << (PROPERTIES_PER_FLAGS_WORD - (index % PROPERTIES_PER_FLAGS_WORD))
      end

      def self.check_for_unknown_flags(descriptions, words)
        known_masks = []
        descriptions.each_with_index do |(_name, _type), index|
          word_index = index / PROPERTIES_PER_FLAGS_WORD
          known_masks[word_index] = (known_masks[word_index] || 0) | flag_bit(index)
        end

        words.each_with_index do |word, word_index|
          unknown = word & ~(known_masks[word_index] || 0) & ~CONTINUATION_BIT & 0xFFFF
          raise UnknownPropertyFlags.new(word_index, unknown) unless unknown.zero?
        end
      end

      def self.encode_value(type, value)
        case type
        when :shortstr
          string = value.to_s
          buffer = +''
          buffer << string.bytesize.chr
          buffer << string
        when :longstr
          string = value.to_s
          buffer = +''
          buffer << [string.bytesize].pack(PACK_UINT32)
          buffer << string
        when :octet
          [value].pack(PACK_CHAR)
        when :timestamp
          AMQ::Pack.pack_uint64_big_endian(value.is_a?(Time) ? value.to_i : value)
        when :table
          AMQ::Protocol::Table.encode(value)
        else
          raise ArgumentError.new("Unsupported property type #{type.inspect}")
        end
      end

      # @return [Array(Object, Integer)] the decoded value and the new offset
      def self.decode_value(type, name, data, offset, data_length)
        case type
        when :shortstr
          assert_available(name, type, offset, 1, data_length)
          size = data.getbyte(offset)
          assert_available(name, type, offset + 1, size, data_length)
          [data.byteslice(offset + 1, size), offset + 1 + size]
        when :longstr
          assert_available(name, type, offset, 4, data_length)
          size = data.byteslice(offset, 4).unpack1(PACK_UINT32)
          assert_available(name, type, offset + 4, size, data_length)
          [data.byteslice(offset + 4, size), offset + 4 + size]
        when :octet
          assert_available(name, type, offset, 1, data_length)
          [data.getbyte(offset), offset + 1]
        when :timestamp
          assert_available(name, type, offset, 8, data_length)
          [Time.at(data.byteslice(offset, 8).unpack1(PACK_UINT64_BE)), offset + 8]
        when :table
          assert_available(name, type, offset, 4, data_length)
          size = 4 + data.byteslice(offset, 4).unpack1(PACK_UINT32)
          assert_available(name, type, offset, size, data_length)
          [Table.decode(data.byteslice(offset, size)), offset + size]
        else
          raise ArgumentError.new("Unsupported property type #{type.inspect}")
        end
      end

      def self.assert_available(name, type, offset, required, data_length)
        available = data_length - offset
        raise TruncatedPropertyValue.new(name, type, required, available) if available < required
      end
    end
  end
end
