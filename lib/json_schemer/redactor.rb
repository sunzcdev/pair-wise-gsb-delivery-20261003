# frozen_string_literal: true
module JSONSchemer
  # Builds a redacted copy of a validated instance for diagnostic output.
  # `paths` are exact JSON Pointers (RFC 6901); hiding an object or array hides
  # its entire subtree. Validation always runs against the original instance —
  # redaction only affects output.
  class Redactor
    REDACTED = '[Redacted]'
    ARRAY_INDEX_REGEX = /\A(?:0|[1-9]\d*)\z/
    UNRESOLVED = Object.new.freeze

    def initialize(instance, paths)
      @tokens = paths.map { |path| Hana::Pointer.parse(path) }.uniq
      @sensitive_values = Set.new
      @tokens.each do |tokens|
        collect_sensitive_values(instance, tokens, 0)
      end
      @instance = redact_node(instance, [])
    end

    attr_reader :instance

    def instance_at(location)
      tokens = Hana::Pointer.parse(location)
      return @instance if tokens.empty?
      current = @instance
      tokens.each do |token|
        return REDACTED if current.equal?(REDACTED)
        if current.is_a?(Array) && ARRAY_INDEX_REGEX.match?(token)
          index = token.to_i
          return nil unless index < current.size
          current = current.fetch(index)
        elsif current.is_a?(Hash)
          return nil unless current.key?(token)
          current = current.fetch(token)
        else
          return nil
        end
      end
      current
    end

    # Redacts values derived from the instance (annotations, details) that were
    # copied outside the instance tree. Containers are redacted recursively;
    # scalar values equal to a hidden leaf are replaced.
    def redact(value)
      case value
      when Hash
        value.transform_values { |item| redact(item) }
      when Array
        value.map { |item| redact(item) }
      when String, Numeric, Symbol
        @sensitive_values.include?(value) ? REDACTED : value
      else
        value
      end
    end

    def leaked?(message)
      message = message.to_s
      @sensitive_values.any? { |value| message.include?(value.to_s) }
    end

  private

    def redacted_here?(tokens)
      @tokens.any? { |path| path == tokens }
    end

    def descend(path, token)
      path + [token]
    end

    def collect_sensitive_values(node, tokens, index)
      if index == tokens.size
        collect_leaves(node)
        return
      end
      node = step(node, tokens[index])
      collect_sensitive_values(node, tokens, index + 1) unless node.equal?(UNRESOLVED)
    end

    def collect_leaves(node)
      case node
      when Hash
        node.each_value { |item| collect_leaves(item) }
      when Array
        node.each { |item| collect_leaves(item) }
      when String, Numeric, Symbol
        @sensitive_values.add(node)
      end
    end

    def step(node, token)
      if node.is_a?(Array) && ARRAY_INDEX_REGEX.match?(token)
        index = token.to_i
        index < node.size ? node.fetch(index) : UNRESOLVED
      elsif node.is_a?(Hash)
        node.key?(token) ? node.fetch(token) : UNRESOLVED
      else
        UNRESOLVED
      end
    end

    def redact_node(node, path)
      return REDACTED if redacted_here?(path)
      case node
      when Hash
        node.each_with_object({}) do |(key, item), out|
          out[key] = redact_node(item, descend(path, key))
        end
      when Array
        node.each_with_index.map do |item, index|
          redact_node(item, descend(path, index.to_s))
        end
      else
        node
      end
    end
  end
end
