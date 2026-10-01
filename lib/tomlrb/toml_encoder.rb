# frozen_string_literal: true

module Tomlrb
  # Encodes Ruby values produced by {Tomlrb.parse} (or supplied by the
  # caller of {Document}) into TOML syntax.  Only used for newly written
  # values and keys; moved or untouched content always keeps its original
  # spelling.
  module TomlEncoder
    module_function

    def encode(value)
      case value
      when String  then encode_string(value)
      when Integer then value.to_s
      when Float   then encode_float(value)
      when true, false then value.to_s
      when Array   then encode_array(value)
      when Hash    then encode_inline_table(value)
      when Time    then value.iso8601
      when LocalDate, LocalDateTime, LocalTime then value.to_s
      else
        raise EncodeError, "Cannot encode #{value.class} as a TOML value"
      end
    end

    def encode_key(key)
      return encode_integer_key(key) if key.is_a?(Integer)
      string = key.to_s
      if string.match?(/\A[A-Za-z0-9_-]+\z/)
        string
      else
        encode_string(string)
      end
    end

    def encode_integer_key(key)
      string = key.to_s
      raise EncodeError, 'Only bare integer keys are supported' unless string.match?(/\A-?\d+\z/)
      string
    end

    BASIC_ESCAPES = {
      "\\" => '\\\\',
      '"' => '\\"',
      "\b" => '\\b',
      "\t" => '\\t',
      "\n" => '\\n',
      "\f" => '\\f',
      "\r" => '\\r'
    }.freeze

    def encode_string(string)
      escaped = string.gsub(/[\\"\b\t\n\f\r]/) { |char| BASIC_ESCAPES[char] }
      escaped = escaped.gsub(/[\x00-\x1F\x7F]/) do |char|
        format('\\u%04X', char.ord)
      end
      "\"#{escaped}\""
    end

    def encode_float(value)
      return 'nan' if value.nan?
      return value.infinite? == 1 ? 'inf' : '-inf' if value.infinite?
      value.to_s
    end

    def encode_array(array)
      "[#{array.map { |item| encode(item) }.join(', ')}]"
    end

    def encode_inline_table(hash)
      pairs = hash.map { |key, value| "#{encode_key(key)} = #{encode(value)}" }
      "{ #{pairs.join(', ')} }"
    end
  end

  class EncodeError < StandardError; end
end
