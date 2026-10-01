# frozen_string_literal: true

require "weakref"

require "dry/core"
require "dry/types"

require "dry/struct/class_interface"
require "dry/struct/errors"
require "dry/struct/version"

module Dry
  # Constructor method for easily creating a {Dry::Struct}.
  # @return [Dry::Struct]
  # @example
  #   require 'dry-struct'
  #
  #   module Types
  #     include Dry.Types()
  #   end
  #
  #   Person = Dry.Struct(name: Types::String, age: Types::Integer)
  #   matz = Person.new(name: "Matz", age: 52)
  #   matz.name #=> "Matz"
  #   matz.age #=> 52
  #
  #   Test = Dry.Struct(expected: Types::String) { schema(schema.strict) }
  #   Test[expected: "foo", unexpected: "bar"]
  #   #=> Dry::Struct::Error: [Test.new] unexpected keys [:unexpected] in Hash input
  def self.Struct(attributes = Dry::Core::Constants::EMPTY_HASH, &block)
    Class.new(Dry::Struct) do
      attributes.each { |a, type| attribute a, type }
      module_eval(&block) if block_given?
    end
  end

  # Typed {Struct} with virtus-like DSL for defining schema.
  #
  # ### Differences between dry-struct and virtus
  #
  # {Struct} look somewhat similar to [Virtus][] but there are few significant differences:
  #
  # * {Struct}s don't provide attribute writers and are meant to be used
  #   as "data objects" exclusively.
  # * Handling of attribute values is provided by standalone type objects from
  #   [`dry-types`][].
  # * Handling of attribute hashes is provided by standalone hash schemas from
  #   [`dry-types`][].
  # * Struct classes quack like [`dry-types`][], which means you can use them
  #   in hash schemas, as array members or sum them
  #
  # {Struct} class can specify a constructor type, which uses [hash schemas][]
  # to handle attributes in `.new` method.
  #
  # [`dry-types`]: https://github.com/dry-rb/dry-types
  # [Virtus]: https://github.com/solnic/virtus
  # [hash schemas]: http://dry-rb.org/gems/dry-types/hash-schemas
  #
  # @example
  #   require 'dry-struct'
  #
  #   module Types
  #     include Dry.Types()
  #   end
  #
  #   class Book < Dry::Struct
  #     attribute :title, Types::String
  #     attribute :subtitle, Types::String.optional
  #   end
  #
  #   rom_n_roda = Book.new(
  #     title: 'Web Development with ROM and Roda',
  #     subtitle: nil
  #   )
  #   rom_n_roda.title #=> 'Web Development with ROM and Roda'
  #   rom_n_roda.subtitle #=> nil
  #
  #   refactoring = Book.new(
  #     title: 'Refactoring',
  #     subtitle: 'Improving the Design of Existing Code'
  #   )
  #   refactoring.title #=> 'Refactoring'
  #   refactoring.subtitle #=> 'Improving the Design of Existing Code'
  class Struct
    extend Core::Extensions
    include Core::Constants
    extend ClassInterface
    extend Core::Deprecations[:"dry-struct"]

    class << self
      # override `Dry::Types::Builder#prepend`
      define_method(:prepend, ::Module.method(:prepend))

      def loader
        @loader ||= ::Zeitwerk::Loader.new.tap do |loader|
          root = ::File.expand_path("..", __dir__)
          loader.tag = "dry-struct"
          loader.inflector = ::Zeitwerk::GemInflector.new("#{root}/dry-struct.rb")
          loader.push_dir(root)
          loader.ignore(
            "#{root}/dry-struct.rb",
            "#{root}/dry/struct/{class_interface,errors,extensions,printer,value,version}.rb",
            "#{root}/dry/struct/extensions"
          )
        end
      end
    end

    loader.setup

    include ::Dry::Equalizer(:__attributes__, inspect: false, immutable: true)

    # {Dry::Types::Hash::Schema} subclass with specific behaviour defined for
    # @return [Dry::Types::Hash::Schema]
    defines :schema
    schema Types["coercible.hash"].schema(EMPTY_HASH)

    defines :abstract_class
    abstract

    # @!attribute [Hash{Symbol => Object}] attributes
    attr_reader :attributes
    alias_method :__attributes__, :attributes

    # @param [Hash, #each] attributes
    def initialize(attributes)
      @attributes = attributes
    end

    # Retrieves value of previously defined attribute by its' `name`
    #
    # @param [String] name
    # @return [Object]
    #
    # @example
    #   class Book < Dry::Struct
    #     attribute :title, Types::String
    #     attribute :subtitle, Types::String.optional
    #   end
    #
    #   rom_n_roda = Book.new(
    #     title: 'Web Development with ROM and Roda',
    #     subtitle: nil
    #   )
    #   rom_n_roda[:title] #=> 'Web Development with ROM and Roda'
    #   rom_n_roda[:subtitle] #=> nil
    def [](name)
      @attributes.fetch(name) do
        if self.class.attribute_names.include?(name)
          nil
        else
          raise MissingAttributeError.new(attribute: name, klass: self.class)
        end
      end
    end

    # Converts the {Dry::Struct} to a hash with keys representing
    # each attribute (as symbols) and their corresponding values
    #
    # @return [Hash{Symbol => Object}]
    #
    # @example
    #   class Book < Dry::Struct
    #     attribute :title, Types::String
    #     attribute :subtitle, Types::String.optional
    #   end
    #
    #   rom_n_roda = Book.new(
    #     title: 'Web Development with ROM and Roda',
    #     subtitle: nil
    #   )
    #   rom_n_roda.to_hash
    #     #=> {title: 'Web Development with ROM and Roda', subtitle: nil}
    def to_h
      self.class.schema.each_with_object({}) do |key, result|
        result[key.name] = Hashify[self[key.name]] if attributes.key?(key.name)
      end
    end
    # TODO: remove in 2.0
    alias_method :to_hash, :to_h

    # Create a copy of {Dry::Struct} with overriden attributes
    #
    # @param [Hash{Symbol => Object}] changeset
    #
    # @return [Struct]
    #
    # @example
    #   class Book < Dry::Struct
    #     attribute :title, Types::String
    #     attribute :subtitle, Types::String.optional
    #   end
    #
    #   rom_n_roda = Book.new(
    #     title: 'Web Development with ROM and Roda',
    #     subtitle: '2nd edition'
    #   )
    #     #=> #<Book title="Web Development with ROM and Roda" subtitle="2nd edition">
    #
    #   rom_n_roda.new(subtitle: '3rd edition')
    #     #=> #<Book title="Web Development with ROM and Roda" subtitle="3rd edition">
    def new(changeset)
      new_attributes = self.class.schema.apply(
        changeset,
        skip_missing: true,
        resolve_defaults: false
      )
      self.class.load(__attributes__.merge(new_attributes))
    rescue Types::SchemaError, Types::MissingKeyError, Types::UnknownKeysError => exception
      raise Error, "[#{self}.new] #{exception}"
    end
    alias_method :__new__, :new

    # Sentinel value for explicitly deleting an optional attribute
    # in a path update
    #
    # @example
    #   user.update_in(:address, :note, Dry::Struct::DELETE)
    #   user.update([:address, :note] => Dry::Struct::DELETE)
    #
    # @see #update
    # @see #delete_in
    DELETE = ::Object.new

    class << DELETE
      # @return [String]
      def inspect = "Dry::Struct::DELETE"

      # @return [String]
      def to_s = inspect
    end

    DELETE.freeze

    # Create a copy of {Dry::Struct} with changes applied at the given paths.
    #
    # A path is an array of segments where a {Symbol} segment follows a struct
    # attribute and an {Integer} segment follows an array index. Every new
    # value is checked against (and coerced by) the type of the attribute or
    # array member it is assigned to. If any value doesn't pass its type
    # check, no changes are applied and an error is raised.
    #
    # Only the structs and arrays along the updated paths are recreated,
    # every other (nested) value is shared with the original struct. Values
    # that are not updated are never passed through type constructors again.
    #
    # Passing {DELETE} as a value removes the attribute at the path. Only
    # omittable (optional) attributes can be deleted, see {RequiredAttributeError}.
    # Assigning `nil` is different from deleting: it keeps the attribute and
    # type-checks `nil` against the attribute type.
    #
    # A single batch cannot contain both a path and one of its own subpaths,
    # such input is ambiguous and rejected with {AmbiguousPathError}.
    #
    # @param [Hash{Array<Symbol, Integer>, Symbol, Integer => Object}] updates
    #   paths mapped to new values (or {DELETE})
    #
    # @return [Struct]
    #
    # @raise [InvalidPathError] when a path cannot be followed
    # @raise [AmbiguousPathError] when paths overlap within a single update
    # @raise [RequiredAttributeError] when trying to delete a required attribute
    # @raise [Error] when a new value doesn't conform its type
    #
    # @example
    #   class Order < Dry::Struct
    #     attribute :id, Types::Integer
    #     attribute :address do
    #       attribute :city, Types::String
    #       attribute? :note, Types::String
    #     end
    #     attribute :tags, Types::Array.of(Types::String)
    #   end
    #
    #   order = Order.new(id: 1, address: {city: "Paris"}, tags: %w[gift])
    #
    #   order.update(
    #     [:address, :city] => "Lyon",
    #     [:address, :note] => Dry::Struct::DELETE,
    #     [:tags, 0] => "urgent"
    #   )
    #     #=> #<Order id=1 address=#<Order::Address city="Lyon" note=nil> tags=["urgent"]>
    def update(updates)
      return self if updates.empty?

      __apply_update__(__build_update_trie__(updates), EMPTY_ARRAY)
    end

    # Create a copy of {Dry::Struct} with a single path updated
    #
    # @param [Array<Symbol, Integer>] path segments leading to the updated value,
    #   given either as separate arguments or as a single array
    # @param [Object] value new value, or {DELETE} to remove an optional attribute
    #
    # @return [Struct]
    #
    # @example
    #   order.update_in(:address, :city, "Lyon")
    #   order.update_in([:tags, 0], "urgent")
    #
    # @see #update
    def update_in(*path, value)
      update(__update_path__(path) => value)
    end

    # Create a copy of {Dry::Struct} with the optional attribute
    # at the given path removed
    #
    # @param [Array<Symbol, Integer>] path segments leading to the deleted attribute
    #
    # @return [Struct]
    #
    # @raise [RequiredAttributeError] when the attribute is required
    #
    # @example
    #   order.delete_in(:address, :note)
    #
    # @see #update
    def delete_in(*path)
      update(__update_path__(path) => DELETE)
    end

    # @return [String]
    def inspect
      klass = self.class
      attrs = klass.attribute_names.map { |key| " #{key}=#{@attributes[key].inspect}" }.join
      "#<#{klass.name || klass.inspect}#{attrs}>"
    end

    # Pattern matching support
    #
    # @api private
    def deconstruct_keys(_keys) = attributes

    private

    # @api private
    def __update_path__(path)
      path.size == 1 && path.first.is_a?(::Array) ? path.first : path
    end

    # Builds a tree of changes from a flat `{path => value}` mapping,
    # rejecting empty, duplicated and overlapping paths
    #
    # @api private
    def __build_update_trie__(updates)
      updates.each_with_object(children: {}) do |(path, value), root|
        segments = ::Kernel.Array(path)

        if segments.empty?
          raise InvalidPathError, "update path cannot be empty"
        end

        node = root

        segments.each do |segment|
          unless segment.is_a?(::Symbol) || segment.is_a?(::Integer)
            raise InvalidPathError,
                  "invalid path segment #{segment.inspect} in #{segments.inspect}: " \
                  "path segments must be Symbols (struct attributes) " \
                  "or Integers (array indices)"
          end

          if node.key?(:value)
            raise AmbiguousPathError,
                  "path #{segments.inspect} overlaps with a shorter path in the same update"
          end

          node = (node[:children][segment] ||= {children: {}})
        end

        if node.key?(:value) || !node[:children].empty?
          raise AmbiguousPathError,
                "path #{segments.inspect} overlaps with another path in the same update"
        end

        node[:value] = value
      end
    end

    # Applies a subtree of changes to this struct, recreating only
    # the attributes along the updated paths
    #
    # @api private
    def __apply_update__(node, path)
      new_attributes = __attributes__.dup

      node[:children].each do |name, child|
        unless name.is_a?(::Symbol)
          raise InvalidPathError,
                "invalid path #{(path + [name]).inspect}: " \
                "#{name.inspect} is not an attribute of #{self.class}"
        end

        unless self.class.has_attribute?(name)
          raise MissingAttributeError.new(attribute: name, klass: self.class)
        end

        key = self.class.schema.key(name)
        current_path = path + [name]

        if child.key?(:value)
          if DELETE.equal?(child[:value])
            if key.required?
              raise RequiredAttributeError,
                    "cannot delete required attribute #{name.inspect} of #{self.class} " \
                    "(path #{current_path.inspect})"
            end

            new_attributes.delete(name)
          else
            new_attributes[name] = __coerce_update_value__(key.type, child[:value], current_path)
          end
        else
          new_attributes[name] =
            __descend_update__(__attributes__[name], key.type, child, current_path)
        end
      end

      self.class.load(new_attributes)
    end

    # @api private
    def __descend_update__(value, type, node, path)
      case value
      when Dry::Struct
        value.__send__(:__apply_update__, node, path)
      when ::Array
        __apply_array_update__(value, type, node, path)
      else
        raise InvalidPathError,
              "cannot follow path #{path.inspect}: " \
              "#{value.inspect} is neither a Dry::Struct nor an Array"
      end
    end

    # Applies a subtree of changes to an array, recreating it
    # without expanding it beyond its current size
    #
    # @api private
    def __apply_array_update__(array, type, node, path)
      member_type = __array_member_type__(type)
      new_array = nil

      node[:children].each do |index, child|
        current_path = path + [index]

        unless index.is_a?(::Integer)
          raise InvalidPathError,
                "invalid path #{current_path.inspect}: " \
                "array indices must be Integers, got #{index.inspect}"
        end

        unless index < array.size && index >= -array.size
          raise InvalidPathError,
                "invalid path #{current_path.inspect}: " \
                "index #{index} is out of bounds for an array of size #{array.size}"
        end

        new_element =
          if child.key?(:value)
            if DELETE.equal?(child[:value])
              raise InvalidPathError,
                    "cannot delete array element at path #{current_path.inspect}: " \
                    "only optional struct attributes can be deleted"
            elsif member_type
              __coerce_update_value__(member_type, child[:value], current_path)
            else
              child[:value]
            end
          else
            __descend_update__(array[index], member_type, child, current_path)
          end

        new_array ||= array.dup
        new_array[index] = new_element
      end

      new_array
    end

    # Type-checks (and coerces) a single new value without
    # touching the rest of the struct
    #
    # @api private
    def __coerce_update_value__(type, value, path)
      type.call_unsafe(value)
    rescue Types::CoercionError => exception
      raise Error,
            "[#{self.class}.update] invalid value #{value.inspect} " \
            "at path #{path.inspect}: #{exception.message}"
    end

    # Extracts the member type of an array type, unwrapping
    # constrained/constructor/optional decorations
    #
    # @api private
    def __array_member_type__(type)
      if type.is_a?(Types::Sum)
        __array_member_type__(type.left) || __array_member_type__(type.right)
      elsif type.respond_to?(:member)
        type.member
      elsif type.respond_to?(:type)
        __array_member_type__(type.type)
      end
    end
  end
end

require "dry/struct/extensions"
require "dry/struct/printer"
require "dry/struct/value"
