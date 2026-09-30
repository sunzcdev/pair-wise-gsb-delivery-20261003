# frozen_string_literal: true

require "dry/container/collection"

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

      # Resolve a multibinding collection into an ordered array of members
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] tag
      #   The collection key to resolve
      # @param [Array<Mixed>] tags
      #   Extra tags to filter members by (resolved together with +tag+)
      # @yield [Dry::Container::Member]
      #   Optional filter evaluated against member metadata before calling, so
      #   factories of members that do not match never run
      #
      # @raise [KeyError]
      #   If the given tag is not a registered collection
      #
      # @return [Array<Mixed>]
      #
      # @api public
      def resolve_all(container, tag, tags = [], &filter)
        key = tag.to_s

        collection = container.fetch(key) do
          raise KeyError.new(
            %(collection not found: "#{key}"), key: key, receiver: container
          )
        end

        unless collection.is_a?(::Dry::Container::Collection)
          raise Error, "item registered with #{key.inspect} is not a collection"
        end

        collection.call(tags.map(&:to_s), &filter)
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
    end
  end
end
