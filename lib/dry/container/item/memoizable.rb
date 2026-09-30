# frozen_string_literal: true

require "dry/container/item"

module Dry
  class Container
    class Item
      # Memoizable class to store and execute item calls
      #
      # @api public
      #
      class Memoizable < Item
        # Sentinel marking a memo slot that has never been published.
        # Distinct from nil, which is a legitimate memoized value.
        #
        # @api private
        UNRESOLVED = ::Object.new.tap(&:freeze).freeze

        # @return [Mutex] the stored mutex
        attr_reader :memoize_mutex

        # Returns a new Memoizable instance
        #
        # @param [Mixed] item
        # @param [Hash] options
        #
        # @raise [Dry::Container::Error]
        #
        # @return [Dry::Container::Item::Base]
        def initialize(item, options = {})
          super
          raise_not_supported_error unless callable?

          @memoize_mutex = ::Mutex.new
          @memoized_item = UNRESOLVED
          # Bumped whenever the cached value must not be published. The
          # mutex itself is not taken for invalidation: an in-flight
          # builder is allowed to finish, but its result is discarded so
          # an object constructed against dependencies that were replaced
          # never becomes the new version of the cache.
          @generation = 0
        end

        # Returns the result of item call using a syncronized mutex
        #
        # @return [Dry::Container::Item::Base]
        def call
          # Fast path: a published value is reused without contention
          return @memoized_item unless @memoized_item.equal?(UNRESOLVED)

          generation = @generation

          result = item.call

          @memoize_mutex.synchronize do
            if @generation == generation && @memoized_item.equal?(UNRESOLVED)
              @memoized_item = result
            end
          end

          result
        end

        # Drop the cached value, if any. Factories already running are
        # allowed to finish, but their results cannot be published and the
        # next resolution rebuilds the item against current dependencies.
        #
        # @return [void]
        #
        # @api private
        def invalidate!
          @generation += 1
          @memoized_item = UNRESOLVED
        end

        private

        # @private
        def raise_not_supported_error
          raise ::Dry::Container::Error, "Memoize only supported for a block or a proc"
        end
      end
    end
  end
end