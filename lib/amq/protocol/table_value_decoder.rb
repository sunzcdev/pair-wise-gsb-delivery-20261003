# encoding: binary
# frozen_string_literal: true

require "amq/protocol/type_constants"
require "amq/protocol/float_32bit"
require "amq/protocol/table_codec"

module AMQ
  module Protocol

    class TableValueDecoder

      #
      # Behaviors
      #

      include TypeConstants

      # Pack format strings use explicit endianness (available as of Ruby 1.9.3)
      PACK_UINT32_BE   = 'N'.freeze
      PACK_INT64_BE    = 'q>'.freeze
      PACK_INT16_BE    = 's>'.freeze
      PACK_UINT64_BE   = 'Q>'.freeze
      PACK_FLOAT32     = 'f'.freeze  # single precision float (native endian, matches encoder)
      PACK_FLOAT64     = 'G'.freeze  # big-endian double precision float
      PACK_UCHAR_UINT32 = 'CN'.freeze

      #
      # Built-in codec registry
      #

      def self.built_in_codecs
        @built_in_codecs ||= begin
          # Registry decoders all take (data, offset, codecs); leaf decoders
          # simply ignore the codec set while nested ones thread it through.
          leaf = ->(decoder) { ->(data, offset, _codecs) { decoder.call(data, offset) } }

          {
            TYPE_STRING       => Table::Codec.new(type: TYPE_STRING,       decoder: leaf.call(method(:decode_string))),
            TYPE_BYTE_ARRAY   => Table::Codec.new(type: TYPE_BYTE_ARRAY,   decoder: leaf.call(method(:decode_string))),
            TYPE_INTEGER      => Table::Codec.new(type: TYPE_INTEGER,      decoder: leaf.call(method(:decode_integer))),
            TYPE_DECIMAL      => Table::Codec.new(type: TYPE_DECIMAL,      decoder: leaf.call(method(:decode_big_decimal))),
            TYPE_TIME         => Table::Codec.new(type: TYPE_TIME,         decoder: leaf.call(method(:decode_time))),
            TYPE_HASH         => Table::Codec.new(type: TYPE_HASH,         decoder: ->(data, offset, codecs) { decode_hash(data, offset, codecs) }),
            TYPE_BOOLEAN      => Table::Codec.new(type: TYPE_BOOLEAN,      decoder: leaf.call(method(:decode_boolean))),
            TYPE_BYTE         => Table::Codec.new(type: TYPE_BYTE,         decoder: leaf.call(method(:decode_byte))),
            TYPE_SIGNED_16BIT => Table::Codec.new(type: TYPE_SIGNED_16BIT, decoder: leaf.call(method(:decode_short))),
            TYPE_SIGNED_64BIT => Table::Codec.new(type: TYPE_SIGNED_64BIT, decoder: leaf.call(method(:decode_long))),
            TYPE_32BIT_FLOAT  => Table::Codec.new(type: TYPE_32BIT_FLOAT,  decoder: leaf.call(method(:decode_32bit_float))),
            TYPE_64BIT_FLOAT  => Table::Codec.new(type: TYPE_64BIT_FLOAT,  decoder: leaf.call(method(:decode_64bit_float))),
            TYPE_VOID         => Table::Codec.new(type: TYPE_VOID,         decoder: ->(data, offset, _codecs) { [nil, offset] }),
            TYPE_ARRAY        => Table::Codec.new(type: TYPE_ARRAY,        decoder: ->(data, offset, codecs) { decode_array(data, offset, codecs) })
          }.freeze
        end
      end

      #
      # API
      #

      def self.decode_array(data, initial_offset, codecs = Table::CodecSet.build)
        array_length = data.byteslice(initial_offset, 4).unpack1(PACK_UINT32_BE)

        ary    = []
        offset = initial_offset + 4
        limit  = initial_offset + 4 + array_length

        while offset < limit
          type, offset = decode_value_type(data, offset)
          value, offset = codecs.decode_value(data, offset, type, limit)
          ary << value
        end

        [ary, limit]
      end


      def self.decode_string(data, offset)
        length = data.byteslice(offset, 4).unpack1(PACK_UINT32_BE)
        offset += 4
        v = data.byteslice(offset, length)
        offset += length

        [v, offset]
      end


      def self.decode_integer(data, offset)
        v = data.byteslice(offset, 4).unpack1(PACK_UINT32_BE)
        offset += 4

        [v, offset]
      end


      def self.decode_long(data, offset)
        v = data.byteslice(offset, 8).unpack1(PACK_INT64_BE)
        offset += 8
        [v, offset]
      end


      def self.decode_big_decimal(data, offset)
        decimals, raw = data.byteslice(offset, 5).unpack(PACK_UCHAR_UINT32)
        offset += 5
        v = BigDecimal(raw.to_s) * (BigDecimal(TEN) ** -decimals)

        [v, offset]
      end


      def self.decode_time(data, offset)
        timestamp = data.byteslice(offset, 8).unpack1(PACK_UINT64_BE)
        v = Time.at(timestamp)
        offset += 8

        [v, offset]
      end


      def self.decode_boolean(data, offset)
        byte = data.getbyte(offset)
        offset += 1
        [(byte == 1), offset]
      end


      def self.decode_32bit_float(data, offset)
        v = data.byteslice(offset, 4).unpack1(PACK_FLOAT32)
        offset += 4

        [v, offset]
      end


      def self.decode_64bit_float(data, offset)
        v = data.byteslice(offset, 8).unpack1(PACK_FLOAT64)
        offset += 8

        [v, offset]
      end


      def self.decode_value_type(data, offset)
        [data.byteslice(offset, 1), offset + 1]
      end


      def self.decode_hash(data, offset, codecs = Table::CodecSet.build)
        length = data.byteslice(offset, 4).unpack1(PACK_UINT32_BE)
        # Nested tables are decoded in place and use the very same codec view.
        v, offset = Table.decode_fields(data, offset + 4, length, codecs)

        [v, offset]
      end


      # Decodes/Converts a byte value from the data at the provided offset.
      #
      # @param [String] data - Binary data string
      # @param [Integer] offset - The offset from which to read the byte
      # @return [Array] - The Integer value and new offset pair
      def self.decode_byte(data, offset)
        [data.getbyte(offset), offset + 1]
      end


      def self.decode_short(data, offset)
        v = data.byteslice(offset, 2).unpack1(PACK_INT16_BE)
        offset += 2
        [v, offset]
      end
    end # TableValueDecoder
  end # Protocol
end # AMQ
