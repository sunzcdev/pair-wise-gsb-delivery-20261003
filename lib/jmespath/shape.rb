# frozen_string_literal: true

require 'set'

module JMESPath
  # A lightweight structural "shape" description, deliberately much smaller
  # than a full JSON Schema implementation. A shape answers the single
  # question a compile-time check needs: which JMESPath types can a value
  # take, and (for objects/arrays) what are the shapes of its parts?
  #
  # Shapes are immutable value objects. Use the {Shape} factory methods:
  #
  #   shape = JMESPath::Shape.object(
  #     id: JMESPath::Shape.number,
  #     tags: JMESPath::Shape.array(JMESPath::Shape.string),
  #     nick: JMESPath::Shape.optional(JMESPath::Shape.string)
  #   )
  #
  class Shape
    # JMESPath runtime type tags (mirrors Nodes::TypeChecker).
    NUMBER     = :number
    STRING     = :string
    BOOLEAN    = :boolean
    NULL       = :null
    OBJECT     = :object
    ARRAY      = :array
    EXPRESSION = :expression

    # The value may take any shape. Everything unprovable stays compatible
    # with this, so unknown data is never reported as an error.
    ANY_KIND = :any

    # The value can never exist (e.g. the result of an impossible branch).
    NEVER_KIND = :never

    # An object field with its presence information.
    class Field
      attr_reader :shape, :required

      def initialize(shape, required: true)
        @shape = shape
        @required = required
      end

      def optional?
        !@required
      end

      def required?
        @required
      end

      # Value shape produced when the field is read. A missing optional
      # field reads as null.
      def value_shape
        required? ? @shape : Shape.nullable(@shape)
      end

      def with_shape(new_shape)
        Field.new(new_shape, required: @required)
      end

      def to_s
        "#{@shape}#{required? ? '' : '?'}"
      end
      alias inspect to_s

      def ==(other)
        other.is_a?(Field) && shape == other.shape && required == other.required
      end
      alias eql? ==

      def hash
        [shape, required].hash
      end
    end

    attr_reader :kind, :fields, :element

    def initialize(kind, fields: nil, element: nil, open: true)
      @kind = kind
      @fields = fields
      @element = element
      @open = open
      freeze
    end

    # ------------------------------------------------------------------
    # Factories
    # ------------------------------------------------------------------

    PRIMITIVES = {
      NUMBER     => new(NUMBER),
      STRING     => new(STRING),
      BOOLEAN    => new(BOOLEAN),
      NULL       => new(NULL),
      EXPRESSION => new(EXPRESSION),
      ANY_KIND   => new(ANY_KIND),
      NEVER_KIND => new(NEVER_KIND)
    }.freeze

    def self.number
      PRIMITIVES[NUMBER]
    end

    def self.string
      PRIMITIVES[STRING]
    end

    def self.boolean
      PRIMITIVES[BOOLEAN]
    end

    def self.null
      PRIMITIVES[NULL]
    end

    def self.expression
      PRIMITIVES[EXPRESSION]
    end

    def self.any
      PRIMITIVES[ANY_KIND]
    end

    def self.never
      PRIMITIVES[NEVER_KIND]
    end

    # @param [Hash] map field name to {Shape} or {Field}; a plain {Shape}
    #   value is treated as a required field.
    # @param [Boolean] open when true (the default) the object may carry
    #   fields not listed here, so accessing an unknown field cannot be
    #   proven erroneous and yields null (plus a runtime note).
    def self.object(map = nil, open: true, **kwargs)
      map = (map || {}).merge(kwargs)
      fields = {}
      map.each do |name, value|
        fields[name.to_s] = value.is_a?(Field) ? value : Field.new(value)
      end
      new(OBJECT, fields: fields, open: open)
    end

    def self.array(element = any)
      new(ARRAY, element: element)
    end

    # Normalize one or more shapes into a single shape, flattening unions.
    def self.union(*shapes)
      members = []
      shapes.flatten.each do |shape|
        shape = coerce(shape)
        if shape.union?
          members.concat(shape.members)
        elsif !members.include?(shape)
          members << shape
        end
      end
      return never if members.empty?
      return members.first if members.size == 1
      new(:union, fields: members)
    end

    # Mark a shape as an optional object field (it may be missing, in which
    # case reading it yields null).
    def self.optional(shape)
      Field.new(coerce(shape), required: false)
    end

    # Infer a shape from a concrete example value.
    # @param [Array,Symbol,String,Set] optional_keys keys that should be
    #   treated as optional rather than required.
    def self.from_value(value, optional_keys = nil)
      infer_value(value, normalize_optional_keys(optional_keys))
    end

    # Merge several example values into one shape; heterogeneous arrays and
    # differing object fields become unions / optional fields.
    def self.from_values(*values)
      values = values.flatten
      return union if values.empty?
      union(values.map { |value| from_value(value) })
    end

    # Accept a kind symbol (e.g. +Shape::NUMBER+) or a {Shape}.
    def self.coerce(value)
      return value if value.is_a?(Shape)
      PRIMITIVES.fetch(value) { raise ArgumentError, "unknown shape #{value.inspect}" }
    end

    def self.nullable(shape)
      union(coerce(shape), null)
    end

    # ------------------------------------------------------------------
    # Queries
    # ------------------------------------------------------------------

    def union?
      @kind == :union
    end

    def object?
      @kind == OBJECT
    end

    def array?
      @kind == ARRAY
    end

    def number?
      @kind == NUMBER
    end

    def string?
      @kind == STRING
    end

    def boolean?
      @kind == BOOLEAN
    end

    def expression?
      @kind == EXPRESSION
    end

    def never?
      @kind == NEVER_KIND
    end

    def any?
      @kind == ANY_KIND
    end

    def open?
      @open
    end

    def ==(other)
      other.is_a?(Shape) &&
        kind == other.kind &&
        open? == other.open? &&
        element == other.element &&
        fields == other.fields
    end
    alias eql? ==

    def hash
      [kind, element, fields, open?].hash
    end

    def members
      union? ? @fields : [self]
    end

    # Shape produced by a field access or index. Returns Ruby +nil+ only
    # when the access is provably absent on a closed object.
    def lookup(key)
      case @kind
      when OBJECT
        field = @fields[key.to_s]
        if field
          field.value_shape
        elsif @open
          Shape.nullable(Shape.any)
        else
          # Closed object without the key: access deterministically reads null.
          Shape.null
        end
      when ARRAY
        key.is_a?(Integer) ? Shape.nullable(@element) : nil
      when :union
        found = members.map { |member| member.lookup(key) }
        non_fieldable = found.any?(&:nil?)
        found = found.compact
        return nil if non_fieldable && found.empty?
        # A scalar/closed-object member reads as null when the key is absent.
        found << Shape.null if non_fieldable
        Shape.union(found)
      when ANY_KIND
        Shape.nullable(Shape.any)
      else
        # Scalar (number/string/boolean/null/expression): field access yields null.
        Shape.null
      end
    end

    def nullable?
      members.any? { |member| member.kind == NULL }
    end

    def add_null
      nullable? ? self : Shape.union(self, Shape.null)
    end

    def remove_null
      return self unless nullable?
      kept = members.reject { |member| member.kind == NULL }
      kept.empty? ? Shape.never : Shape.union(kept)
    end

    def null_kinding?
      @kind == NULL
    end

    # Can this value possibly be of the given kind?
    def possibly?(kind)
      members.any? { |member| member.kind == kind || member.kind == ANY_KIND }
    end

    # Are all concrete possible values of the given kind?
    def definitely?(kind)
      concrete = members.reject { |member| member.kind == ANY_KIND }
      !concrete.empty? && concrete.all? { |member| member.kind == kind }
    end

    # Greatest-lower-bound of two shapes: the values both can describe.
    def intersect(other)
      other = Shape.coerce(other)
      return self if other.any?
      return other if any?
      return Shape.never if never? || other.never?

      if union? || other.union?
        return Shape.union(
          members.flat_map do |member|
            other.members.map { |other_member| member.intersect(other_member) }
          end
        )
      end

      return intersect_object(other) if object? && other.object?
      return Shape.array(element.intersect(other.element)) if array? && other.array?

      kind == other.kind ? self : Shape.never
    end

    def to_s
      case @kind
      when OBJECT then describe_object
      when ARRAY then "array<#{@element}>"
      when :union then members.map(&:to_s).join(' | ')
      else @kind.to_s
      end
    end
    alias inspect to_s

    # ------------------------------------------------------------------
    # Internal
    # ------------------------------------------------------------------

    def self.infer_value(value, optional_keys)
      case value
      when nil then null
      when true, false then boolean
      when Numeric then number
      when String then string
      when Array
        element = value.empty? ? any : union(value.map { |item| infer_value(item, optional_keys) })
        array(element)
      when Hash
        infer_hash(value, optional_keys)
      when Struct
        hash = value.members.each_with_object({}) { |key, memo| memo[key] = value[key] }
        infer_hash(hash, optional_keys)
      else
        any
      end
    end
    private_class_method :infer_value

    def self.infer_hash(hash, optional_keys)
      map = {}
      hash.each do |key, item|
        field_shape = infer_value(item, optional_keys)
        map[key] = if optional_keys.include?(key.to_s)
                     optional(field_shape)
                   else
                     field_shape
                   end
      end
      object(map)
    end
    private_class_method :infer_hash

    def self.normalize_optional_keys(keys)
      case keys
      when nil then Set.new
      when Set then keys
      else Array(keys).map(&:to_s).to_set
      end
    end
    private_class_method :normalize_optional_keys

    private

    def intersect_object(other)
      names = (fields.keys + other.fields.keys).uniq
      intersected = {}
      names.each do |name|
        left = fields[name]
        right = other.fields[name]
        if left && right
          combined = left.shape.intersect(right.shape)
          required = left.required? && right.required?
          intersected[name] = Field.new(combined, required: required)
        elsif left
          shape = left.required? ? left.shape.intersect(Shape.null) : left.shape
          intersected[name] = Field.new(shape, required: false)
        else
          shape = right.required? ? right.shape.intersect(Shape.null) : right.shape
          intersected[name] = Field.new(shape, required: false)
        end
      end
      Shape.object(intersected, open: open? && other.open?)
    end

    def describe_object
      parts = fields.keys.sort.map do |name|
        field = fields[name]
        "#{name}:#{field.shape}#{field.required? ? '' : '?'}"
      end
      "{#{parts.join(', ')}}"
    end
  end
end
