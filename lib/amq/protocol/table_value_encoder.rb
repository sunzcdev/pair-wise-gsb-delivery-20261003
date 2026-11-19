# encoding: binary
# frozen_string_literal: true

require "amq/protocol/type_constants"
require "date"

require "amq/protocol/float_32bit"

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

      # Single source of truth for the AMQP 0-9-1 type tag of a Ruby value.
      # Encoding and encoding planning both go through this method, so a
      # planned type can never diverge from the type actually written.
      def self.type_tag(value)
        case value
        when String, Symbol          then TYPE_STRING
        when Integer                 then TYPE_SIGNED_64BIT
        when Float32Bit              then TYPE_32BIT_FLOAT
        when Float                   then TYPE_64BIT_FLOAT
        when true, false             then TYPE_BOOLEAN
        when Time, DateTime          then TYPE_TIME
        when nil                     then TYPE_VOID
        when Array                   then TYPE_ARRAY
        when Hash                    then TYPE_HASH
        else
          if defined?(BigDecimal) && value.is_a?(BigDecimal)
            TYPE_DECIMAL
          else
            nil
          end
        end
      end


      def self.encode(value)
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
        when DateTime then
          accumulator << TYPE_TIME
          accumulator << [value.to_time.to_i].pack(PACK_INT64_BE)
        when nil then
          accumulator << TYPE_VOID
        when Array then
          accumulator << TYPE_ARRAY
          accumulator << [self.array_size(value)].pack(PACK_UINT32)

          value.each { |v| accumulator << self.encode(v) }
        when Hash then
          accumulator << TYPE_HASH
          accumulator << AMQ::Protocol::Table.encode(value)
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
      end # self.encode(value)


      # Encodes everything but the leading type tag. Used by the table
      # writer, which already owns the (planned) type tag.
      def self.encode_payload(value)
        accumulator = String.new

        case value
        when String then
          accumulator << [value.bytesize].pack(PACK_UINT32)
          accumulator << value.dup.force_encoding(accumulator.encoding)
        when Symbol then
          str = value.to_s
          accumulator << [str.bytesize].pack(PACK_UINT32)
          accumulator << str.force_encoding(accumulator.encoding)
        when Integer then
          accumulator << [value].pack(PACK_INT64_BE)
        when AMQ::Protocol::Float32Bit then
          accumulator << [value.value].pack(PACK_32BIT_FLOAT)
        when Float then
          accumulator << [value].pack(PACK_64BIT_FLOAT)
        when true, false then
          accumulator << (value ? BOOLEAN_TRUE : BOOLEAN_FALSE)
        when Time then
          accumulator << [value.to_i].pack(PACK_INT64_BE)
        when DateTime then
          accumulator << [value.to_time.to_i].pack(PACK_INT64_BE)
        when nil then
          # type tag alone is enough
        when Array then
          raise ArgumentError, "arrays are encoded from their encoding plan"
        when Hash then
          raise ArgumentError, "hashes are encoded from their encoding plan"
        else
          if defined?(BigDecimal) && value.is_a?(BigDecimal)
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
      end # self.encode_payload(value)




      def self.field_value_size(value)
        AMQ::Protocol::Table.plan_value_public(value).size
      end # self.field_value_size(value)


      def self.array_size(value)
        acc = 0
        value.each { |v| acc += self.field_value_size(v) }

        acc
      end # self.array_size(value)

    end # TableValueEncoder

  end # Protocol
end # AMQ
