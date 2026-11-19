# encoding: binary
# frozen_string_literal: true

require "amq/protocol/constants"
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

      # AMQP 0-9-1 decimal field limits: a scale octet followed by
      # a signed 32-bit big-endian integer coefficient.
      DECIMAL_MAX_SCALE         = 255
      DECIMAL_MIN_COEFFICIENT   = -(2**31)
      DECIMAL_MAX_COEFFICIENT   = (2**31) - 1
      DECIMAL_COEFFICIENT_RANGE = (DECIMAL_MIN_COEFFICIENT..DECIMAL_MAX_COEFFICIENT).freeze

      # Supported BigDecimal encoding modes:
      #
      # * :legacy — historical behavior, kept for backwards compatibility.
      #   May silently round or truncate values that do not fit the wire format.
      # * :exact  — encodes the value as a (scale, integer coefficient) pair
      #   exactly as defined by the protocol and raises ArgumentError when the
      #   value cannot be represented losslessly (non-finite values, scale
      #   above 255, coefficient outside the signed 32-bit range).
      BIG_DECIMAL_MODES = [:legacy, :exact].freeze

      # Returns the current BigDecimal encoding mode (:legacy by default).
      def self.big_decimal_mode
        @big_decimal_mode ||= :legacy
      end

      # Selects the BigDecimal encoding mode. Nested field tables and arrays
      # go through this same encoder, so the mode applies to them as well.
      def self.big_decimal_mode=(mode)
        mode = mode.to_sym if mode.respond_to?(:to_sym)
        unless BIG_DECIMAL_MODES.include?(mode)
          raise ArgumentError, "unknown BigDecimal encoding mode #{mode.inspect}, expected one of #{BIG_DECIMAL_MODES.inspect}"
        end

        @big_decimal_mode = mode
      end

      #
      # API
      #

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
            self.encode_big_decimal(value, accumulator)
          else
            raise ArgumentError, "Unsupported value #{value.inspect} of type #{value.class.name}"
          end # if
        end # case

        accumulator
      end # self.encode(value)


      # Encodes a BigDecimal as an AMQP decimal field (type tag, scale octet,
      # 32-bit coefficient) into the given accumulator.
      def self.encode_big_decimal(value, accumulator)
        accumulator << TYPE_DECIMAL

        if self.big_decimal_mode == :exact
          scale, coefficient = self.big_decimal_scale_and_coefficient(value)
          accumulator << [scale, coefficient].pack(PACK_UCHAR_INT32_BE)
        else
          # Historical, potentially lossy behavior, kept for backwards
          # compatibility. Prefer the :exact mode for new code.
          if value.exponent < 0
            decimals = -value.exponent
            raw = (value * (decimals ** 10)).to_i
            accumulator << [decimals + 1, raw].pack(PACK_UCHAR_UINT32) # somewhat like floating point
          else
            # per spec, the "decimals" octet is unsigned (!)
            accumulator << [0, value.to_i].pack(PACK_UCHAR_UINT32)
          end
        end

        accumulator
      end # self.encode_big_decimal(value, accumulator)


      # Decomposes a finite BigDecimal into the (scale, coefficient) pair used
      # by the AMQP decimal type, such that value == coefficient * 10**(-scale).
      #
      # Raises ArgumentError when the value cannot be represented losslessly:
      # non-finite values (Infinity, -Infinity, NaN), a scale that does not fit
      # the protocol octet, or a coefficient outside the signed 32-bit range.
      #
      # @return [Array(Integer, Integer)] scale and integer coefficient
      def self.big_decimal_scale_and_coefficient(value)
        unless value.finite?
          raise ArgumentError, "Cannot encode non-finite BigDecimal value #{value.to_s} as an AMQP decimal field"
        end

        sign, digits, _base, exponent = value.split
        coefficient = sign * digits.to_i
        scale       = digits.length - exponent

        if coefficient.zero?
          # zero has no significant digits; encode it canonically as 0E0
          scale = 0
        elsif scale < 0
          # e.g. 1E+3: shift the exponent into the coefficient, which is exact
          coefficient *= 10**(-scale)
          scale = 0
        end

        if scale > DECIMAL_MAX_SCALE
          raise ArgumentError, "Cannot encode #{value.to_s} as an AMQP decimal field: scale #{scale} does not fit into an octet"
        end

        unless DECIMAL_COEFFICIENT_RANGE.cover?(coefficient)
          raise ArgumentError, "Cannot encode #{value.to_s} as an AMQP decimal field: coefficient #{coefficient} is outside the signed 32-bit range"
        end

        [scale, coefficient]
      end # self.big_decimal_scale_and_coefficient(value)




      def self.field_value_size(value)
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
          acc += (4 + Table.hash_size(value))
        when Array then
          acc += (4 + self.array_size(value))
        else
          # We don't want to require these libraries.
          if defined?(BigDecimal) && value.is_a?(BigDecimal)
            # scale octet + 32-bit coefficient, same as on the wire
            acc += 5
          end
        end

        acc
      end # self.field_value_size(value)


      def self.array_size(value)
        acc = 0
        value.each { |v| acc += self.field_value_size(v) }

        acc
      end # self.array_size(value)

    end # TableValueEncoder

  end # Protocol
end # AMQ
