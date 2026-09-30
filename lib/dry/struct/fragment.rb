# frozen_string_literal: true

module Dry
  class Struct
    # Reusable fragment of attribute definitions that can be imported into
    # {Struct} classes (or other fragments) via
    # {ClassInterface#attributes_from}.
    #
    # Every attribute keeps track of the fragment (or struct) it originally
    # came from. Reaching the *same* definition through multiple import
    # paths counts once, while same-named attributes coming from *different*
    # definitions raise an {AttributeConflictError} — even when their types
    # happen to be identical — and must be handled explicitly.
    #
    # @example
    #   module Timestamps
    #     include Dry::Struct::Fragment
    #
    #     attribute :created_at, "time"
    #     attribute? :archived_at, "time"
    #   end
    #
    #   class Event < Dry::Struct
    #     attributes_from Timestamps
    #   end
    #
    #   Event.attribute_source(:created_at).owner # => Timestamps
    module Fragment
      # Identity of the definition an attribute originates from. Two sources
      # are equal only when they point at the same attribute of the same
      # fragment or struct, which is what makes diamond-shaped reuse count
      # once while genuinely different definitions conflict.
      class Source
        # @return [Module, Class] fragment or struct owning the definition
        attr_reader :owner

        # @return [Symbol] attribute name within the owning definition
        attr_reader :name

        # @return [Thread::Backtrace::Location, nil] where the attribute was defined
        attr_reader :location

        def initialize(owner, name, location: nil)
          @owner = owner
          @name = name
          @location = location
        end

        def ==(other)
          other.is_a?(Source) && other.owner.equal?(owner) && other.name == name
        end
        alias_method :eql?, :==

        def hash = [owner, name].hash

        def to_s
          if location
            "#{owner}##{name} (defined at #{location.path}:#{location.lineno})"
          else
            "#{owner}##{name}"
          end
        end
        alias_method :inspect, :to_s
      end

      # A single attribute definition, carrying its type, requiredness and
      # original {Source}
      class Definition
        # @return [Symbol] current (possibly renamed) attribute name
        attr_reader :name

        # @return [Dry::Types::Type, nil]
        attr_reader :type

        # @return [Source]
        attr_reader :source

        def initialize(name:, type:, required:, source:)
          @name = name
          @type = type
          @required = required
          @source = source
        end

        def required? = @required

        # @return [Symbol] key used in schema hashes, `:"name?"` for omittable keys
        def schema_key = @required ? @name : :"#{@name}?"

        # @return [Definition] copy under a new name, keeping the original source
        def renamed(new_name)
          self.class.new(name: new_name, type: @type, required: @required, source: @source)
        end
      end

      # Builds the list of definitions an import would bring in, applying
      # `only:`/`except:` selection and `rename:` mapping. Names in all
      # options refer to the attribute names of the source.
      #
      # @param [Dry::Struct, Fragment] source
      # @return [Array<Definition>]
      def self.plan(source, only: nil, except: nil, rename: nil)
        definitions = definitions_of(source)
        source_names = definitions.map(&:name)

        if only
          only = only.map(&:to_sym)
          unknown_option_keys(:only, only, source_names, source)
          definitions = definitions.select { |definition| only.include?(definition.name) }
        end

        if except
          except = except.map(&:to_sym)
          unknown_option_keys(:except, except, source_names, source)
          definitions = definitions.reject { |definition| except.include?(definition.name) }
        end

        if rename
          rename = rename.to_h { |from, to| [from.to_sym, to.to_sym] }
          unknown_option_keys(:rename, rename.keys, definitions.map(&:name), source)
          definitions = definitions.map do |definition|
            rename.key?(definition.name) ? definition.renamed(rename[definition.name]) : definition
          end
        end

        definitions
      end

      # @param [Dry::Struct, Fragment] source
      # @return [Array<Definition>]
      def self.definitions_of(source)
        if source.is_a?(::Module) && source.respond_to?(:fragment_definitions)
          source.fragment_definitions.values
        elsif source.is_a?(::Class) && source <= Struct
          source.schema.keys.map do |key|
            Definition.new(
              name: key.name,
              type: key.type,
              required: key.required?,
              source: Source.new(source, key.name)
            )
          end
        else
          raise(
            ::ArgumentError,
            "attributes can only be imported from a Dry::Struct or a Dry::Struct::Fragment, " \
            "got #{source.inspect}"
          )
        end
      end

      # Decides how an incoming definition interacts with an existing one
      # under the same name.
      #
      # @return [:dedup] same original definition — count it once
      # @return [:keep_existing] explicit resolution keeps the existing definition
      # @return [:use_incoming] explicit resolution replaces the existing definition
      # @raise [AttributeConflictError] on unresolved conflicts and on
      #   resolutions that would relax requiredness or rewrite a settled definition
      def self.adjudicate(name, incoming:, existing:, replaceable:, resolve: nil)
        return :dedup if existing.source == incoming.source

        sources = [existing.source, incoming.source]
        choice = resolve && resolve[name]

        unless choice
          raise AttributeConflictError.new(name, sources)
        end

        winner, loser =
          if existing.source.owner.equal?(choice)
            [existing, incoming]
          elsif incoming.source.owner.equal?(choice)
            [incoming, existing]
          else
            raise(
              ::ArgumentError,
              "cannot resolve attribute :#{name} with #{choice.inspect}; " \
              "it is defined by #{sources[0]} and #{sources[1]}"
            )
          end

        if loser.required? && !winner.required?
          raise AttributeConflictError.new(
            name,
            sources,
            message: "Attribute :#{name} is defined by both #{sources[0]} and #{sources[1]}; " \
                     "resolving in favor of #{winner.source} would relax requiredness — " \
                     "make the definition required or exclude the other one with `except:`"
          )
        end

        if winner.equal?(incoming)
          unless replaceable
            raise AttributeConflictError.new(
              name,
              sources,
              message: "Attribute :#{name} is defined by both #{sources[0]} and #{sources[1]}; " \
                       "the definition from #{existing.source.owner} is already settled here " \
                       "and cannot be replaced — rename or exclude the incoming attribute instead"
            )
          end

          :use_incoming
        else
          :keep_existing
        end
      end

      def self.unknown_option_keys(option, keys, known, source)
        unknown = keys - known
        return if unknown.empty?

        raise(
          ::ArgumentError,
          "#{source} does not define #{unknown.map { |key| ":#{key}" }.join(", ")} " \
          "referenced in `#{option}:`"
        )
      end
      private_class_method :unknown_option_keys

      def self.included(base)
        base.extend(DSL)
      end

      # Definition DSL mixed into fragments
      module DSL
        # @return [Hash{Symbol => Definition}]
        def fragment_definitions
          @fragment_definitions ||= {}
        end

        # @param [Symbol] name
        # @param [Dry::Types::Type, String] type
        def attribute(name, type = Undefined)
          add_fragment_definition(name, type, required: true, location: caller_locations(1, 1).first)
        end

        # Defines an omittable attribute
        #
        # @param [Symbol] name
        # @param [Dry::Types::Type, String] type
        def attribute?(name, type = Undefined)
          add_fragment_definition(name, type, required: false, location: caller_locations(1, 1).first)
        end

        # Imports definitions from another fragment or struct, preserving
        # their original sources
        #
        # @see ClassInterface#attributes_from
        def attributes_from(source, only: nil, except: nil, rename: nil, resolve: nil)
          plan = Fragment.plan(source, only: only, except: except, rename: rename)
          resolved = []

          plan.each do |definition|
            existing = fragment_definitions[definition.name]

            if existing.nil?
              fragment_definitions[definition.name] = definition
              next
            end

            decision = Fragment.adjudicate(
              definition.name,
              incoming: definition,
              existing: existing,
              replaceable: true,
              resolve: resolve
            )
            resolved << definition.name unless decision.equal?(:dedup)

            fragment_definitions[definition.name] = definition if decision.equal?(:use_incoming)
          end

          check_unused_resolutions(resolve, resolved)

          self
        end

        private

        def add_fragment_definition(name, type, required:, location:)
          if Undefined.equal?(type)
            raise ::ArgumentError, "you must supply a type to `attribute`"
          end

          name = name.to_sym

          if fragment_definitions.key?(name)
            raise RepeatedAttributeError, name
          end

          type = Types[type] if type.is_a?(::String)
          source = Source.new(self, name, location: location)
          fragment_definitions[name] =
            Definition.new(name: name, type: type, required: required, source: source)
        end

        def check_unused_resolutions(resolve, resolved)
          return unless resolve

          unused = resolve.keys - resolved
          return if unused.empty?

          raise(
            ::ArgumentError,
            "no conflict to resolve for #{unused.map { |key| ":#{key}" }.join(", ")}"
          )
        end
      end
    end
  end
end
