# encoding: binary
# frozen_string_literal: true

require "amq/protocol/type_constants"
require "date"

require "amq/protocol/float_32bit"
require "amq/protocol/table_codec"

module AMQ
  module Protocol

    class TableValueEncoder

      #
      # Behaviors
      #

      include TypeConstants

      #
      # API
      #

      def self.encode(value, codecs = Table::CodecSet.build)
        accumulator = String.new

        case value
        when String then
          accumulator << TYPE_STRING
          accumulator << [value.bytesize].pack(PACK_UINT32)
          accumulator << value.dup.force_encoding(accumulator.encoding)
        when Symbol then
          str = value.to_s
          accumulator << TYPE_STRING
          accumulator << [str.bytesize].pack(PACK_UINT32)
          accumulator << str.force_encoding(accumulator.encoding)
        when Integer then
          accumulator << TYPE_SIGNED_64BIT
          accumulator << [value].pack(PACK_INT64_BE)
        when AMQ::Protocol::Float32Bit then
          accumulator << TYPE_32BIT_FLOAT
          accumulator << [value.value].pack(PACK_32BIT_FLOAT)
        when Float then
          accumulator << TYPE_64BIT_FLOAT
          accumulator << [value].pack(PACK_64BIT_FLOAT)
        when true, false then
          accumulator << TYPE_BOOLEAN
          accumulator << (value ? BOOLEAN_TRUE : BOOLEAN_FALSE)
        when Time then
          accumulator << TYPE_TIME
          accumulator << [value.to_i].pack(PACK_INT64_BE)
        when nil then
          accumulator << TYPE_VOID
        when Array then
          accumulator << TYPE_ARRAY
          accumulator << [self.array_size(value, codecs)].pack(PACK_UINT32)

          value.each do |v|
            type, encoded = codecs.encode_value(v)
            accumulator << type << encoded
          end
        when Hash then
          accumulator << TYPE_HASH
          accumulator << Table.encode(value, codecs: codecs)
        else
          # We don't want to require these libraries.
          if defined?(BigDecimal) && value.is_a?(BigDecimal)
            accumulator << TYPE_DECIMAL
            if value.exponent < 0
              decimals = -value.exponent
              raw = (value * (decimals ** 10)).to_i
              accumulator << [decimals + 1, raw].pack(PACK_UCHAR_UINT32) # somewhat like floating point
            else
              # per spec, the "decimals" octet is unsigned (!)
              accumulator << [0, value.to_i].pack(PACK_UCHAR_UINT32)
            end
          else
            raise ArgumentError, "Unsupported value #{value.inspect} of type #{value.class.name}"
          end # if
        end # case

        accumulator
      end # self.encode(value, codecs)




      def self.field_value_size(value, codecs = Table::CodecSet.build)
        # Custom codecs own the length rule for values they claim.
        return codecs.value_size(value) if codecs.custom_value?(value)

        # the type tag takes 1 byte
        acc = 1

        case value
        when String then
          acc += (value.bytesize + 4)
        when Integer then
          acc += 8
        when Float then
          acc += 8
        when Time, DateTime then
          acc += 8
        when true, false then
          acc += 1
        when nil then
          # nothing, type tag alone is enough
        when Hash then
          acc += (4 + Table.hash_size(value, codecs: codecs))
        when Array then
          acc += (4 + self.array_size(value, codecs))
        end

        acc
      end # self.field_value_size(value, codecs)


      def self.array_size(value, codecs = Table::CodecSet.build)
        acc = 0
        value.each { |v| acc += self.field_value_size(v, codecs) }

        acc
      end # self.array_size(value, codecs)

    end # TableValueEncoder

  end # Protocol
end # AMQ
