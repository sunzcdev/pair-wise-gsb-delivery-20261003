# frozen_string_literal: true

require 'json'

module JWT
  # @api private
  class JSON
    class DuplicateKeyError < StandardError; end

    class StrictHash < ::Hash
      def []=(key, value)
        raise JWT::JSON::DuplicateKeyError, "Duplicate key: #{key}" if key?(key)

        super
      end
    end

    private_constant(:StrictHash)

    class << self
      def generate(data)
        ::JSON.generate(data)
      end

      def parse(data)
        ::JSON.parse(data)
      end

      def parse_strict(data, max_nesting_depth:)
        normalize(
          ::JSON.parse(
            data,
            max_nesting: max_nesting_depth,
            object_class: StrictHash
          )
        )
      end

      def normalize(value)
        case value
        when ::Hash
          value.each_with_object({}) do |(key, nested_value), result|
            result[key] = normalize(nested_value)
          end
        when ::Array
          value.map { |element| normalize(element) }
        else
          value
        end
      end
    end
  end
end
