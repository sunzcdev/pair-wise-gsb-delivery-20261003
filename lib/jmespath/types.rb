# frozen_string_literal: true
module JMESPath
  # Lightweight structural shapes used by {JMESPath.check_types} to catch
  # obvious function type errors before runtime.
  #
  # This is intentionally *not* a JSON Schema engine. Shapes only model
  # what is needed to type-check a query: scalars, objects with optional
  # keys, arrays with element types, null and unions. Anything left
  # undescribed is `any` and remains the responsibility of the existing
  # runtime checks.
  #
  # Shapes can be built with the factory methods or from plain Ruby data:
  #
  #   JMESPath::Types.object(
  #     'name'  => :string,
  #     'email' => [:maybe, :string],        # optional field
  #     'tags'  => [:string],                # array of strings
  #     'score' => [:or, :number, :null],    # union
  #   )
  #
  #   JMESPath::Types.build(
  #     'name'   => :string,
  #     'email?' => :string,                 # "?" suffix marks optional
  #   )
  module Types
    # Base class of all shapes. Types are immutable value objects; `==`
    # compares structure and ignores {#note} (provenance only).
    class Type
      # @return [String,nil] explains how this type was derived, used to
      #   build the "reason" part of compile-time errors.
      attr_reader :note

      def initialize(note = nil)
        @note = note
      end

      def |(other)
        Types.union(self, other)
      end

      def &(other)
        Types.intersect(self, other)
      end

      def with_note(note)
        copy = dup
        copy.instance_variable_set(:@note, note)
        copy
      end

      def nullable?
        false
      end

      # The provenance note of this type, or of the nearest wrapped type
      # that carries one. Used to explain compile-time errors.
      def explanation
        @note
      end

      def eql?(other)
        self == other
      end

      def hash
        to_s.hash
      end

      def inspect
        to_s
      end
    end

    # Unknown shape. Intersects with everything and therefore never
    # produces a compile-time error.
    class AnyType < Type
      def nullable?
        true
      end

      def ==(other)
        other.is_a?(AnyType)
      end

      def to_s
        'any'
      end
    end

    # One of: string, number, boolean, null, expression.
    class ScalarType < Type
      attr_reader :name

      def initialize(name, note = nil)
        super(note)
        @name = name
      end

      def ==(other)
        other.is_a?(ScalarType) && other.name == name
      end

      def to_s
        name
      end
    end

    class ArrayType < Type
      attr_reader :element

      def initialize(element, note = nil)
        super(note)
        @element = element
      end

      def ==(other)
        other.is_a?(ArrayType) && other.element == element
      end

      def to_s
        element.is_a?(AnyType) ? 'array' : "array<#{element}>"
      end

      def explanation
        @note || element.explanation
      end
    end

    # An object with known fields. Keys listed in `optional_keys` may be
    # absent. When `open` is true, undeclared keys yield `any` instead of
    # `null` (the shape is treated as partial).
    class ObjectType < Type
      attr_reader :fields, :optional_keys

      def initialize(fields = {}, optional_keys = [], open: false, note: nil)
        super(note)
        @fields = fields
        @optional_keys = optional_keys
        @open = open
      end

      def open?
        @open
      end

      def optional?(key)
        @optional_keys.include?(key)
      end

      def ==(other)
        other.is_a?(ObjectType) &&
          other.fields == fields &&
          other.optional_keys.sort == optional_keys.sort &&
          other.open? == open?
      end

      def to_s
        return 'object' if @fields.empty? && @open

        pairs = @fields.map do |key, value|
          optional?(key) ? "#{key}?: #{value}" : "#{key}: #{value}"
        end
        pairs << '...' if @open
        "object{#{pairs.join(', ')}}"
      end
    end

    class UnionType < Type
      attr_reader :types

      def initialize(types, note = nil)
        super(note)
        @types = types
      end

      def nullable?
        @types.any?(&:nullable?)
      end

      def ==(other)
        other.is_a?(UnionType) && other.types == types
      end

      def to_s
        @types.map(&:to_s).join(' | ')
      end

      def explanation
        @note || @types.map(&:explanation).compact.first
      end
    end

    ANY = AnyType.new
    STRING = ScalarType.new('string')
    NUMBER = ScalarType.new('number')
    BOOLEAN = ScalarType.new('boolean')
    NULL = ScalarType.new('null')
    EXPRESSION = ScalarType.new('expression')
    ARRAY_ANY = ArrayType.new(ANY)
    OBJECT_ANY = ObjectType.new({}, [], open: true)

    class << self
      def array_of(element)
        ArrayType.new(build(element))
      end

      # Shorthand for `union(type, NULL)`.
      def optional(type)
        union(build(type), NULL)
      end

      # @param [Hash] fields maps field names to shapes; names ending with
      #   `?` are optional.
      def object(fields = {}, options = {})
        optional = Array(options[:optional]).map(&:to_s)
        open = options[:open] ? true : false
        built = {}
        fields.each_pair do |key, value|
          key = key.to_s
          if key.end_with?('?')
            key = key[0..-2]
            optional << key
          end
          built[key] = build(value)
        end
        ObjectType.new(built, optional.uniq, open: open)
      end

      def union(*types)
        types = types.map { |t| build(t) }
        flat = types.flat_map { |t| t.is_a?(UnionType) ? t.types : [t] }
        return ANY if flat.any? { |t| t.is_a?(AnyType) }

        flat = flat.uniq
        case flat.size
        when 0 then ANY
        when 1 then flat.first
        else UnionType.new(flat)
        end
      end

      # Returns nil when the two shapes are provably disjoint. Object
      # intersection is approximate: only shared keys are intersected.
      def intersect(left, right)
        return right if left.is_a?(AnyType)
        return left if right.is_a?(AnyType)

        if left.is_a?(UnionType)
          members = left.types.map { |t| intersect(t, right) }.compact
          return members.empty? ? nil : union(*members)
        end
        return intersect(right, left) if right.is_a?(UnionType)

        case left
        when ScalarType
          left == right ? left : nil
        when ArrayType
          return nil unless right.is_a?(ArrayType)

          element = intersect(left.element, right.element)
          element && ArrayType.new(element)
        when ObjectType
          return nil unless right.is_a?(ObjectType)

          intersect_objects(left, right)
        end
      end

      # Removes `null` from a shape; nil when nothing remains.
      def remove_null(type)
        case type
        when UnionType
          members = type.types.reject { |t| t == NULL }
          members.empty? ? nil : union(*members)
        when ScalarType
          type == NULL ? nil : type
        else
          type
        end
      end

      def maybe_null?(type)
        case type
        when UnionType then type.types.any? { |t| maybe_null?(t) }
        else type.nullable?
        end
      end

      # Shape of a plain Ruby value (used for literals).
      def from_value(value)
        case value
        when nil then NULL
        when true, false then BOOLEAN
        when Numeric then NUMBER
        when String then STRING
        when Array then ARRAY_ANY
        when Hash then OBJECT_ANY
        else ANY
        end
      end

      # Builds a shape from plain Ruby data:
      #
      #   :string / :number / :boolean / :null / :any / :array / :object
      #   [spec]                      # array of spec
      #   [:maybe, spec]              # spec | null
      #   [:or, spec, spec, ...]      # union
      #   { 'key' => spec, 'opt?' => spec }
      def build(spec)
        case spec
        when Type then spec
        when Symbol then named(spec)
        when Array
          return ARRAY_ANY if spec.empty?

          head = spec[0]
          if %i[or union].include?(head)
            union(*spec[1..-1].map { |s| build(s) })
          elsif %i[maybe optional nullable].include?(head)
            optional(build(spec[1]))
          else
            array_of(build(head))
          end
        when Hash then object(spec)
        else
          raise ArgumentError, "unsupported shape spec: #{spec.inspect}"
        end
      end

      # Replaces the shape found at `path` (a list of object keys) with
      # the result of the block. When the block returns nil (impossible
      # narrowing) the original shape is kept: we never invent errors we
      # cannot prove.
      def narrow_path(type, path, &block)
        if path.empty?
          block.call(type) || type
        else
          case type
          when ObjectType
            key = path.first
            old = type.fields[key] || (type.open? ? ANY : NULL)
            narrowed = narrow_path(old, path[1..-1], &block)
            fields = type.fields.merge(key => narrowed)
            ObjectType.new(fields, type.optional_keys - [key], open: type.open?)
          when UnionType
            union(*type.types.map { |t| narrow_path(t, path, &block) })
          else
            type
          end
        end
      end

      private

      def named(spec)
        case spec
        when :any then ANY
        when :string then STRING
        when :number then NUMBER
        when :boolean then BOOLEAN
        when :null then NULL
        when :array then ARRAY_ANY
        when :object then OBJECT_ANY
        else
          raise ArgumentError, "unknown type name: #{spec.inspect}"
        end
      end

      def intersect_objects(left, right)
        return right if left.fields.empty? && left.open?
        return left if right.fields.empty? && right.open?

        fields = {}
        (left.fields.keys | right.fields.keys).each do |key|
          if left.fields.key?(key) && right.fields.key?(key)
            value = intersect(left.fields[key], right.fields[key])
            return nil unless value

            fields[key] = value
          else
            fields[key] = left.fields[key] || right.fields[key]
          end
        end
        optional = (left.optional_keys & right.optional_keys) |
                   (fields.keys - left.fields.keys) |
                   (fields.keys - right.fields.keys)
        ObjectType.new(fields, optional, open: left.open? && right.open?)
      end
    end
  end
end
