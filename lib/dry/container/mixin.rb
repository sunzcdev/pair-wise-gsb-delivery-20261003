# frozen_string_literal: true

require "concurrent/hash"

require "dry/container/collection"
require "dry/container/member"

module Dry
  class Container
    # @api public
    class Config
      DEFAULT_NAMESPACE_SEPARATOR = "."
      DEFAULT_RESOLVER = Resolver.new
      DEFAULT_REGISTRY = Registry.new

      # @api public
      attr_accessor :namespace_separator

      # @api public
      attr_accessor :resolver

      # @api public
      attr_accessor :registry

      # @api private
      def initialize(
        namespace_separator: DEFAULT_NAMESPACE_SEPARATOR,
        resolver: DEFAULT_RESOLVER,
        registry: DEFAULT_REGISTRY
      )
        @namespace_separator = namespace_separator
        @resolver = resolver
        @registry = registry
      end
    end

    # @api public
    module Configuration
      # Use dry/configurable if it's available
      begin
        require "dry/configurable"

        # @api private
        def self.extended(klass)
          super
          klass.class_eval do
            extend Dry::Configurable

            setting :namespace_separator, default: Config::DEFAULT_NAMESPACE_SEPARATOR
            setting :resolver, default: Config::DEFAULT_RESOLVER
            setting :registry, default: Config::DEFAULT_REGISTRY
          end
        end
      rescue LoadError
        # @api private
        def config
          @config ||= Config.new
        end
      end

      # @api private
      def configure
        yield config
      end
    end

    PREFIX_NAMESPACE = lambda do |namespace, key, config|
      [namespace, key].join(config.namespace_separator)
    end

    EMPTY_HASH = {}.freeze

    # Mixin to expose Inversion of Control (IoC) container behaviour
    #
    # @example
    #
    #   class MyClass
    #     extend Dry::Container::Mixin
    #   end
    #
    #   MyClass.register(:item, 'item')
    #   MyClass.resolve(:item)
    #   => 'item'
    #
    #   class MyObject
    #     include Dry::Container::Mixin
    #   end
    #
    #   container = MyObject.new
    #   container.register(:item, 'item')
    #   container.resolve(:item)
    #   => 'item'
    #
    # @api public
    #
    # rubocop:disable Metrics/ModuleLength
    module Mixin
      # @private
      def self.extended(base)
        hooks_mod = ::Module.new do
          def inherited(subclass)
            subclass.instance_variable_set(:@_container, @_container.dup)
            super
          end
        end

        base.class_eval do
          extend Configuration
          extend hooks_mod

          @_container = ::Concurrent::Hash.new
        end
      end

      # @private
      module Initializer
        def initialize(*args, &block)
          @_container = ::Concurrent::Hash.new
          super
        end
      end

      # @private
      def self.included(base)
        base.class_eval do
          extend Configuration
          prepend Initializer

          def config
            self.class.config
          end
        end
      end

      # Register an item with the container to be resolved later
      #
      # @param [Mixed] key
      #   The key to register the container item with (used to resolve)
      # @param [Mixed] contents
      #   The item to register with the container (if no block given)
      # @param [Hash] options
      #   Options to pass to the registry when registering the item
      # @yield
      #   If a block is given, contents will be ignored and the block
      #   will be registered instead
      #
      # @return [Dry::Container::Mixin] self
      #
      # @api public
      def register(key, contents = nil, options = EMPTY_HASH, &block)
        if block_given?
          item = block
          options = contents if contents.is_a?(::Hash)
        else
          item = contents
        end

        config.registry.call(_container, key, item, options)

        self
      rescue FrozenError
        raise FrozenError,
              "can't modify frozen #{self.class} (when attempting to register '#{key}')"
      end

      # Bind a named member of a multibinding collection
      #
      # @param [Mixed] tag
      #   The collection key the member belongs to; used to resolve the set
      # @param [Mixed] name
      #   Stable name of the member within the collection
      # @param [Mixed] contents
      #   The item to register (if no block given)
      # @param [Hash] options
      #   Options to pass to the registry when registering the member
      # @option options [Symbol, Array] :tags
      #   Additional tags the member can be filtered by
      # @option options [Integer] :order
      #   Explicit weight used to order the set
      # @option options [String] :source
      #   Origin of the binding, used in name-conflict errors
      # @yield
      #   If a block is given, contents will be ignored and the block is used
      #
      # @return [Dry::Container::Mixin] self
      #
      # @api public
      def bind(tag, name, contents = nil, options = EMPTY_HASH, &block)
        if block_given?
          item = block
          options = contents if contents.is_a?(::Hash)
        else
          item = contents
        end

        options = options.merge(namespace_separator: config.namespace_separator)

        config.registry.bind(_container, tag, name, item, options)

        self
      rescue FrozenError
        raise FrozenError,
              "can't modify frozen #{self.class} " \
              "(when attempting to bind '#{name}' to '#{tag}')"
      end

      # Resolve an item from the container
      #
      # @param [Mixed] key
      #   The key for the item you wish to resolve
      # @yield
      #   Fallback block to call when a key is missing. Its result will be returned
      # @yieldparam [Mixed] key Missing key
      #
      # @return [Mixed]
      #
      # @api public
      def resolve(key, &block)
        config.resolver.call(_container, key, &block)
      end

      # Resolve a multibinding collection into an ordered array of members
      #
      # @param [Mixed] tag
      #   The collection key to resolve
      # @param [Array<Mixed>] tags
      #   Extra tags to filter the members by
      # @yield [Dry::Container::Member]
      #   Optional filter evaluated against member metadata before calling, so
      #   factories of unselected members never run
      #
      # @return [Array<Mixed>]
      #
      # @api public
      def resolve_all(tag, *tags, &filter)
        config.resolver.resolve_all(_container, tag, tags, &filter)
      end

      # Resolve an item from the container
      #
      # @param [Mixed] key
      #   The key for the item you wish to resolve
      #
      # @return [Mixed]
      #
      # @api public
      # @see Dry::Container::Mixin#resolve
      def [](key)
        resolve(key)
      end

      # Merge in the items of the other container
      #
      # @param [Dry::Container] other
      #   The other container to merge in
      # @param [Symbol, nil] namespace
      #   Namespace to prefix other container items with, defaults to nil
      #
      # @return [Dry::Container::Mixin] self
      #
      # @api public
      def merge(other, namespace: nil, &block)
        target = _container
        separator = config.namespace_separator

        collections, others = other._container.partition { |_, entry|
          entry.is_a?(::Dry::Container::Collection)
        }

        others.each do |key, entry|
          next if entry.is_a?(::Dry::Container::Member)

          prefixed_key = namespace ? PREFIX_NAMESPACE.call(namespace, key, config) : key
          merge_item(target, prefixed_key, entry, &block)
        end

        collections.each do |key, collection|
          prefixed_key = namespace ? PREFIX_NAMESPACE.call(namespace, key, config) : key
          merge_collection(
            target, prefixed_key, collection, namespace: namespace, separator: separator
          )
        end

        self
      end

      # @api private
      def merge_collection(target, key, collection, namespace:, separator:)
        existing = target[key]

        incoming =
          if namespace
            collection.prefix(namespace, separator)
          else
            collection
          end

        if existing.nil?
          target[key] = incoming
          owner = incoming
        elsif existing.is_a?(::Dry::Container::Collection)
          incoming.members.each { |member| existing.add(member) }
          owner = existing
        else
          raise Error,
                "Cannot merge collection #{key.inspect}: the key is already used " \
                "by a regular item"
        end

        owner.members.each do |member|
          member_key = [key, member.name].join(separator)
          target[member_key] = owner.member(member.name)
        end
      end

      # @api private
      def merge_item(target, key, item)
        if target.key?(key)
          if block_given?
            target[key] = yield(key, target[key], item)
          else
            raise Error, "There is already an item registered with the key #{key.inspect}"
          end
        else
          target[key] = item
        end
      end

      # Check whether an item is registered under the given key
      #
      # @param [Mixed] key
      #   The key you wish to check for registration with
      #
      # @return [Bool]
      #
      # @api public
      def key?(key)
        config.resolver.key?(_container, key)
      end

      # An array of registered names for the container
      #
      # @return [Array<String>]
      #
      # @api public
      def keys
        config.resolver.keys(_container)
      end

      # Calls block once for each key in container, passing the key as a parameter.
      #
      # If no block is given, an enumerator is returned instead.
      #
      # @return [Dry::Container::Mixin] self
      #
      # @api public
      def each_key(&block)
        config.resolver.each_key(_container, &block)
        self
      end

      # Calls block once for each key/value pair in the container, passing the key and
      # the registered item parameters.
      #
      # If no block is given, an enumerator is returned instead.
      #
      # @return [Enumerator]
      #
      # @api public
      #
      # @note In discussions with other developers, it was felt that being able to iterate
      #       over not just the registered keys, but to see what was registered would be
      #       very helpful. This is a step toward doing that.
      def each(&block)
        config.resolver.each(_container, &block)
      end

      # Decorates an item from the container with specified decorator
      #
      # @return [Dry::Container::Mixin] self
      #
      # @api public
      def decorate(key, with: nil, &block)
        key = key.to_s
        original = _container.delete(key) do
          raise Error, "Nothing registered with the key #{key.inspect}"
        end

        if with.is_a?(Class)
          decorator = with.method(:new)
        elsif block.nil? && !with.respond_to?(:call)
          raise Error, "Decorator needs to be a Class, block, or respond to the `call` method"
        else
          decorator = with || block
        end

        _container[key] = original.map(decorator)
        self
      end

      # Evaluate block and register items in namespace
      #
      # @param [Mixed] namespace
      #   The namespace to register items in
      #
      # @return [Dry::Container::Mixin] self
      #
      # @api public
      def namespace(namespace, &block)
        ::Dry::Container::NamespaceDSL.new(
          self,
          namespace,
          config.namespace_separator,
          &block
        )

        self
      end

      # Import a namespace
      #
      # @param [Dry::Container::Namespace] namespace
      #   The namespace to import
      #
      # @return [Dry::Container::Mixin] self
      #
      # @api public
      def import(namespace)
        namespace(namespace.name, &namespace.block)

        self
      end

      # Freeze the container. Nothing can be registered after freezing
      #
      # @api public
      def freeze
        super
        _container.freeze
        self
      end

      # @private no, really
      def _container
        @_container
      end

      # @api public
      def dup
        copy = super
        copy.instance_variable_set(:@_container, copied_container)
        copy
      end

      # @api public
      def clone
        copy = super
        unless copy.frozen?
          copy.instance_variable_set(:@_container, copied_container)
        end
        copy
      end

      private

      # Build an independent copy of the underlying storage. Collections and
      # their members are mutable and shared between several keys, so a shallow
      # hash copy would let duplicates and mutations leak across containers.
      #
      # @api private
      def copied_container
        copied = ::Concurrent::Hash.new
        collections = {}

        _container.each do |key, entry|
          case entry
          when ::Dry::Container::Collection
            duplicate = entry.dup
            collections[key] = duplicate
            copied[key] = duplicate
          when ::Dry::Container::Member
            next
          else
            copied[key] = entry
          end
        end

        collections.each do |tag, collection|
          collection.members.each do |member|
            copied[[tag, member.name].join(config.namespace_separator)] =
              collection.member(member.name)
          end
        end

        copied
      end
    end
    # rubocop:enable Metrics/ModuleLength
  end
end
