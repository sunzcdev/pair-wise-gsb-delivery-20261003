# frozen_string_literal: true

module Dry
  class Struct
    # Builds a struct instance incrementally, in multiple steps.
    #
    # Unlike {.new}, which requires every mandatory attribute up front, a
    # StagedBuilder validates each batch of values as it arrives and keeps
    # track of the attributes that are still missing. Completeness is only
    # verified when {#finish} is called, which is also the moment when
    # default values are resolved and the actual struct is instantiated.
    #
    # Nested structs and arrays of structs are staged recursively: assigning
    # a partial hash for a nested attribute keeps a child builder, so missing
    # keys deep in the structure are reported with their full path.
    #
    # @example
    #   builder = User.builder
    #   builder.assign(name: "Jane")
    #   builder.assign(address: {city: "NYC"})
    #   builder.assign(address: {zipcode: "123"})
    #   builder.finish
    #   # => #<User name="Jane" address=#<Address city="NYC" zipcode="123">>
    class StagedBuilder
      # Raised when {#finish} is called while required attributes are missing
      class MissingAttributesError < Error
        # @return [Array<Array<Symbol, Integer>>] missing attribute paths,
        #   each path is a list of attribute names and array indices
        attr_reader :paths

        def initialize(paths, klass)
          @paths = paths.map(&:freeze).freeze
          super(
            "[#{klass}.builder] missing attributes: " \
            "#{paths.map { |path| format_path(path) }.join(', ')}"
          )
        end

        private

        def format_path(path)
          path.map.with_index do |segment, index|
            if segment.is_a?(::Integer)
              "[#{segment}]"
            elsif index.zero?
              segment.to_s
            else
              ".#{segment}"
            end
          end.join
        end
      end

      # @return [Class] the struct class being built
      attr_reader :struct_class

      def initialize(struct_class)
        @struct_class = struct_class
        @values = {}
        @children = {}
        @defaults = {}
      end

      # Validates and stores a batch of attributes.
      #
      # Every given value is checked against its attribute type right away.
      # An explicit nil is treated as a real value and validated like any
      # other. If any key or value is invalid, the whole batch is rejected
      # and everything stored before stays untouched.
      #
      # @param [Hash{Symbol => Object}] attributes
      # @return [self]
      # @raise [Dry::Struct::Error] when a key is unknown or a value doesn't
      #   conform to its attribute type
      def assign(attributes)
        staged = attributes.map do |name, value|
          key = fetch_key(name)
          [key.name, stage(key, value)]
        end

        staged.each do |name, (kind, payload)|
          if kind.equal?(:value)
            @values[name] = payload
            @children.delete(name)
          else
            @children[name] = payload
            @values.delete(name)
          end
        end

        self
      end

      # Builds the struct if every required attribute has been provided.
      #
      # Default values are resolved here, and only here. Successfully
      # resolved defaults are memoized, so a failed attempt followed by more
      # {#assign} calls will not evaluate them again.
      #
      # @return [Dry::Struct]
      # @raise [MissingAttributesError] listing every missing attribute path
      def finish
        attributes = {}
        missing = []

        struct_class.schema.each do |key|
          name = key.name

          if @values.key?(name)
            attributes[name] = @values[name]
          elsif @children.key?(name)
            begin
              attributes[name] = finish_child(@children[name])
            rescue MissingAttributesError => error
              missing.concat(error.paths.map { |path| [name, *path] })
            end
          elsif @defaults.key?(name)
            attributes[name] = @defaults[name]
          elsif key.type.default?
            attributes[name] = @defaults[name] = key.type.call_unsafe(Undefined)
          elsif key.required?
            missing << [name]
          end
        end

        raise MissingAttributesError.new(missing, struct_class) unless missing.empty?

        struct_class.load(attributes)
      end

      private

      def initialize_copy(source)
        super
        @values = @values.dup
        @children = @children.dup
        @defaults = @defaults.dup
      end

      # Stages a single value without touching the builder state, so that
      # a failing {#assign} leaves previously stored attributes intact
      def stage(key, value)
        if (nested_class = self.class.nested_struct(key.type)) && value.is_a?(::Hash)
          [:child, stage_child(nested_class, key.name, value)]
        elsif (member_class = self.class.member_struct(key.type)) && value.is_a?(::Array)
          [:children, stage_members(member_class, key.name, value)]
        else
          [:value, validate(key, value)]
        end
      end

      def stage_child(klass, name, hash)
        child = @children[name]
        child = child.is_a?(StagedBuilder) ? child.dup : self.class.new(klass)
        child.assign(hash)
        child
      end

      def stage_members(klass, name, array)
        existing = @children[name]
        existing = EMPTY_ARRAY unless existing.is_a?(::Array)

        staged = array.each_with_index.map do |element, index|
          if element.is_a?(::Hash)
            sibling = existing[index]
            child =
              if sibling.is_a?(Array) && sibling[1].is_a?(StagedBuilder)
                sibling[1].dup
              else
                self.class.new(klass)
              end
            [:child, child.assign(element)]
          else
            [:value, validate_member(klass, element)]
          end
        end

        staged.concat(existing.drop(staged.size))
      end

      def finish_child(child)
        if child.is_a?(StagedBuilder)
          child.finish
        else
          finish_members(child)
        end
      end

      def finish_members(members)
        missing = []

        values = members.each_with_index.map do |(kind, payload), index|
          if kind.equal?(:value)
            payload
          else
            begin
              payload.finish
            rescue MissingAttributesError => error
              missing.concat(error.paths.map { |path| [index, *path] })
              nil
            end
          end
        end

        raise MissingAttributesError.new(missing, struct_class) unless missing.empty?

        values
      end

      def validate(key, value)
        key.type.call_unsafe(value)
      rescue Types::CoercionError => exception
        raise Error, "[#{struct_class}.builder] #{key.name}: #{exception.message}"
      end

      def validate_member(member_class, value)
        member_class.call_unsafe(value)
      rescue Types::CoercionError => exception
        raise Error, "[#{struct_class}.builder] #{exception.message}"
      end

      def fetch_key(name)
        key_name = name.respond_to?(:to_sym) ? name.to_sym : name
        schema = struct_class.schema
        return schema.key(key_name) if schema.key?(key_name)

        raise Error, "[#{struct_class}.builder] unknown attribute: #{name.inspect}"
      end

      class << self
        # @return [Class, nil] struct class carried by the type, if any
        # @api private
        def nested_struct(type)
          if type.is_a?(::Class) && type <= Struct
            type
          elsif type.respond_to?(:optional?) && type.optional? && type.respond_to?(:right)
            nested_struct(type.right)
          end
        end

        # @return [Class, nil] struct class of an array member type, if any
        # @api private
        def member_struct(type)
          nested_struct(type.member) if type.respond_to?(:member)
        end
      end
    end
  end
end
