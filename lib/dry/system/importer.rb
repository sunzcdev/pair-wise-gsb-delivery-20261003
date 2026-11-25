# frozen_string_literal: true

require "dry/system/constants"

module Dry
  module System
    # Default importer implementation
    #
    # This is currently configured by default for every System::Container.
    # Importer objects are responsible for importing components from one
    # container to another. This is used in cases where an application is split
    # into multiple sub-systems.
    #
    # @api private
    class Importer
      # @api private
      class Item
        attr_reader :namespace, :container, :import_keys

        def initialize(namespace:, container:, import_keys:)
          @namespace = namespace
          @container = container
          @import_keys = import_keys
        end
      end

      attr_reader :container

      attr_reader :registry

      attr_reader :imported_keys

      # @api private
      def initialize(container)
        @container = container
        @registry = {}
        @imported_keys = Hash.new { |h, k| h[k] = [] }
      end

      # @api private
      def register(namespace:, container:, keys: nil)
        registry[namespace_key(namespace)] = Item.new(
          namespace: namespace,
          container: container,
          import_keys: keys
        )
      end

      # @api private
      def [](name)
        registry.fetch(namespace_key(name))
      end

      # @api private
      def key?(name)
        registry.key?(namespace_key(name))
      end
      alias_method :namespace?, :key?

      # @api private
      def finalize!
        registry.each_key { import(_1) }
        self
      end

      # @api private
      def import(namespace, keys: Undefined)
        item = self[namespace]
        keys = Undefined.default(keys, item.import_keys)

        if keys
          import_keys(item.container, namespace, keys_to_import(keys, item))
        else
          import_all(item.container, namespace)
        end

        self
      end

      # Explicitly rebuilds the given imported keys from their source container,
      # replacing any previously imported registrations.
      #
      # Only keys declared for import from the given namespace can be rebound; the
      # source of any other keys is left untouched. A key declared but not yet
      # lazily loaded is imported for the first time. If one of the keys can no
      # longer be resolved from the source container, no keys are replaced and an
      # {ImportedKeyNotFoundError} is raised.
      #
      # @api private
      def rebind(namespace, keys:, source: Undefined)
        item = self[namespace]
        ns_key = namespace_key(namespace)

        unless Undefined.equal?(source) || item.container.equal?(source)
          raise ArgumentError, "Import namespace #{namespace.inspect} is not registered for the given source container"
        end

        keys = Array(keys).map(&:to_s)

        unknown_keys = keys - rebindable_keys(item, ns_key)
        unless unknown_keys.empty?
          raise ImportedKeyNotFoundError.new(unknown_keys.first, namespace)
        end

        missing_keys = keys.reject { |key| item.container.key?(key) }
        unless missing_keys.empty?
          raise ImportedKeyNotFoundError.new(missing_keys.first, namespace)
        end

        # Build every replacement before replacing anything, so a failure to
        # rebuild one key leaves all existing registrations untouched. Keys not
        # loaded yet are imported for the first time; already-imported keys have
        # their old registrations removed before being rebuilt.
        merge_container = build_merge_container(item.container, keys)

        stale_keys = keys.select { |key| imported_keys[ns_key].include?(key) }
        stale_keys.each { |key| unregister_imported(namespace, key) }

        merge(container, merge_container, namespace: namespace)
        track_imported_keys(namespace, merge_container._container.keys)

        self
      end

      private

      # Returns nil if given a nil (i.e. root) namespace. Otherwise, converts the namespace to a
      # string.
      #
      # This ensures imported components are found when either symbols or strings to given as the
      # namespace in {Container.import}.
      def namespace_key(namespace)
        return nil if namespace.nil?

        namespace.to_s
      end

      def keys_to_import(keys, item)
        keys
          .then { (arr = item.import_keys) ? _1 & arr : _1 }
          .then { (arr = item.container.exports) ? _1 & arr : _1 }
      end

      # The keys eligible for rebinding: those already imported, plus any keys
      # declared for import that have not been lazily loaded yet.
      def rebindable_keys(item, ns_key)
        declared = item.import_keys || item.container.keys
        declared = declared & item.container.exports if item.container.exports

        imported_keys[ns_key] | declared
      end

      def import_keys(other, namespace, keys)
        merge_container = build_merge_container(other, keys)
        track_imported_keys(namespace, merge_container._container.keys)
        merge(container, merge_container, namespace: namespace)
      end

      def import_all(other, namespace)
        merge_container =
          if other.exports
            build_merge_container(other, other.exports)
          else
            build_merge_container(other.finalize!, other.keys)
          end

        track_imported_keys(namespace, merge_container._container.keys)
        merge(container, merge_container, namespace: namespace)
      end

      def track_imported_keys(namespace, keys)
        imported_keys[namespace_key(namespace)].concat(keys).uniq!
      end

      def unregister_imported(namespace, key)
        namespaced = [namespace, key].compact.join(KEY_SEPARATOR)
        container._container.delete(namespaced)
        imported_keys[namespace_key(namespace)].delete(key)
      end

      # Merges `other` into `container`, favoring the container's existing registrations
      def merge(container, other, namespace:)
        container.merge(other, namespace: namespace) { |_key, old_item, new_item|
          old_item || new_item
        }
      end

      def build_merge_container(other, keys)
        keys.each_with_object(Core::Container.new) { |key, ic|
          next unless other.key?(key)

          # Access the other container's items directly so that we can preserve all their
          # options when we merge them with the target container (e.g. if a component in
          # the provider container was registered with a block, we want block registration
          # behavior to be exhibited when later resolving that component from the target
          # container). TODO: Make this part of dry-system's public API.
          item = other._container[key]

          # By default, we "protect" components that were themselves imported into the
          # other container from being implicitly exported; imported components are
          # considered "private" and must be explicitly included in `exports` to be
          # exported.
          next if item.options[:imported] && !other.exports

          if item.callable?
            ic.register(key, **item.options, imported: true, &item.item)
          else
            ic.register(key, item.item, **item.options, imported: true)
          end
        }
      end
    end
  end
end
