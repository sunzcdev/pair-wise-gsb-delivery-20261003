# frozen_string_literal: true
module JSONSchemer
  # Redacts instance values from validation output using exact JSON Pointers.
  #
  # Validation always runs against the real instance; redacted values are only
  # generated in the output layer. A pointer to an object or array hides its
  # entire subtree.
  module Redactor
    REDACTED = '[REDACTED]'

    module Null
      class << self
        def redact_instance(instance, _instance_location)
          instance
        end

        def redact_details(details, _instance_location, _custom_keyword)
          details
        end
      end
    end

    NULL = Null

    class << self
      def build(sensitive_paths)
        sensitive_paths ? Sensitive.new(sensitive_paths) : NULL
      end
    end

    class Sensitive
      def initialize(paths)
        @paths = paths.map do |path|
          Hana::Pointer.parse(path)
        rescue Hana::Pointer::FormatError
          raise InvalidSensitivePath, path
        end
      end

      def redact_instance(instance, instance_location)
        return Redactor::REDACTED if hidden_location?(instance_location)
        descendants = if instance_location.empty?
          @paths
        else
          descendant_paths(instance_location)
        end
        return instance if descendants.empty?
        redact_tree(instance, descendants)
      end

      def redact_details(details, instance_location, custom_keyword)
        return details unless custom_keyword
        return details unless details.is_a?(Hash) && details.key?('result')
        return details unless hidden_location?(instance_location) || descendant_paths(instance_location).any?
        redacted = details.dup
        redacted['result'] = Redactor::REDACTED
        redacted
      end

      private

      def hidden_location?(location_tokens)
        @paths.any? do |tokens|
          location_tokens.size >= tokens.size && location_tokens.first(tokens.size) == tokens
        end
      end

      def descendant_paths(location_tokens)
        @paths.filter_map do |tokens|
          next unless tokens.size > location_tokens.size && tokens.first(location_tokens.size) == location_tokens
          tokens[location_tokens.size..]
        end
      end

      def redact_tree(value, paths)
        return Redactor::REDACTED if paths.any?(&:empty?)

        case value
        when Hash
          value.each_with_object({}) do |(key, child), out|
            child_paths = paths.filter_map do |tokens|
              tokens[1..] if tokens.first == key
            end
            out[key] = child_paths.empty? ? child : redact_tree(child, child_paths)
          end
        when Array
          value.each_with_index.map do |child, index|
            key = index.to_s
            child_paths = paths.filter_map do |tokens|
              tokens[1..] if tokens.first == key
            end
            child_paths.empty? ? child : redact_tree(child, child_paths)
          end
        else
          value
        end
      end
    end
  end
end
