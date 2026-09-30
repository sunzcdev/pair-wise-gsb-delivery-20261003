# frozen_string_literal: true

require "weakref"

module Dry
  class Struct
    # Class-level interface of {Struct} and {Value}
    module ClassInterface
      include Core::ClassAttributes

      include Types::Type
      include Types::Builder

      # Adds an attribute for this {Struct} with given `name` and `type`
      # and modifies {.schema} accordingly.
      #
      # @param [Symbol] name name of the defined attribute
      # @param [Dry::Types::Type, nil] type or superclass of nested type
      # @return [Dry::Struct]
      # @yield
      #   If a block is given, it will be evaluated in the context of
      #   a new struct class, and set as a nested type for the given
      #   attribute. A class with a matching name will also be defined for
      #   the nested type.
      # @raise [RepeatedAttributeError] when trying to define attribute with the
      #   same name as previously defined one
      #
      # @example with nested structs
      #   class Language < Dry::Struct
      #     attribute :name, Types::String
      #     attribute :details, Dry::Struct do
      #       attribute :type, Types::String
      #     end
      #   end
      #
      #   Language.schema # new lines for readability
      #   # => #<Dry::Types[
      #           Constructor<Schema<keys={
      #             name: Constrained<Nominal<String> rule=[type?(String)]>
      #             details: Language::Details
      #           }> fn=Kernel.Hash>]>
      #
      #   ruby = Language.new(name: 'Ruby', details: { type: 'OO' })
      #   ruby.name #=> 'Ruby'
      #   ruby.details #=> #<Language::Details type="OO">
      #   ruby.details.type #=> 'OO'
      #
      # @example with a nested array of structs
      #   class Language < Dry::Struct
      #     attribute :name, Types::String
      #     attribute :versions, Types::Array.of(Types::String)
      #     attribute :celebrities, Types::Array.of(Dry::Struct) do
      #       attribute :name, Types::String
      #       attribute :pseudonym, Types::String
      #     end
      #   end
      #
      #   Language.schema # new lines for readability
      #   => #<Dry::Types[Constructor<Schema<keys={
      #         name: Constrained<Nominal<String> rule=[type?(String)]>
      #         versions: Constrained<
      #                     Array<Constrained<Nominal<String> rule=[type?(String)]>
      #                   > rule=[type?(Array)]>
      #         celebrities: Constrained<Array<Language::Celebrity> rule=[type?(Array)]>
      #      }> fn=Kernel.Hash>]>
      #
      #   ruby = Language.new(
      #     name: 'Ruby',
      #     versions: %w(1.8.7 1.9.8 2.0.1),
      #     celebrities: [
      #       { name: 'Yukihiro Matsumoto', pseudonym: 'Matz' },
      #       { name: 'Aaron Patterson', pseudonym: 'tenderlove' }
      #     ]
      #   )
      #   ruby.name #=> 'Ruby'
      #   ruby.versions #=> ['1.8.7', '1.9.8', '2.0.1']
      #   ruby.celebrities
      #     #=> [
      #           #<Language::Celebrity name='Yukihiro Matsumoto' pseudonym='Matz'>,
      #           #<Language::Celebrity name='Aaron Patterson' pseudonym='tenderlove'>
      #         ]
      #   ruby.celebrities[0].name #=> 'Yukihiro Matsumoto'
      #   ruby.celebrities[0].pseudonym #=> 'Matz'
      #   ruby.celebrities[1].name #=> 'Aaron Patterson'
      #   ruby.celebrities[1].pseudonym #=> 'tenderlove'
      def attribute(name, type = Undefined, &)
        attributes(name => build_type(name, type, &))
      end

      # Add attributes from another struct or {Fragment}
      #
      # Imported attributes keep track of the definition they originate from
      # (see {.attribute_source}). Reaching the same definition through
      # multiple import paths counts once, while same-named attributes from
      # different definitions raise {AttributeConflictError} — even when
      # their types are identical — and must be disambiguated explicitly
      # with `rename:`, `except:` or `resolve:`.
      #
      # @example
      #   class Address < Dry::Struct
      #     attribute :city, Types::String
      #     attribute :country, Types::String
      #   end
      #
      #   class User < Dry::Struct
      #     attribute :name, Types::String
      #     attributes_from Address
      #   end
      #
      #   User.new(name: 'Quispe', city: 'La Paz', country: 'Bolivia')
      #
      # @example with nested structs
      #   class User < Dry::Struct
      #     attribute :name, Types::String
      #     attribute :address do
      #       attributes_from Address
      #     end
      #   end
      #
      # @example selecting, renaming and namespacing imported attributes
      #   class User < Dry::Struct
      #     attributes_from Address, only: %i[city]
      #     attributes_from Timestamps, rename: {created_at: :signed_up_at}
      #     attributes_from Address, namespace: :work_address
      #   end
      #
      # @example resolving a conflict between two definitions
      #   class User < Dry::Struct
      #     attributes_from IdentityA
      #     attributes_from IdentityB, resolve: {id: IdentityA}
      #   end
      #
      # @param source [Dry::Struct, Dry::Struct::Fragment]
      # @param only [Array<Symbol>, nil] import only these attributes
      # @param except [Array<Symbol>, nil] import all but these attributes
      # @param rename [Hash{Symbol => Symbol}, nil] import attributes under new names
      # @param namespace [Symbol, nil] nest imported attributes under this attribute
      # @param resolve [Hash{Symbol => Module}, nil] explicit conflict resolutions,
      #   mapping an attribute name to the fragment or struct whose definition wins.
      #   A resolution never relaxes requiredness and cannot replace a definition
      #   already settled on the class itself
      # @raise [AttributeConflictError] on unresolved conflicts between
      #   definitions from different sources
      def attributes_from(source, only: nil, except: nil, rename: nil, namespace: nil, resolve: nil)
        if namespace
          attribute(namespace) {}
          schema
            .key(namespace)
            .type
            .attributes_from(source, only: only, except: except, rename: rename, resolve: resolve)

          return self
        end

        import_attributes(Fragment.plan(source, only: only, except: except, rename: rename),
                          resolve: resolve)
      end

      # Adds an omittable (key is not required on initialization) attribute for this {Struct}
      #
      # @example
      #   class User < Dry::Struct
      #     attribute  :name,  Types::String
      #     attribute? :email, Types::String
      #   end
      #
      #   User.new(name: 'John') # => #<User name="John" email=nil>
      #
      # @param [Symbol] name name of the defined attribute
      # @param [Dry::Types::Type, nil] type or superclass of nested type
      # @return [Dry::Struct]
      #
      def attribute?(*args, &)
        if args.size == 1 && !block_given?
          Core::Deprecations.warn(
            "Dry::Struct.attribute? is deprecated for checking attribute presence, " \
            "use has_attribute? instead",
            tag: :"dry-struct"
          )

          has_attribute?(args[0])
        else
          name, * = args

          attribute(:"#{name}?", build_type(*args, &))
        end
      end

      # @param [Hash{Symbol => Dry::Types::Type}] new_schema
      # @param [Hash{Symbol => Fragment::Source}, nil] sources origins of the
      #   new attributes, used by imports to preserve provenance. This is a
      #   positional argument so that bare-hash `attributes(name: type)` calls
      #   keep working
      # @return [Dry::Struct]
      # @raise [RepeatedAttributeError] when trying to define attribute with the
      #   same name as previously defined one
      # @see #attribute
      # @example
      #   class Book < Dry::Struct
      #     attributes(
      #       title: Types::String,
      #       author: Types::String
      #     )
      #   end
      #
      #   Book.schema
      #   # => #<Dry::Types[Constructor<Schema<keys={
      #   #      title: Constrained<Nominal<String> rule=[type?(String)]>
      #   #      author: Constrained<Nominal<String> rule=[type?(String)]>
      #   #    }> fn=Kernel.Hash>]>
      def attributes(new_schema, sources = nil)
        keys = new_schema.keys.map { |k| k.to_s.chomp("?").to_sym }
        check_schema_duplication(keys)

        schema schema.schema(new_schema)

        define_accessors(keys)

        @attribute_names = nil

        record_attribute_sources(keys, sources)

        subclasses.each do |d|
          inherited_attrs = new_schema.reject { |k, _| d.has_attribute?(k.to_s.chomp("?").to_sym) }
          inherited_keys = inherited_attrs.keys.map { |k| k.to_s.chomp("?").to_sym }
          d.attributes(inherited_attrs, @attribute_sources.slice(*inherited_keys))
        end

        self
      end

      # Add an arbitrary transformation for new attribute types.
      #
      # @param [#call,nil] proc
      # @param [#call,nil] block
      # @example
      #   class Book < Dry::Struct
      #     transform_types { |t| t.meta(struct: :Book) }
      #
      #     attribute :title, Types::String
      #   end
      #
      #   Book.schema.key(:title).meta # => { struct: :Book }
      #
      def transform_types(proc = nil, &block)
        schema schema.with_type_transform(proc || block)
      end

      # Add an arbitrary transformation for input hash keys.
      #
      # @param [#call,nil] proc
      # @param [#call,nil] block
      # @example
      #   class Book < Dry::Struct
      #     transform_keys(&:to_sym)
      #
      #     attribute :title, Types::String
      #   end
      #
      #   Book.new('title' => "The Old Man and the Sea")
      #   # => #<Book title="The Old Man and the Sea">
      def transform_keys(proc = nil, &block)
        schema schema.with_key_transform(proc || block)
      end

      # @param [Hash{Symbol => Dry::Types::Type, Dry::Struct}] new_keys
      # @raise [RepeatedAttributeError] when trying to define attribute with the
      #   same name as previously defined one
      def check_schema_duplication(new_keys)
        overlapping_keys = new_keys & (attribute_names - superclass.attribute_names)

        if overlapping_keys.any?
          raise RepeatedAttributeError, overlapping_keys.first
        end
      end
      private :check_schema_duplication

      # Applies a planned import to the schema, counting shared definitions
      # once and raising {AttributeConflictError} on diverging ones
      #
      # @param [Array<Fragment::Definition>] plan
      # @param [Hash{Symbol => Module}, nil] resolve
      # @return [Dry::Struct]
      def import_attributes(plan, resolve: nil)
        additions = {}
        sources = {}
        imported = {}
        resolved = []

        plan.each do |definition|
          name = definition.name
          existing = imported[name] || existing_definition(name)

          if existing.nil?
            additions[definition.schema_key] = definition.type
            sources[name] = definition.source
            imported[name] = definition
            next
          end

          decision = Fragment.adjudicate(
            name,
            incoming: definition,
            existing: existing,
            replaceable: imported.key?(name) || !own_attribute?(name),
            resolve: resolve
          )
          resolved << name unless decision.equal?(:dedup)

          next unless decision.equal?(:use_incoming)

          additions[definition.schema_key] = definition.type
          sources[name] = definition.source
          imported[name] = definition
        end

        check_unused_resolutions(resolve, resolved)

        attributes(additions, sources) unless additions.empty?

        self
      end
      private :import_attributes

      # @param [Symbol] name
      # @return [Fragment::Definition, nil] the definition an attribute
      #   currently comes from, if any
      def existing_definition(name)
        source = attribute_sources[name]
        return nil if source.nil? || !has_attribute?(name)

        Fragment::Definition.new(
          name: name,
          type: nil,
          required: schema.key(name).required?,
          source: source
        )
      end
      private :existing_definition

      # @param [Symbol] name
      # @return [Boolean] whether the attribute is defined on this very class
      def own_attribute?(name)
        return false unless attribute_names.include?(name)

        !superclass.respond_to?(:attribute_names) || !superclass.attribute_names.include?(name)
      end
      private :own_attribute?

      # @param [Array<Symbol>] keys
      # @param [Hash{Symbol => Fragment::Source}, nil] sources
      def record_attribute_sources(keys, sources)
        own_sources = @attribute_sources ||= {}

        keys.each do |key|
          own_sources[key] = (sources && sources[key]) || Fragment::Source.new(self, key)
        end
      end
      private :record_attribute_sources

      # @param [Hash{Symbol => Module}, nil] resolve
      # @param [Array<Symbol>] resolved
      def check_unused_resolutions(resolve, resolved)
        return unless resolve

        unused = resolve.keys - resolved
        return if unused.empty?

        raise(
          ::ArgumentError,
          "no conflict to resolve for #{unused.map { |key| ":#{key}" }.join(", ")}"
        )
      end
      private :check_unused_resolutions

      # @param [Hash{Symbol => Object},Dry::Struct] attributes
      # @raise [Struct::Error] if the given attributes don't conform {#schema}
      def new(attributes = default_attributes, safe = false, &)
        if attributes.is_a?(Struct)
          if equal?(attributes.class)
            attributes
          else
            # This implicit coercion is arguable but makes sense overall
            # in cases there you pass child struct to the base struct constructor
            # User.new(super_user)
            #
            # We may deprecate this behavior in future forcing people to be explicit
            new(attributes.to_h, safe, &)
          end
        elsif safe
          load(schema.call_safe(attributes) { |output = attributes| return yield output })
        else
          load(schema.call_unsafe(attributes))
        end
      rescue Types::CoercionError => exception
        raise Error, "[#{self}.new] #{exception}", exception.backtrace
      end

      # @api private
      def call_safe(input, &)
        if input.is_a?(self)
          input
        else
          new(input, true, &)
        end
      end

      # @api private
      def call_unsafe(input)
        if input.is_a?(self)
          input
        else
          new(input)
        end
      end

      # @api private
      def load(attributes)
        struct = allocate
        struct.__send__(:initialize, attributes)
        struct
      end

      # @param [#call,nil] constructor
      # @param [#call,nil] block
      # @return [Dry::Struct::Constructor]
      def constructor(constructor = nil, **, &block)
        Constructor[self, fn: constructor || block]
      end

      # @param [Hash{Symbol => Object},Dry::Struct] input
      # @yieldparam [Dry::Types::Result::Failure] failure
      # @yieldreturn [Dry::Types::Result]
      # @return [Dry::Types::Result]
      def try(input)
        success(self[input])
      rescue Error => exception
        failure_result = failure(input, exception)
        block_given? ? yield(failure_result) : failure_result
      end

      # @param [Hash{Symbol => Object},Dry::Struct] input
      # @return [Dry::Types::Result]
      # @private
      def try_struct(input)
        if input.is_a?(self)
          input
        else
          yield
        end
      end

      # @param [({Symbol => Object})] args
      # @return [Dry::Types::Result::Success]
      def success(*args) = result(Types::Result::Success, *args)

      # @param [({Symbol => Object})] args
      # @return [Dry::Types::Result::Failure]
      def failure(*args) = result(Types::Result::Failure, *args)

      # @param [Class] klass
      # @param [({Symbol => Object})] args
      def result(klass, *args) = klass.new(*args)

      # @return [false]
      def default? = false

      # @param [Object, Dry::Struct] other
      # @return [Boolean]
      def ===(other) = other.is_a?(self)
      alias_method :primitive?, :===

      # @return [true]
      def constrained? = true

      # @return [self]
      def primitive = self

      # @return [false]
      def optional? = false

      # @return [Proc]
      def to_proc
        @to_proc ||= proc { |input| call(input) }
      end

      # Checks if this {Struct} has the given attribute
      #
      # @param [Symbol] key Attribute name
      # @return [Boolean]
      def has_attribute?(key) = schema.key?(key)

      # Gets the list of attribute names
      #
      # @return [Array<Symbol>]
      def attribute_names
        @attribute_names ||= schema.map(&:name)
      end

      # Returns the origins of the attributes, mapping each attribute name
      # to the {Fragment::Source} it was defined by (or first imported from)
      #
      # @return [Hash{Symbol => Fragment::Source}]
      def attribute_sources
        own = @attribute_sources || EMPTY_HASH

        if superclass.respond_to?(:attribute_sources)
          superclass.attribute_sources.merge(own)
        else
          own.dup
        end
      end

      # Returns the origin of a single attribute
      #
      # @param [Symbol] name
      # @return [Fragment::Source, nil]
      def attribute_source(name) = attribute_sources[name]

      # @return [{Symbol => Object}]
      def meta(meta = Undefined)
        if meta.equal?(Undefined)
          schema.meta
        elsif meta.empty?
          self
        else
          ::Class.new(self) do
            schema schema.meta(meta) unless meta.empty?
          end
        end
      end

      # Build a sum type
      # @param [Dry::Types::Type] type
      # @return [Dry::Types::Sum]
      def |(type)
        if type.is_a?(::Class) && type <= Struct
          Sum.new(self, type)
        else
          super
        end
      end

      # Make the struct abstract. This class will be used as a default
      # parent class for nested structs
      def abstract
        abstract_class self
      end

      # Dump to the AST
      #
      # @return [Array]
      #
      # @api public
      def to_ast(meta: true)
        [:struct, [::WeakRef.new(self), schema.to_ast(meta: meta)]]
      end

      # Stores an object for building nested struct classes
      # @return [StructBuilder]
      def struct_builder
        @struct_builder ||= StructBuilder.new(self).freeze
      end
      private :struct_builder

      # Retrieves default attributes from defined {.schema}.
      # Used in a {Struct} constructor if no attributes provided to {.new}
      #
      # @return [Hash{Symbol => Object}]
      def default_attributes(default_schema = schema)
        default_schema.each_with_object({}) do |key, result|
          result[key.name] = default_attributes(key.schema) if struct?(key.type)
        end
      end
      private :default_attributes

      # Checks if the given type is a Dry::Struct
      #
      # @param [Dry::Types::Type] type
      # @return [Boolean]
      def struct?(type)
        type.is_a?(::Class) && type <= Struct
      end
      private :struct?

      # Constructs a type
      #
      # @return [Dry::Types::Type, Dry::Struct]
      def build_type(name, type = Undefined, &)
        type_object =
          if type.is_a?(::String)
            Types[type]
          elsif !block_given? && Undefined.equal?(type)
            raise(
              ::ArgumentError,
              "you must supply a type or a block to `Dry::Struct.attribute`"
            )
          else
            type
          end

        if block_given?
          struct_builder.(name, type_object, &)
        else
          type_object
        end
      end
      private :build_type

      # @api private
      def define_accessors(keys)
        (keys - instance_methods).each do |key|
          if valid_method_name?(key)
            class_eval(<<-RUBY, __FILE__, __LINE__ + 1)
              def #{key}                      # def email
                @attributes[#{key.inspect}]   #   @attributes[:email]
              end                             # end
            RUBY
          else
            define_method(key) { @attributes[key] }
          end
        end
      end
      private :define_accessors

      # @api private
      private def valid_method_name?(key) = key.to_s.match?(/\A[a-zA-Z_]\w*\z/)
    end
  end
end
