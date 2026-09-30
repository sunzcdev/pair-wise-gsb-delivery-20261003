module MultiJson
  # Raised when a value cannot be represented in canonical JSON (RFC 8785).
  class CanonicalDumpError < StandardError; end

  # Serializes Ruby objects to canonical JSON as specified by RFC 8785
  # (JSON Canonicalization Scheme, JCS).
  #
  # This serializer is deliberately standalone: it does not consult the
  # currently selected adapter, MultiJson.dump_options, or any other
  # global state, so the produced bytes are stable and suitable for
  # cryptographic signing.
  #
  # Accepted Ruby input types (anything else raises CanonicalDumpError):
  #
  # * NilClass, TrueClass, FalseClass
  # * Integer (limited to the range exactly representable as an IEEE 754
  #   double: -(2**53)..2**53)
  # * Float (finite only; NaN and Infinity are rejected)
  # * String (must contain valid Unicode; transcoded to UTF-8 as needed,
  #   never Unicode-normalized)
  # * Symbol (serialized as its string form)
  # * Array of the above
  # * Hash with String or Symbol keys and values of the above
  module CanonicalDump
    extend self

    # Integers outside this range cannot be represented exactly as
    # IEEE 754 doubles and are rejected instead of being silently
    # rounded to a different value.
    MAX_SAFE_INTEGER = 2**53
    MIN_SAFE_INTEGER = -MAX_SAFE_INTEGER

    # Serializes object to a canonical JSON string (UTF-8 encoded).
    def dump(object)
      output = "".b
      serialize(object, output)
      output.force_encoding(Encoding::UTF_8)
    end

    private

    def serialize(value, output)
      case value
      when Hash
        serialize_object(value, output)
      when Array
        serialize_array(value, output)
      when String
        serialize_string(value, output)
      when Symbol
        serialize_string(value.to_s, output)
      when Integer
        serialize_integer(value, output)
      when Float
        serialize_float(value, output)
      when true
        output << "true"
      when false
        output << "false"
      when nil
        output << "null"
      else
        raise CanonicalDumpError, "cannot serialize #{value.class} to canonical JSON"
      end
    end

    def serialize_object(hash, output)
      entries = hash.map { |key, value| [key_to_name(key), value] }

      seen = {}
      entries.each do |name, _|
        raise CanonicalDumpError, "duplicate object key #{name.inspect}" if seen.key?(name)

        seen[name] = true
      end

      # RFC 8785 sorts properties lexicographically by their UTF-16
      # code units, which differs from UTF-8 byte order for non-BMP
      # characters.
      entries.sort_by! { |name, _| utf16_sort_key(name) }

      output << "{"
      entries.each_with_index do |(name, value), index|
        output << "," if index.positive?
        serialize_string(name, output)
        output << ":"
        serialize(value, output)
      end
      output << "}"
    end

    def serialize_array(array, output)
      output << "["
      array.each_with_index do |element, index|
        output << "," if index.positive?
        serialize(element, output)
      end
      output << "]"
    end

    def serialize_string(string, output)
      utf8 = to_utf8(string)

      output << "\""
      # Bytes below 0x20 plus quote and backslash are the only bytes
      # ever escaped; every multi-byte UTF-8 sequence consists of bytes
      # >= 0x80 and passes through untouched (no normalization).
      utf8.each_byte do |byte|
        case byte
        when 0x22
          output << "\\\""
        when 0x5C
          output << "\\\\"
        when 0x08
          output << "\\b"
        when 0x09
          output << "\\t"
        when 0x0A
          output << "\\n"
        when 0x0C
          output << "\\f"
        when 0x0D
          output << "\\r"
        when 0x00..0x1F
          output << format("\\u%04x", byte)
        else
          output << byte
        end
      end
      output << "\""
    end

    def serialize_integer(value, output)
      unless (MIN_SAFE_INTEGER..MAX_SAFE_INTEGER).cover?(value)
        raise CanonicalDumpError,
          "integer #{value} is outside the exactly representable range #{MIN_SAFE_INTEGER}..#{MAX_SAFE_INTEGER}"
      end

      output << value.to_s
    end

    def serialize_float(value, output)
      raise CanonicalDumpError, "NaN cannot be represented in canonical JSON" if value.nan?
      raise CanonicalDumpError, "Infinity cannot be represented in canonical JSON" if value.infinite?

      output << float_to_ecma_string(value)
    end

    # Formats a finite Float the way ECMAScript's Number::toString does,
    # as required by RFC 8785: the shortest string that round-trips,
    # plain notation for 1e-6 <= |value| < 1e21, exponential otherwise.
    def float_to_ecma_string(value)
      return "0" if value.zero?

      mantissa, exponent = value.to_s.split("e", 2)
      exponent = exponent ? exponent.to_i : 0

      negative = mantissa.start_with?("-")
      mantissa = mantissa[1..] if negative

      integer_part, fraction_part = mantissa.split(".", 2)
      raw_digits = integer_part + fraction_part.to_s
      digits = raw_digits.sub(/\A0+/, "").sub(/0+\z/, "")
      digit_count = digits.length
      # Position of the decimal point relative to the first significant
      # digit: value = digits * 10**(point - digit_count).
      point = integer_part.length - (raw_digits.length - digit_count - trailing_zero_count(raw_digits)) + exponent

      result =
        if digit_count <= point && point <= 21
          digits + ("0" * (point - digit_count))
        elsif point.positive? && point <= 21
          "#{digits[0...point]}.#{digits[point..]}"
        elsif point > -6 && point <= 0
          "0.#{"0" * (-point)}#{digits}"
        elsif digit_count == 1
          "#{digits}#{exponent_suffix(point)}"
        else
          "#{digits[0]}.#{digits[1..]}#{exponent_suffix(point)}"
        end

      negative ? "-#{result}" : result
    end

    def exponent_suffix(point)
      exponent = point - 1
      (exponent.negative? ? "e-" : "e+") + exponent.abs.to_s
    end

    def trailing_zero_count(digits)
      digits.length - digits.sub(/0+\z/, "").length
    end

    def key_to_name(key)
      case key
      when String
        key
      when Symbol
        key.to_s
      else
        raise CanonicalDumpError, "object keys must be Strings or Symbols, got #{key.class}"
      end
    end

    def utf16_sort_key(name)
      to_utf8(name).encode(Encoding::UTF_16BE)
    rescue EncodingError => error
      raise CanonicalDumpError, "object key #{name.inspect} is not valid Unicode: #{error.message}"
    end

    def to_utf8(string)
      # String#encode is a no-op when the string is already UTF-8 and
      # would not catch invalid byte sequences, so validate explicitly.
      unless string.valid_encoding?
        raise CanonicalDumpError, "string #{string.inspect} is not valid #{string.encoding}"
      end

      string.encode(Encoding::UTF_8)
    rescue EncodingError => error
      raise CanonicalDumpError, "string #{string.inspect} is not valid Unicode: #{error.message}"
    end
  end
end
