# frozen_string_literal: true

module Dry
  class Struct
    # Staged builder for incrementally assembling {Dry::Struct} instances.
    #
    # A builder accepts attribute values in multiple steps via {#set}. Every
    # given value is validated (and coerced) against its attribute type right
    # away, while attributes that were not provided yet are simply left blank.
    # Completeness is only verified on {#finish}, which evaluates default
    # values and builds the final struct instance.
    #
    # Nested structs and arrays of structs given as hashes are staged
    # recursively, so gaps deep inside the input are reported with their full
    # path (e.g. `address.city`, `friends[0].zip`).
    #
    # @example
    #   builder = Dry::Struct::Builder.new(User)
    #   builder.set(name: "Jane")
    #   builder.set(address: {city: "NYC"})
    #   builder.finish # => #<User name="Jane" ...>
    class Builder
      # Raised when {#finish} is called while required attributes are missing
      class IncompleteError < Error
        # @return [Array<String>] full paths of the missing attributes
        attr_reader :paths

        def initialize(klass, paths)
          @paths = paths
          super("[#{klass}.builder] missing attributes: #{paths.join(", ")}")
        end
      end

      # @return [Class] the struct class being built
      attr_reader :struct_class

      # @param struct_class [Class] a {Dry::Struct} subclass
      def initialize(struct_class)
        unless struct_class.is_a?(::Class) && struct_class <= Struct
          raise ::ArgumentError, "expected a Dry::Struct class, got #{struct_class.inspect}"
        end

        @struct_class = struct_class
        @values = {}
        @defaults = {}
      end

      # Validate and stage a batch of attributes.
      #
      # Values are checked against their attribute types immediately. The
      # batch is applied atomically: if any value is invalid, an error is
      # raised and everything staged so far is left untouched.
      #
      # @param attributes [Hash{Symbol, String => Object}]
      # @return [self]
      # @raise [Error] when a given value doesn't conform its attribute type
      def set(attributes)
        unless attributes.is_a?(::Hash)
          raise ::ArgumentError, "expected a Hash, got #{attributes.inspect}"
        end

        staged = []
        plain = {}

        attributes.each do |key, value|
          schema_key = schema_key(key)

          if schema_key && value.is_a?(::Hash) && (nested = struct_type(schema_key.type))
            staged << [schema_key.name, staged_child(schema_key.name, nested, value)]
          elsif schema_key && value.is_a?(::Array) && (member = struct_member(schema_key.type))
            staged << [schema_key.name, staged_elements(schema_key.name, member, value)]
          else
            plain[key] = value
          end
        end

        coerce(plain).each { |name, value| staged << [name, value] }
        staged.each { |name, value| @values[name] = value }

        self
      end
      alias_method :update, :set

      # Full paths of the required attributes that are still missing
      #
      # @return [Array<String>]
      def missing
        [].tap { |gaps| scan([], gaps) }
      end

      # Build the struct instance.
      #
      # Evaluates default values for attributes that were never set (each
      # default is evaluated at most once, even across failed attempts) and
      # verifies that every required attribute is present. A failed attempt
      # doesn't invalidate the builder: more attributes can be staged and
      # {#finish} can be called again.
      #
      # @return [Dry::Struct]
      # @raise [IncompleteError] when required attributes are missing
      def finish
        gaps = []
        attributes = resolve([], gaps)

        raise IncompleteError.new(struct_class, gaps) unless gaps.empty?

        struct_class.load(attributes)
      end

      private

      # Staged values are copied on dup so that a failed batch never leaks
      # partially applied nested state into the original builder
      def initialize_copy(source)
        super
        @values = @values.transform_values { |value| copy_staged(value) }
        @defaults = @defaults.dup
      end

      def copy_staged(value)
        case value
        when Builder then value.dup
        when ::Array then value.map { |element| copy_staged(element) }
        else value
        end
      end

      def schema_key(key)
        name = key.respond_to?(:to_sym) ? key.to_sym : key
        struct_class.schema.key(name, nil)
      end

      def struct_type(type)
        type if type.is_a?(::Class) && type <= Struct
      end

      def struct_member(type)
        member = type.member if type.respond_to?(:member)
        member if member.is_a?(::Class) && member <= Struct
      end

      def staged_child(name, klass, hash)
        child = @values[name].is_a?(Builder) ? @values[name].dup : Builder.new(klass)
        child.set(hash)
        child
      rescue Error => exception
        raise Error, "[#{struct_class}.builder] #{name} => #{exception.message}"
      end

      def staged_elements(name, member, array)
        existing = @values[name]

        array.each_with_index.map do |element, index|
          if element.is_a?(::Hash)
            child =
              if existing.is_a?(::Array) && existing[index].is_a?(Builder)
                existing[index].dup
              else
                Builder.new(member)
              end
            begin
              child.set(element)
            rescue Error => exception
              raise Error, "[#{struct_class}.builder] #{name}[#{index}] => #{exception.message}"
            end
            child
          else
            begin
              member.call_unsafe(element)
            rescue Types::CoercionError => exception
              raise Error, "[#{struct_class}.builder] #{name}[#{index}] => #{exception.message}"
            end
          end
        end
      end

      def coerce(plain)
        return {} if plain.empty?

        struct_class.schema.apply(plain, skip_missing: true, resolve_defaults: false)
      rescue Types::CoercionError => exception
        raise Error, "[#{struct_class}.builder] #{exception}"
      end

      # Build the attributes hash, evaluating defaults and collecting gaps
      def resolve(path, gaps)
        struct_class.schema.keys.each_with_object({}) do |key, result|
          name = key.name

          if @values.key?(name)
            result[name] = resolve_staged(@values[name], path + [name], gaps)
          elsif @defaults.key?(name)
            result[name] = @defaults[name]
          elsif key.default?
            result[name] = (@defaults[name] = key.call_unsafe(Core::Constants::Undefined))
          elsif key.required?
            gaps << format_path(path + [name])
          end
        end
      end

      def resolve_staged(value, path, gaps)
        case value
        when Builder
          value.struct_class.load(value.send(:resolve, path, gaps))
        when ::Array
          value.each_with_index.map do |element, index|
            resolve_staged(element, path + [index], gaps)
          end
        else
          value
        end
      end

      # Collect gaps without evaluating defaults or building anything
      def scan(path, gaps)
        struct_class.schema.keys.each do |key|
          name = key.name

          if @values.key?(name)
            scan_staged(@values[name], path + [name], gaps)
          elsif key.required? && !key.default?
            gaps << format_path(path + [name])
          end
        end
      end

      def scan_staged(value, path, gaps)
        case value
        when Builder
          value.send(:scan, path, gaps)
        when ::Array
          value.each_with_index { |element, index| scan_staged(element, path + [index], gaps) }
        end
      end

      def format_path(segments)
        segments.each_with_object(+"") do |segment, path|
          if segment.is_a?(::Integer)
            path << "[#{segment}]"
          else
            path << "." unless path.empty?
            path << segment.to_s
          end
        end
      end
    end
  end
end
