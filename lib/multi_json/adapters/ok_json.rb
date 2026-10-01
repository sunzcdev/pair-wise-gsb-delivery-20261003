require "multi_json/adapter"
require "multi_json/convertible_hash_keys"
require "multi_json/vendor/okjson"

module MultiJson
  module Adapters
    class OkJson < Adapter
      include ConvertibleHashKeys
      ParseError = ::MultiJson::OkJson::Error

      # The vendored tokenizer hands over the untouched number text, which is
      # wrapped into a LexicalNumber. No Float is ever involved on this path.
      def self.supports?(option)
        option == :preserve_numbers || super
      end

      def load(string, options = {})
        number = options[:preserve_numbers] ? LexicalNumber.method(:from_lexeme) : nil
        result = ::MultiJson::OkJson.decode("[#{string}]", number: number).first
        options[:symbolize_keys] ? symbolize_keys(result) : result
      rescue ArgumentError # invalid byte sequence in UTF-8
        raise ParseError
      end

      def dump(object, _ = {})
        ::MultiJson::OkJson.valenc(stringify_keys(object))
      end
    end
  end
end
