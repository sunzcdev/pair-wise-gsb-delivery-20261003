# frozen_string_literal: true

class RedisClient
  module CommandBuilder
    extend self

    # Maximum nesting depth accepted by `build`. Nested containers (lists,
    # option hashes/pairs and the structured nodes) beyond this depth are
    # rejected before anything is sent to the server. The limit also guards
    # against self-referential structures.
    MAX_DEPTH = 10

    # Raised when `build` (or `dump`) is called without any argument.
    EmptyCommandError = Class.new(ArgumentError)

    # Raised when the nesting of a structured command exceeds `MAX_DEPTH`.
    DepthError = Class.new(ArgumentError)

    # Base class of the structured argument nodes produced by `scalar`,
    # `option`, `list` and `binary`. Nodes are plain value objects: they never
    # mutate the input they wrap and contain no I/O logic.
    class Node
    end

    # A single, order preserving command token (String, Symbol, Integer or
    # Float). `nil`, `true` and `false` are not valid scalars, their meaning
    # has to be declared explicitly through `option`.
    class Scalar < Node
      attr_reader :value

      def initialize(value)
        unless SCALAR_TYPES.any? { |type| value.is_a?(type) }
          raise TypeError, "Unsupported command argument type: #{value.class}"
        end
        @value = value
      end
    end

    # An order preserving option pair. The emission rules are declared up
    # front instead of being guessed from the value:
    #
    # - `nil` and `false` emit nothing,
    # - `true` emits only the option name (a flag),
    # - any other value emits the option name followed by the value.
    class Pair < Node
      attr_reader :name, :value

      def initialize(name, value)
        @name = name
        @value = value
      end
    end

    # A list of arguments that is expanded in place. An empty list emits no
    # token, which keeps it distinct from an empty string scalar.
    class List < Node
      attr_reader :elements

      def initialize(elements)
        raise TypeError, "list expects an Array, got: #{elements.class}" unless elements.is_a?(Array)
        @elements = elements
      end
    end

    # A verbatim binary body. The string is copied and tagged as binary so its
    # exact bytes are encoded without being reinterpreted.
    class Binary < Node
      attr_reader :value

      def initialize(value)
        raise TypeError, "binary expects a String, got: #{value.class}" unless value.is_a?(String)
        @value = value.dup.force_encoding(Encoding::BINARY)
      end
    end

    SCALAR_TYPES = [String, Symbol, Integer, Float].freeze
    private_constant :SCALAR_TYPES

    # Mark `value` as a scalar token. See `Scalar`.
    def scalar(value)
      Scalar.new(value)
    end

    # Build an order preserving option pair. See `Pair` for the declared
    # `nil`/boolean emission rules.
    def option(name, value)
      Pair.new(name, value)
    end

    # Mark `elements` as a list that must be expanded in place. See `List`.
    def list(elements)
      List.new(elements)
    end

    # Mark `value` as a verbatim binary body. See `Binary`.
    def binary(value)
      Binary.new(value)
    end

    # Structured command entry point.
    #
    # Each element of `command` is interpreted according to its declared
    # shape rather than through `to_s`:
    #
    # - `Scalar`/scalar values (String, Symbol, Integer, Float) emit one token,
    # - `Pair`/Hashes emit option name/value tokens in insertion order,
    # - `List`/Arrays are recursively expanded in place (an empty one emits
    #   nothing),
    # - `Binary` emits the exact bytes of the wrapped string.
    #
    # The input Array and any nested Array/Hash are never modified. Errors are
    # raised before the command could be sent and mention the offending
    # argument path, e.g. `command[2][:ex]`.
    #
    # Returns an Array of binary encoded Strings ready to be RESP encoded.
    def build(command)
      raise TypeError, "build expects an Array, got: #{command.class}" unless command.is_a?(Array)

      tokens = []
      command.each_with_index do |element, index|
        emit_element(element, "[#{index}]", 0, tokens)
      end

      if tokens.empty?
        raise EmptyCommandError, "can't issue an empty redis command"
      end

      tokens
    end

    # Encode a structured command as a RESP array. Token generation (`build`)
    # and RESP encoding are independent concerns: this method is a convenience
    # wrapper that runs both, while `build` and `RESP3.dump` can also be
    # inspected separately.
    def dump(command, buffer = nil)
      require "redis_client/ruby_connection/resp3"
      RESP3.dump(build(command), buffer)
    end

    # Legacy entry point kept for backward compatibility with `#call` and
    # friends. It preserves the historical one level flattening and keyword
    # flag semantics. New code should use `build` with the structured nodes.
    if Symbol.method_defined?(:name)
      def generate(args, kwargs = nil)
        command = args.flat_map do |element|
          case element
          when Hash
            element.flatten
          else
            element
          end
        end

        kwargs&.each do |key, value|
          if value
            if value == true
              command << key.name
            else
              command << key.name << value
            end
          end
        end

        command.map! do |element|
          case element
          when String
            element
          when Symbol
            element.name
          when Integer, Float
            element.to_s
          else
            raise TypeError, "Unsupported command argument type: #{element.class}"
          end
        end

        if command.empty?
          raise ArgumentError, "can't issue an empty redis command"
        end

        command
      end
    else
      def generate(args, kwargs = nil)
        command = args.flat_map do |element|
          case element
          when Hash
            element.flatten
          else
            element
          end
        end

        kwargs&.each do |key, value|
          if value
            if value == true
              command << key.to_s
            else
              command << key.to_s << value
            end
          end
        end

        command.map! do |element|
          case element
          when String
            element
          when Integer, Float, Symbol
            element.to_s
          else
            raise TypeError, "Unsupported command argument type: #{element.class}"
          end
        end

        if command.empty?
          raise ArgumentError, "can't issue an empty redis command"
        end

        command
      end
    end

    private

    def emit_element(element, path, depth, tokens)
      case element
      when Scalar
        tokens << scalar_token(element.value, path)
      when Binary
        tokens << binary_token(element.value, path)
      when Pair
        emit_pair(element, path, depth, tokens)
      when List
        emit_container(element.elements, path, depth, tokens)
      when Hash
        emit_hash(element, path, depth, tokens)
      when Array
        emit_container(element, path, depth, tokens)
      when String, Symbol, Integer, Float
        tokens << scalar_token(element, path)
      else
        raise TypeError, "Unsupported command argument type: #{element.class} at #{path}"
      end
    end

    def emit_container(elements, path, depth, tokens)
      check_depth(depth, path)
      elements.each_with_index do |element, index|
        emit_element(element, "#{path}[#{index}]", depth + 1, tokens)
      end
    end

    def emit_hash(hash, path, depth, tokens)
      check_depth(depth, path)
      hash.each do |key, value|
        emit_pair(Pair.new(key, value), "#{path}[#{key.inspect}]", depth + 1, tokens)
      end
    end

    def emit_pair(pair, path, depth, tokens)
      check_depth(depth, path)

      name = name_token(pair.name, path)
      value = pair.value

      case value
      when nil, false
        # Declared omission: nil and false flags emit nothing.
      when true
        # A boolean flag emits only its name.
        tokens << name
      else
        tokens << name
        emit_element(value, "#{path} (option value)", depth + 1, tokens)
      end
    end

    def check_depth(depth, path)
      if depth >= MAX_DEPTH
        raise DepthError, "command argument nesting too deep (limit #{MAX_DEPTH}) at #{path}"
      end
    end

    def scalar_token(value, path)
      case value
      when String
        value
      when Symbol
        symbol_name(value)
      when Integer, Float
        value.to_s
      else
        raise TypeError, "Unsupported command argument type: #{value.class} at #{path}"
      end
    end

    def name_token(value, path)
      case value
      when String
        value
      when Symbol
        symbol_name(value)
      else
        raise TypeError, "Unsupported command option name type: #{value.class} at #{path}"
      end
    end

    def binary_token(value, path)
      raise TypeError, "binary expects a String, got: #{value.class} at #{path}" unless value.is_a?(String)
      value.dup.force_encoding(Encoding::BINARY)
    end

    if Symbol.method_defined?(:name)
      def symbol_name(symbol)
        symbol.name
      end
    else
      def symbol_name(symbol)
        symbol.to_s
      end
    end
  end
end
