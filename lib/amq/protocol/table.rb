# encoding: binary
# frozen_string_literal: true

require "amq/protocol/type_constants"
require "amq/protocol/table_codec"
require "amq/protocol/table_value_encoder"
require "amq/protocol/table_value_decoder"

module AMQ
  module Protocol
    class Table

      #
      # Behaviors
      #

      include TypeConstants

      # Pack format string
      PACK_UINT32_BE = 'N'.freeze

      #
      # API
      #

      class InvalidTableError < StandardError
        def initialize(key, value)
          super("Invalid table value on key #{key}: #{value.inspect} (#{value.class})")
        end
      end


      def self.encode(table, codecs: CodecSet.build)
        buffer = +''

        table ||= {}

        table.each do |key, value|
          key = key.to_s # it can be a symbol as well
          buffer << key.bytesize.chr << key

          type, encoded_value = codecs.encode_value(value)
          buffer << type << encoded_value.force_encoding(buffer.encoding)
        end

        [buffer.bytesize].pack(PACK_UINT32_BE) << buffer
      end




      def self.decode(data, codecs: CodecSet.build)
        decode_fields(data, 4, data.unpack1(PACK_UINT32_BE), codecs).first
      end

      # Decodes one field table that begins at +offset+ in +data+, reading it in
      # place so that nested tables and arrays share the same absolute offset
      # space and the same frozen codec view as the enclosing table.
      def self.decode_fields(data, offset, table_length, codecs)
        table = {}
        limit = offset + table_length

        while offset < limit
          key, offset  = decode_table_key(data, offset)
          type, offset = TableValueDecoder.decode_value_type(data, offset)
          table[key], offset = codecs.decode_value(data, offset, type, limit)
        end

        [table, limit]
      end


      def self.length(data)
        data.unpack1(PACK_UINT32_BE)
      end


      def self.hash_size(value, codecs: CodecSet.build)
        acc = 0
        value.each do |k, v|
          acc += (1 + k.to_s.bytesize)
          acc += codecs.value_size(v)
        end

        acc
      end


      def self.decode_table_key(data, offset)
        key_length = data.getbyte(offset)
        offset += 1
        key = data.byteslice(offset, key_length)
        offset += key_length

        [key, offset]
      end



    end # Table
  end # Protocol
end # AMQ
