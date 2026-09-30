# frozen_string_literal: true

require "concurrent/hash"
require "dry/container/dependency_state"

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
            subclass.instance_variable_set(
              :@_dependency_state,
              @_dependency_state.dup
            )
            super
          end
        end

        base.class_eval do
          extend Configuration
          extend hooks_mod

          @_container = ::Concurrent::Hash.new
          @_dependency_state = ::Dry::Container::DependencyState.new
        end
      end

      # @private
      module Initializer
        def initialize(*args, &block)
          @_container = ::Concurrent::Hash.new
          @_dependency_state = ::Dry::Container::DependencyState.new
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

        key_s = key.to_s
        options = options.dup

        config.registry.call(_container, key, item, options)

        _dependency_state.publish_edges(key_s, Array(options[:depends_on]))
        _dependency_state.bump(key_s)

        self
      rescue FrozenError
        raise FrozenError,
              "can't modify frozen #{self.class} (when attempting to register '#{key}')"
      end

      # Resolve an item from the container
      #
      # The resolution is consistent: if one of the key's explicit
      # dependencies is replaced while it is being constructed, the
      # whole construction is retried, so a single resolution can never
      # observe a half-old/half-new set of dependencies.
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
        _dependency_state.resolve(self, key, &block)
      end

      # Replace a previously registered item, taking its explicit
      # dependencies into account.
      #
      # Memoized values of the replaced key and of every key that
      # (transitively) depends on it are invalidated: the next resolution
      # rebuilds them. Keys unrelated to the replacement keep their
      # identity. A factory already running against the old dependencies
      # is allowed to finish, but its result is never published, and a
      # resolution racing the replacement retries rather than observing a
      # mix of old and new dependencies.
      #
      # If the replacement fails (unknown key or unknown declared
      # dependency), all existing registrations are left untouched.
      #
      # @param [Mixed] key
      #   The key to replace
      # @param [Mixed] contents
      #   The new item to register (if no block given)
      # @param [Hash] options
      #   Options for the new registration; +depends_on+ can be given to
      #   declare which other keys this item depends on, and +memoize+
      #   follows the same semantics as for {#register}
      # @yield
      #   If a block is given, contents will be ignored and the block
      #   will be registered instead
      #
      # @raise [KeyError]
      #   If no item is registered with the given key
      # @raise [Dry::Container::Error]
      #   If a declared dependency is not registered
      #
      # @return [Dry::Container::Mixin] self
      #
      # @api public
      def replace(key, contents = nil, options = EMPTY_HASH, &block)
        if block_given?
          item = block
          options = contents.is_a?(::Hash) ? contents.dup : {}
        else
          item = contents
          options = options.dup
        end

        key_s = key.to_s

        unless _container.key?(key_s)
          raise ::Dry::Container::KeyError.new(
            %(key not found: "#{key_s}"), key: key_s, receiver: _container
          )
        end

        declared = options[:depends_on] ? Array(options[:depends_on]).map(&:to_s) : nil

        # Validate everything before mutating anything: a failed
        # replacement keeps the previous, working set of registrations
        unless declared.nil?
          unknown = declared.reject { |dep| _container.key?(dep) }
          unless unknown.empty?
            raise Error,
                  "cannot replace #{key_s.inspect}: undeclared dependency key(s) " \
                  "#{unknown.map(&:inspect).join(', ')} are not registered"
          end
        end

        deps = declared || _dependency_state.dependencies(key_s)

        # Dependency edges are container-managed metadata: keep the old
        # ones unless the replacement explicitly declares a new set.
        new_options = options.dup
        new_options[:depends_on] = deps unless new_options.key?(:depends_on)

        # Building the new item outside the transaction means a failing
        # factory (e.g. memoize: true for a plain value) changes nothing
        new_item = config.registry.factory.call(item, new_options)

        _dependency_state.mutex.synchronize do
          # Computed under the lock so two concurrent replacements cannot
          # invalidate closures against a stale dependency graph
          affected = _dependency_state.dependent_closure(key_s)

          # Invalidate before swapping the slot: an in-flight builder of
          # any affected key finishes, but its stale result is discarded
          affected.each do |affected_key|
            affected_item = _container[affected_key]
            affected_item.invalidate! if affected_item.respond_to?(:invalidate!)
          end

          _container[key_s] = new_item
          _dependency_state.publish_edges(key_s, deps)
          _dependency_state.bump(key_s)
          affected.each do |affected_key|
            _dependency_state.bump(affected_key) unless affected_key == key_s
          end
        end

        self
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
        if namespace
          _container.merge!(
            other._container.each_with_object(::Concurrent::Hash.new) { |(key, item), hsh|
              hsh[PREFIX_NAMESPACE.call(namespace, key, config)] = item
            },
            &block
          )
        else
          _container.merge!(other._container, &block)
        end

        self
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

      # @private
      def _dependency_state
        @_dependency_state ||= ::Dry::Container::DependencyState.new
      end

      # @api public
      def dup
        copy = super
        copy.instance_variable_set(:@_container, _container.dup)
        copy.instance_variable_set(:@_dependency_state, _dependency_state.dup)
        copy
      end

      # @api public
      def clone
        copy = super
        unless copy.frozen?
          copy.instance_variable_set(:@_container, _container.dup)
          copy.instance_variable_set(:@_dependency_state, _dependency_state.dup)
        end
        copy
      end
    end
    # rubocop:enable Metrics/ModuleLength
  end
end
