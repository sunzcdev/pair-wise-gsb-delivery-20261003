# frozen_string_literal: true
module JSONSchemer
  class StrictKeyCollisionError < StandardError
    attr_reader :collisions

    def initialize(collisions)
      @collisions = collisions
      super
    end
  end

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

    # Strict version of `deep_stringify_keys` that records every hash whose keys
    # collapse to the same string after `stringify`. `collisions` is an array of
    # records (see `strict_key_collision!`) shared across the whole recursion so
    # that all colliding instances are reported at once.
    def deep_stringify_keys_strict(obj, collisions, tokens = [])
      case obj
      when Hash
        obj.each_with_object({}) do |(key, value), out|
          string_key = stringify(key)
          out[string_key] = deep_stringify_keys_strict(value, collisions, tokens + [string_key])
        end.tap do
          strict_key_collision!(obj, tokens, collisions)
        end
      when Array
        obj.each_with_index.map do |item, index|
          deep_stringify_keys_strict(item, collisions, tokens + [index.to_s])
        end
      else
        obj
      end
    end

    # Adds a collision record for `hash` when two or more of its original keys
    # stringify to the same value. Keys within a record and records sharing a
    # location are sorted so the raised error never depends on hash insertion
    # order.
    def strict_key_collision!(hash, tokens, collisions)
      grouped = hash.keys.group_by { |key| stringify(key) }
      grouped.each do |string_key, keys|
        next if keys.size == 1
        collisions << {
          'tokens' => tokens.dup,
          'string_key' => string_key,
          'keys' => keys.map { |key| key.inspect }.sort,
          'instance' => hash
        }
      end
    end

    # Prefixes relative collision records with the resolved JSON Pointer of the
    # instance being stringified and raises to abort validation immediately,
    # before any format checks or custom keywords run against the bad object.
    def raise_strict_key_collisions(instance_location, collisions)
      prefix_tokens = Hana::Pointer.parse(Location.resolve(instance_location))
      collisions.each do |collision|
        collision['tokens'] = prefix_tokens + collision.fetch('tokens')
      end
      raise StrictKeyCollisionError.new(collisions)
    end
  end
end
