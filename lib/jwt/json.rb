# frozen_string_literal: true

require 'json'

module JWT
  # @api private
  class JSON
    class << self
      def generate(data)
        ::JSON.generate(data)
      end

      def parse(data)
        ::JSON.parse(data)
      end

      def strict_parse(data, max_nesting:)
        parsed = ::JSON.parse(
          data,
          max_nesting: max_nesting,
          object_class: DuplicateKeyRejectingHash,
          create_additions: false
        )
        normalize(parsed)
      end

      private

      def normalize(value)
        case value
        when DuplicateKeyRejectingHash
          value.each_with_object({}) { |(key, item), result| result[key] = normalize(item) }
        when Array
          value.map { |item| normalize(item) }
        else
          value
        end
      end
    end

    DuplicateKeyError = Class.new(::JSON::ParserError)

    class DuplicateKeyRejectingHash < Hash
      DuplicateKey = DuplicateKeyError

      def []=(key, value)
        raise DuplicateKey, key if key?(key)

        super
      end
    end

    private_constant(:DuplicateKeyRejectingHash)
  end
end
