# frozen_string_literal: true

require "dry/container/item/factory"
require "dry/container/member"
require "dry/container/collection"

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

      # Bind a named member of a multibinding collection
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] tag
      #   The collection key the member belongs to (also used to resolve the set)
      # @param [Mixed] name
      #   The member's stable, unique name within the collection
      # @param [Mixed] item
      #   The item to register with the container
      # @param [Hash] options
      # @option options [Symbol, Array] :tags
      #   Additional tags to resolve the member by
      # @option options [Integer] :order
      #   Explicit weight used to order members within the collection
      # @option options [String] :source
      #   Origin of the binding, reported on name conflicts
      #
      # @raise [Dry::Container::Error]
      #   If the member name is already taken or the collection key collides
      #   with a regular item
      #
      # @return [Dry::Container::Member]
      #
      # @api public
      def bind(container, tag, name, item, options)
        separator = options[:namespace_separator] || "."
        tag = tag.to_s.dup.freeze
        name = name.to_s.dup.freeze
        member_key = [tag, name].join(separator).freeze

        @_mutex.synchronize do
          if container[member_key] && !container[member_key].is_a?(::Dry::Container::Member)
            raise Error,
                  "There is already an item registered with the key #{member_key.inspect}"
          end

          existing = container[tag]

          if existing && !existing.is_a?(::Dry::Container::Collection)
            raise Error,
                  "Cannot bind member #{name.inspect} to #{tag.inspect}: " \
                  "the key is already used by a regular item"
          end

          member = ::Dry::Container::Member.new(
            name, item, options.merge(collection_tag: tag)
          )
          collection = existing || ::Dry::Container::Collection.new(tag)
          collection.add(member)

          container[tag] = collection
          container[member_key] = member

          member
        end
      end

      def factory
        @factory ||= ::Dry::Container::Item::Factory.new
      end
    end
  end
end
