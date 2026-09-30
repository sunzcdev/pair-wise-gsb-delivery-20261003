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
          @memoized = false
        end

        # Returns the result of item call using a syncronized mutex
        #
        # The memoized value is rebuilt when one of the dependencies declared
        # via the +:depends_on+ option has been replaced since the value was
        # memoized
        #
        # @return [Dry::Container::Item::Base]
        def call
          memoize_mutex.synchronize do
            if @memoized && dependencies_fresh?
              @memoized_item
            else
              build
            end
          end
        end

        private

        # Builds the value and publishes it as the memoized one
        #
        # The value is only published when no dependency was replaced while
        # it was being built: a value built from pre-replacement dependencies
        # must never become the cached post-replacement version
        #
        # @private
        def build
          versions = current_dependency_versions
          value = item.call

          if versions == current_dependency_versions
            @memoized_item = value
            @memoized_versions = versions
            @memoized = true
          end

          value
        end

        # @private
        def dependencies_fresh?
          @memoized_versions == current_dependency_versions
        end

        # @private
        def current_dependency_versions
          cache_state ? cache_state.versions_for(dependency_keys) : nil
        end

        # @private
        def dependency_keys
          Array(options[:depends_on])
        end

        # @private
        def raise_not_supported_error
          raise ::Dry::Container::Error, "Memoize only supported for a block or a proc"
        end
      end
    end
  end
end
