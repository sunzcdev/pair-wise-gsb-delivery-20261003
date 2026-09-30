# frozen_string_literal: true

module Dry
  class Struct
    # A reusable group of attributes not tied to any particular struct.
    #
    # Fragments preserve the {Origin origin} of every attribute. Importing the
    # same original definition through two import paths is deduplicated, while
    # same-named attributes coming from distinct definitions must be resolved
    # explicitly.
    #
    # @api public
    class AttributeFragment
      # Identity of a single attribute definition.
      #
      # @!attribute source
      #   @return [Dry::Struct::AttributeFragment, Class<Dry::Struct>]
      # @!attribute original_name
      #   @return [Symbol] attribute name as declared at the source
      # @!attribute location
      #   @return [String, nil] file:line of the definition, for diagnostics
      Origin = ::Data.define(:source, :original_name, :location) do
        def describe
          case source
          when AttributeFragment
            source_name = source.name || "anonymous attribute fragment"
            "attribute :#{original_name} from #{source_name}"
          else
            "attribute :#{original_name} from #{source}"
          end
        end

        def eql?(other)
          other.is_a?(Origin) &&
            source.equal?(other.source) &&
            original_name.equal?(other.original_name)
        end

        alias_method :==, :eql?

        def hash
          [source.object_id, original_name].hash
        end
      end

      # A fragment entry positioned at a (possibly nested) namespace path.
      #
      # @!attribute chomped_name [Symbol] name without the omittable `?`
      # @!attribute key [Dry::Types::Schema::Key] the key, carrying requiredness
      # @!attribute origin [Origin]
      # @!attribute path [Array<Symbol>] namespace prefix
      Entry = ::Data.define(:chomped_name, :key, :origin, :path) do
        def leaf? = path.empty?

        def first_namespace = path.first

        def descend
          with(path: path[1..])
        end
      end

      # @return [Symbol, String, nil]
      attr_reader :name

      # @param name [Symbol, String, nil] label used in diagnostics
      # @api private
      def initialize(name = nil)
        @name = name
        @entries = []
        @index = {}
      end

      # @api private
      def add_entry(chomped_name, key, origin, path = EMPTY_ARRAY)
        index_key = [path, chomped_name]

        if (existing = @index[index_key])
          return if existing.origin.eql?(origin)

          raise AttributeConflictError.build(
            attribute: chomped_name,
            definitions: [existing.origin, origin]
          )
        end

        entry = Entry.new(chomped_name, key, origin, path)
        @entries << entry
        @index[index_key] = entry
        self
      end

      # @return [Array<Entry>]
      # @api private
      def entries = @entries

      # @api private
      def build_type(name, type = Undefined, &block)
        host.__send__(:build_type, name, type, &block)
      end

      # Anonymous struct class reused for building nested struct types.
      # @return [Class<Dry::Struct>]
      # @api private
      def host
        @host ||= ::Class.new(Struct) { abstract }
      end

      # Evaluate the fragment definition
      # @api private
      def define(&block)
        Evaluator.new(self).instance_exec(&block)
        @entries.freeze
        @index.freeze
        freeze
        self
      end

      # Context used while evaluating a fragment definition block.
      #
      # @api private
      class Evaluator
        # @return [AttributeFragment]
        attr_reader :fragment

        # @param fragment [AttributeFragment]
        def initialize(fragment)
          @fragment = fragment
        end

        # Add an attribute to the fragment.
        def attribute(name, type = Undefined, &block)
          add_attribute(name, type, required: true, &block)
        end

        # Add an omittable attribute to the fragment.
        def attribute?(name, type = Undefined, &block)
          add_attribute(name, type, required: false, &block)
        end

        # Compose another fragment (or struct) into this fragment.
        def attributes_from(source, **options)
          AttributeFragment.import_entries(source, **options).each do |entry|
            fragment.add_entry(
              entry.chomped_name,
              entry.key,
              entry.origin,
              entry.path
            )
          end
        end

        private

        def add_attribute(name, type, required:, &block)
          chomped_name = name.to_s.chomp("?").to_sym
          if fragment.entries.any? { |entry| entry.leaf? && entry.chomped_name == chomped_name }
            raise RepeatedAttributeError, chomped_name
          end

          location = AttributeFragment.user_location
          built_type = fragment.build_type(chomped_name, type, &block)
          key_name = required ? chomped_name : :"#{chomped_name}?"
          key = Types::Schema::Key.new(built_type, key_name, required: required)
          origin = Origin.new(fragment, chomped_name, location)
          fragment.add_entry(chomped_name, key, origin)
        end
      end

      # Path to this gem's `lib` directory, used to strip internal frames.
      # @return [String]
      # @api private
      GEM_LIB = ::File.expand_path("..", __dir__).freeze

      # @return [String, nil] the first caller frame outside of dry-struct
      # @api private
      def self.user_location
        location = caller_locations(2, 20)&.find do |frame|
          path = frame.absolute_path || frame.path
          path && !path.start_with?(GEM_LIB)
        end
        location && "#{location.path}:#{location.lineno}"
      end

      # Produce positioned, renamed and namespaced entries from a source.
      #
      # @param source [AttributeFragment, Class<Dry::Struct>]
      # @param only [Array<Symbol>, nil] keep only these (chomped) names
      # @param except [Array<Symbol>, nil] drop these (chomped) names
      # @param rename [Hash{Symbol => Symbol}] map chomped names to new names
      # @param namespace [Symbol, nil] nest entries under this attribute
      # @return [Array<Entry>]
      # @api private
      def self.import_entries(
        source,
        only: nil,
        except: nil,
        rename: ::Dry::Core::Constants::EMPTY_HASH,
        namespace: nil
      )
        entries = source_entries(source)

        selectable = selectable_names(entries)
        wanted = normalize_name_list(only, selectable, label: "only")
        excluded = normalize_name_list(except, selectable, label: "except")
        validate_rename(rename, selectable)

        entries = entries.dup
        entries.select! { |entry| wanted.include?(selection_name(entry)) } if wanted
        entries.reject! { |entry| excluded.include?(selection_name(entry)) } if excluded

        entries.map do |entry|
          original_name = selection_name(entry)
          new_name = rename.fetch(original_name, original_name)
          renamed = rename.key?(original_name)

          if entry.leaf?
            key_name = entry.key.required? ? new_name : :"#{new_name}?"
            key = entry.key.with(name: key_name)
            leaf_name = renamed ? new_name : entry.chomped_name
          else
            key = entry.key
            leaf_name = entry.chomped_name
          end

          path =
            if entry.leaf?
              namespace ? [namespace] : EMPTY_ARRAY
            else
              tail = entry.path.dup
              tail[0] = new_name if renamed
              namespace ? [namespace, *tail] : tail
            end

          Entry.new(leaf_name, key, entry.origin, path)
        end
      end

      # Names selectable by `only:`/`except:`/`rename:`: the top-level entry
      # name, which is the namespace name for already-nested entries.
      #
      # @api private
      def self.selection_name(entry)
        entry.leaf? ? entry.chomped_name : entry.first_namespace
      end

      # @api private
      def self.selectable_names(entries)
        entries.map { |entry| selection_name(entry) }.uniq
      end

      # @api private
      def self.source_entries(source)
        if source.is_a?(AttributeFragment)
          source.entries
        elsif source.is_a?(::Class) && source <= Struct
          source.schema.keys.map do |key|
            chomped = key.name.to_s.chomp("?").to_sym
            origin =
              source.own_attribute_origin(chomped) ||
              Origin.new(source, chomped, source.attribute_location)
            Entry.new(chomped, key, origin, EMPTY_ARRAY)
          end
        else
          raise(
            ::ArgumentError,
            "#{source.inspect} is neither an attribute fragment nor a Dry::Struct"
          )
        end
      end

      # @api private
      def self.normalize_name_list(names, available, label:)
        return nil if names.nil?

        list = Array(names).map { |name| name.to_s.chomp("?").to_sym }
        unknown = list - available

        if unknown.any?
          raise(
            ::ArgumentError,
            "unknown attribute#{unknown.size > 1 ? "s" : ""} " \
            "#{unknown.map(&:inspect).join(", ")} passed to `#{label}:` " \
            "(available: #{available.map(&:inspect).join(", ")})"
          )
        end

        list
      end

      # @api private
      def self.validate_rename(rename, available)
        return if rename.empty?

        unknown = rename.keys.map { |name| name.to_s.chomp("?").to_sym } - available

        if unknown.any?
          raise(
            ::ArgumentError,
            "unknown attribute#{unknown.size > 1 ? "s" : ""} " \
            "#{unknown.map(&:inspect).join(", ")} passed to `rename:` " \
            "(available: #{available.map(&:inspect).join(", ")})"
          )
        end
      end
    end
  end
end
