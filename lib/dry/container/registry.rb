# frozen_string_literal: true

require "dry/container/item/factory"

module Dry
  class Container
    # Default registry for registering items with the container
    #
    # @api public
    class Registry
      # @private
      def initialize
        @_mutex = ::Mutex.new
      end

      # Register an item with the container to be resolved later
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] key
      #   The key to register the container item with (used to resolve)
      # @param [Mixed] item
      #   The item to register with the container
      # @param [Hash] options
      # @option options [Symbol] :call
      #   Whether the item should be called when resolved
      #
      # @raise [Dry::Container::Error]
      #   If an item is already registered with the given key
      #
      # @return [Mixed]
      #
      # @api public
      def call(container, key, item, options)
        key = key.to_s.dup.freeze
        @_mutex.synchronize do
          if container.key?(key)
            raise Error, "There is already an item registered with the key #{key.inspect}"
          end

          container[key] = factory.call(item, options)
        end
      end

      # Replace an already registered item with a new one
      #
      # The new item is built before the container is touched, so a failure
      # leaves the original registrations untouched
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] key
      #   The key of the registered item to replace
      # @param [Mixed] item
      #   The item to register in its place
      # @param [Hash] options
      #   Options to pass to the factory when building the new item
      #
      # @raise [Dry::Container::Error]
      #   If no item is registered with the given key
      #
      # @return [Mixed]
      #
      # @api public
      def replace(container, key, item, options)
        key = key.to_s.dup.freeze
        new_item = factory.call(item, options)

        @_mutex.synchronize do
          unless container.key?(key)
            raise Error, "There is no item registered with the key #{key.inspect}"
          end

          container[key] = new_item
        end
      end

      def factory
        @factory ||= ::Dry::Container::Item::Factory.new
      end    end
  end
end
