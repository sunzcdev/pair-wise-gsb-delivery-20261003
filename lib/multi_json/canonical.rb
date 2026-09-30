require "multi_json/canonicalization_error"

module MultiJson
  # RFC 8785 (JSON Canonicalization Scheme) serialization.
  #
  # This serializer is intentionally independent of the configured adapter
  # and its options. It accepts only the subset of Ruby values that map to
  # JSON without losing information:
  #
  # * +Hash+ with +String+ keys
  # * +Array+
  # * +String+ containing valid UTF-8 (no Unicode normalization is applied)
  # * +Integer+ in the inclusive range from <tt>-(2**53 - 1)</tt> to
  #   <tt>2**53 - 1</tt>, the "safe integer" range recommended by RFC 8785
  # * finite +Float+
  # * +true+, +false+, and +nil+
  #
  # Anything else, including integers outside that range, symbols, custom
  # objects, non-finite floats, non-string keys, and strings that are not
  # valid Unicode, raises CanonicalizationError.
  module Canonical
    UTF_8 = Encoding::UTF_8
    US_ASCII = Encoding::US_ASCII
    UTF_16BE = Encoding::UTF_16BE

    MAX_SAFE_INTEGER = 2**53 - 1
    ESCAPED_CHARACTERS = {
      "\b" => "\\b",
      "\t" => "\\t",
      "\n" => "\\n",
      "\f" => "\\f",
      "\r" => "\\r",
      '"' => "\\\"",
      "\\" => "\\\\"
    }.freeze

    module_function

    def dump(object)
      output = String.new(encoding: UTF_8)
      serialize(output, object)
      output
    end

    def serialize(output, object)
      case object
      when Hash
        serialize_hash(output, object)
      when Array
        serialize_array(output, object)
      when String
        serialize_string(output, object)
      when Integer
        serialize_integer(output, object)
      when Float
        serialize_float(output, object)
      when true
        output << "true"
      when false
        output << "false"
      when nil
        output << "null"
      else
        raise CanonicalizationError, "unsupported type for canonical JSON: #{type_name(object)}"
      end
    end

    def type_name(object)
      object.class.name
    rescue StandardError
      "BasicObject"
    end

    def serialize_hash(output, object)
      pairs = object.map do |key, value|
        unless key.is_a?(String)
          key_type =
            begin
              key.class
            rescue StandardError
              BasicObject
            end
          raise CanonicalizationError,
            "hash keys must be strings, got #{key_type}"
        end
        validate_string!(key)
        [key, value]
      end
      pairs.sort_by! { |key, _| key.encode(UTF_16BE) }

      output << "{"
      pairs.each_with_index do |(key, value), index|
        output << "," unless index.zero?
        serialize_string(output, key)
        output << ":"
        serialize(output, value)
      end
      output << "}"
    end

    def serialize_array(output, object)
      output << "["
      object.each_with_index do |value, index|
        output << "," unless index.zero?
        serialize(output, value)
      end
      output << "]"
    end

    def serialize_string(output, string)
      validate_string!(string)
      output << '"'
      string.each_char do |character|
        escaped = ESCAPED_CHARACTERS[character]
        if escaped
          output << escaped
        elsif character.ord <= 0x1f
          output << "\\u#{character.ord.to_s(16).rjust(4, "0")}"
        else
          output << character
        end
      end
      output << '"'
    end

    def serialize_integer(output, integer)
      if integer.abs > MAX_SAFE_INTEGER
        raise CanonicalizationError,
          "integer #{integer} is outside the exact JCS integer range " \
            "(#{-MAX_SAFE_INTEGER}..#{MAX_SAFE_INTEGER})"
      end
      output << integer.to_s
    end

    def serialize_float(output, float)
      unless float.finite?
        raise CanonicalizationError, "NaN and Infinity are not valid JSON numbers"
      end
      output << format_float(float)
    end

    # Formats a finite Float using the shortest round-trippable decimal
    # representation produced by Ruby, then applies the fixed/exponential
    # notation boundaries required by ECMAScript Number.prototype.toString,
    # which RFC 8785 uses for number serialization.
    def format_float(float)
      return "0" if float.zero?

      negative = float.negative?
      representation = float.to_s.delete_prefix("-")
      digits, first_digit_index = parse_float_representation(representation)

      formatted = fixed_notation(digits, first_digit_index) ||
        exponential_notation(digits, first_digit_index)
      negative ? "-#{formatted}" : formatted
    end

    def parse_float_representation(representation)
      if representation.include?("e")
        mantissa, exponent = representation.split("e")
        digits = mantissa.delete(".")
        [significant_digits(digits), exponent.to_i]
      else
        integer_part, fractional_part = representation.split(".")
        digits = integer_part + fractional_part.to_s
        leading_zeros = digits[/\A0*/].length
        first_digit_index = integer_part.length - leading_zeros - 1
        [significant_digits(digits), first_digit_index]
      end
    end

    def significant_digits(digits)
      digits.sub(/\A0+/, "").sub(/0+\z/, "")
    end

    def fixed_notation(digits, first_digit_index)
      digit_count = digits.length
      integer_digits = first_digit_index + 1

      if integer_digits >= digit_count && integer_digits <= 21
        digits + "0" * (integer_digits - digit_count)
      elsif integer_digits.positive? && integer_digits <= 21
        digits[0, integer_digits] + "." + digits[integer_digits..]
      elsif integer_digits <= 0 && integer_digits >= -5
        "0." + ("0" * -integer_digits) + digits
      end
    end

    def exponential_notation(digits, first_digit_index)
      mantissa =
        if digits.length == 1
          digits
        else
          "#{digits[0]}.#{digits[1..]}"
        end
      exponent = first_digit_index
      sign = exponent.negative? ? "-" : "+"
      "#{mantissa}e#{sign}#{exponent.abs}"
    end

    def validate_string!(string)
      encoding = string.encoding
      unless (encoding == UTF_8 || encoding == US_ASCII) && string.valid_encoding?
        raise CanonicalizationError,
          "strings must be valid UTF-8, got #{encoding} data"
      end
    end
  end

  # Serializes a Ruby object according to RFC 8785 (JSON Canonicalization
  # Scheme). Unlike dump, the result never depends on the current adapter or
  # its options; unsupported inputs raise CanonicalizationError.
  def canonical_dump(object)
    Canonical.dump(object)
  end
end
