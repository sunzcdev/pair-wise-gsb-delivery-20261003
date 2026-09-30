# frozen_string_literal: true

require "dry/container/item/multi"

module Dry
  class Container
    # Default resolver for resolving items from container
    #
    # @api public
    class Resolver
      # Resolve an item from the container
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] key
      #   The key for the item you wish to resolve
      # @yield
      #   Fallback block to call when a key is missing. Its result will be returned
      # @yieldparam [Mixed] key Missing key
      #
      # @raise [KeyError]
      #   If the given key is not registered with the container (and no block provided)
      #
      #
      # @return [Mixed]
      #
      # @api public
      def call(container, key)
        item = container.fetch(key.to_s) do
          if block_given?
            return yield(key)
          else
            raise KeyError.new(%(key not found: "#{key}"), key: key.to_s, receiver: container)
          end
        end

        item.call
      end

      # Resolve the collection of members of a multi-binding
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] key
      #   The key the multi-binding is registered with
      # @param [Array, nil] tags
      #   When given, only members carrying all of the tags are resolved
      #
      # @raise [KeyError]
      #   If the given key is not registered with the container
      # @raise [Dry::Container::Error]
      #   If the given key is not registered as a multi-binding
      #
      # @return [Array]
      #
      # @api public
      def resolve_many(container, key, tags: nil)
        fetch_multi(container, key).resolve(tags: tags)
      end

      # Resolve a single member of a multi-binding by its name
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] key
      #   The key the multi-binding is registered with
      # @param [Mixed] name
      #   The member name
      #
      # @raise [KeyError]
      #   If the given key is not registered with the container
      # @raise [Dry::Container::Error]
      #   If the given key is not registered as a multi-binding, or has no
      #   member with the given name
      #
      # @return [Mixed]
      #
      # @api public
      def resolve_member(container, key, name)
        fetch_multi(container, key).resolve_member(name)
      end

      # Check whether an items is registered under the given key
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] key
      #   The key you wish to check for registration with
      #
      # @return [Bool]
      #
      # @api public
      def key?(container, key)
        container.key?(key.to_s)
      end

      # An array of registered names for the container
      #
      # @return [Array]
      #
      # @api public
      def keys(container)
        container.keys
      end

      # Calls block once for each key in container, passing the key as a parameter.
      #
      # If no block is given, an enumerator is returned instead.
      #
      # @return Hash
      #
      # @api public
      def each_key(container, &block)
        container.each_key(&block)
      end

      # Calls block once for each key in container, passing the key and
      # the registered item parameters.
      #
      # If no block is given, an enumerator is returned instead.
      #
      # @return Key, Value
      #
      # @api public
      # @note In discussions with other developers, it was felt that being able
      #       to iterate over not just the registered keys, but to see what was
      #       registered would be very helpful. This is a step toward doing that.
      def each(container, &block)
        container.map { |key, value| [key, value.call] }.each(&block)
      end

      private

      # @api private
      def fetch_multi(container, key)
        item = container.fetch(key.to_s) do
          raise KeyError.new(%(key not found: "#{key}"), key: key.to_s, receiver: container)
        end

        unless item.is_a?(::Dry::Container::Item::Multi)
          raise Error,
                "Cannot resolve #{key.inspect} as a multi-binding: " \
                "a single item is registered with this key"
        end

        item
      end
    end
  end
end
