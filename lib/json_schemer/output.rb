# frozen_string_literal: true
module JSONSchemer
  module Output
    FRAGMENT_ENCODE_REGEX = /[^\w?\/:@\-.~!$&'()*+,;=]/

    attr_reader :keyword, :schema

    def x_error
      return @x_error if defined?(@x_error)
      @x_error = schema.parsed['x-error']&.message(error_key)
    end

  private

    def result(instance, instance_location, keyword_location, valid, nested = nil, type: nil, annotation: nil, details: nil, ignore_nested: false)
      Result.new(self, instance, instance_location, keyword_location, valid, nested, type, annotation, details, ignore_nested, valid ? 'annotations' : 'errors')
    end

    def escaped_keyword
      @escaped_keyword ||= Location.escape_json_pointer_token(keyword)
    end

    def join_location(location, keyword)
      Location.join(location, keyword)
    end

    def fragment_encode(location)
      Format.percent_encode(location, FRAGMENT_ENCODE_REGEX)
    end

    # :nocov:
    if Symbol.method_defined?(:name)
      def stringify(key)
        key.is_a?(Symbol) ? key.name : key.to_s
      end
    else
      def stringify(key)
        key.to_s
      end
    end
    # :nocov:

    def deep_stringify_keys(obj)
      case obj
      when Hash
        obj.each_with_object({}) do |(key, value), out|
          out[stringify(key)] = deep_stringify_keys(value)
        end
      when Array
        obj.map { |item| deep_stringify_keys(item) }
      else
        obj
      end
    end

    def deep_stringify_keys_strict(obj, location = Location.root)
      case obj
      when Hash
        original_keys = {}
        obj.each_with_object({}) do |(key, value), out|
          string_key = stringify(key)
          if out.key?(string_key)
            raise KeyCollision.new(Location.resolve(location), [original_keys.fetch(string_key), key])
          end
          original_keys[string_key] = key
          out[string_key] = deep_stringify_keys_strict(value, Location.join(location, string_key))
        end
      when Array
        obj.map.with_index do |item, index|
          deep_stringify_keys_strict(item, Location.join(location, index.to_s))
        end
      else
        obj
      end
    end

    def stringify_instance(instance, location = Location.root, strict_keys = false)
      strict_keys ? deep_stringify_keys_strict(instance, location) : deep_stringify_keys(instance)
    end
  end
end
