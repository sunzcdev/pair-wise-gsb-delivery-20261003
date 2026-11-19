# encoding: binary
# frozen_string_literal: true

require "amq/pack"
require "amq/protocol/table"

module AMQ
  module Protocol
    # Primitive AMQP field codecs shared by every protocol version.
    #
    # This layer only knows about primitive wire types; which fields and in
    # which order they appear is decided by a version's description (see
    # AMQ::Protocol::Spec) and emitted by the versioned generator.
    module Codec
      PACK_UINT16 = "n"
      PACK_UINT32 = "N"
      PACK_UINT64 = "Q>"
      PACK_OCTET  = "C"

      BIT_TYPES = %w[bit].freeze

      module_function

      # Encodes one primitive value into a binary String.
      def encode_value(type, value)
        case type.to_s
        when "shortstr" then encode_shortstr(value)
        when "longstr"  then encode_longstr(value)
        when "octet"    then encode_octet(value)
        when "short"    then encode_short(value)
        when "long"     then encode_long(value)
        when "longlong" then encode_longlong(value)
        when "timestamp" then encode_timestamp(value)
        when "table"    then Protocol::Table.encode(value || {})
        when "bit"
          raise ArgumentError, "bit fields must be encoded in a group, not individually"
        else
          raise ArgumentError, "unsupported AMQP primitive type #{type.inspect}"
        end
      end

      # Decodes one primitive value. Returns [value, next_offset].
      def decode_value(type, data, offset)
        case type.to_s
        when "shortstr" then decode_shortstr(data, offset)
        when "longstr"  then decode_longstr(data, offset)
        when "octet"    then [data.getbyte(offset), offset + 1]
        when "short"    then [data.byteslice(offset, 2).unpack1(PACK_UINT16), offset + 2]
        when "long"     then [data.byteslice(offset, 4).unpack1(PACK_UINT32), offset + 4]
        when "longlong" then decode_longlong(data, offset)
        when "timestamp" then decode_timestamp(data, offset)
        when "table"    then decode_table(data, offset)
        when "bit"
          raise ArgumentError, "bit fields must be decoded in a group, not individually"
        else
          raise ArgumentError, "unsupported AMQP primitive type #{type.inspect}"
        end
      end

      # Packs a consecutive group of bit fields into one octet.
      def encode_bits(values)
        byte = 0
        values.each_with_index do |value, bit|
          byte |= (1 << bit) if value && value != 0
        end
        [byte].pack(PACK_OCTET)
      end

      # Unpacks one octet into `count` booleans. Returns [[...], next_offset].
      def decode_bits(data, offset, count)
        return [[], offset] if count.zero?

        byte = data.getbyte(offset) || 0
        [Array.new(count) { |bit| (byte & (1 << bit)) != 0 }, offset + 1]
      end

      def encode_shortstr(value)
        bytes = value.to_s.dup.force_encoding(Encoding::BINARY)
        raise ArgumentError, "shortstr is limited to 255 bytes, got #{bytes.bytesize}" if bytes.bytesize > 255

        [bytes.bytesize].pack(PACK_OCTET) << bytes
      end

      def decode_shortstr(data, offset)
        length = data.getbyte(offset)
        [data.byteslice(offset + 1, length), offset + 1 + length]
      end

      def encode_longstr(value)
        bytes = value.to_s.dup.force_encoding(Encoding::BINARY)
        [bytes.bytesize].pack(PACK_UINT32) << bytes
      end

      def decode_longstr(data, offset)
        length = data.byteslice(offset, 4).unpack1(PACK_UINT32)
        [data.byteslice(offset + 4, length), offset + 4 + length]
      end

      def encode_octet(value)
        [Integer(value)].pack(PACK_OCTET)
      end

      def encode_short(value)
        [Integer(value)].pack(PACK_UINT16)
      end

      def encode_long(value)
        [Integer(value)].pack(PACK_UINT32)
      end

      def encode_longlong(value)
        AMQ::Pack.pack_uint64_big_endian(Integer(value))
      end

      def decode_longlong(data, offset)
        [AMQ::Pack.unpack_uint64_big_endian(data.byteslice(offset, 8)).first, offset + 8]
      end

      def encode_timestamp(value)
        seconds = value.respond_to?(:to_i) ? value.to_i : Integer(value)
        AMQ::Pack.pack_uint64_big_endian(seconds)
      end

      def decode_timestamp(data, offset)
        [Time.at(AMQ::Pack.unpack_uint64_big_endian(data.byteslice(offset, 8)).first), offset + 8]
      end

      def decode_table(data, offset)
        length = data.byteslice(offset, 4).unpack1(PACK_UINT32)
        [Protocol::Table.decode(data.byteslice(offset, length + 4)), offset + length + 4]
      end
    end
  end
end
