# frozen_string_literal: true

require "dry/container/item/factory"
require "dry/container/item/multi"

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

      # Register a multi-binding with the container to be resolved later
      #
      # Multi-bindings are already built items (they resolve their own
      # members), so they are stored as-is instead of being wrapped by the
      # item factory
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] key
      #   The key to register the multi-binding with (used to resolve)
      # @param [Dry::Container::Item::Multi] multi
      #   The multi-binding to register with the container
      #
      # @raise [Dry::Container::Error]
      #   If an item is already registered with the given key
      #
      # @return [Mixed]
      #
      # @api public
      def register_multi(container, key, multi)
        key = key.to_s.dup.freeze
        @_mutex.synchronize do
          if container.key?(key)
            raise Error, "There is already an item registered with the key #{key.inspect}"
          end

          container[key] = multi
        end
      end

      def factory
        @factory ||= ::Dry::Container::Item::Factory.new
      end
    end
  end
end
