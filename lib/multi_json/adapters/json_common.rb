require "multi_json/adapter"

module MultiJson
  module Adapters
    class JsonCommon < Adapter
      defaults :load, create_additions: false, quirks_mode: true

      # The json gem hands the raw, untouched number text to +decimal_class+,
      # so wrapping LexicalNumber keeps the exact spelling without any Float
      # round-trip. Only versions implementing :decimal_class are accepted.
      def self.supports?(option)
        case option
        when :preserve_numbers
          return @decimal_numbers if defined?(@decimal_numbers)

          @decimal_numbers = probe_decimal_class
        else
          super
        end
      end

      def self.probe_decimal_class
        result = ::JSON.parse("1.5", decimal_class: String)
        result == "1.5" && result.is_a?(String)
      rescue ::JSON::ParserError, ArgumentError, TypeError
        false
      end
      private_class_method :probe_decimal_class

      def load(string, options = {})
        string = string.dup.force_encoding(::Encoding::ASCII_8BIT) if string.respond_to?(:force_encoding)

        json_options = options.dup
        preserve = json_options.delete(:preserve_numbers)
        json_options[:decimal_class] = LexicalNumber if preserve
        json_options[:symbolize_names] = true if json_options.delete(:symbolize_keys)

        result = ::JSON.parse(string, json_options)
        preserve ? wrap_integers(result) : result
      end

      def dump(object, options = {})
        if options.delete(:pretty)
          options.merge!({
            indent: "  ",
            space: " ",
            object_nl: "\n",
            array_nl: "\n"
          })
        end

        object.to_json(options)
      end

      private

      # The json gem only routes decimals/exponents through :decimal_class.
      # Integral JSON numbers come back as exact (arbitrary-precision) Integers,
      # so wrap those too. Their spelling is canonical because the backend does
      # not expose the source text, but no precision is ever lost and Float is
      # never involved.
      def wrap_integers(value)
        case value
        when LexicalNumber then value
        when Integer then LexicalNumber.from_integer(value)
        when Array then value.map { |item| wrap_integers(item) }
        when Hash
          value.each_with_object({}) do |(key, item), object|
            object[key] = wrap_integers(item)
          end
        else
          value
        end
      end
    end
  end
end
