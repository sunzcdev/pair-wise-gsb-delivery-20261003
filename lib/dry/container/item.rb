# frozen_string_literal: true

module Dry
  class Container
    # Base class to abstract Memoizable and Callable implementations
    #
    # @api abstract
    #
    class Item
      # @return [Mixed] the item to be solved later
      attr_reader :item

      # @return [Hash] the options to memoize, call or no.
      attr_reader :options

      # @return [Dry::Container::CacheState, nil] the cache state of the
      #   container the item is registered with, used to detect replaced
      #   dependencies
      #
      # @api private
      attr_accessor :cache_state

      # @api abstract
      def initialize(item, options = {})
        @item = item
        @options = {
          call: item.is_a?(::Proc) && item.parameters.empty?
        }.merge(options)
      end

      # @api abstract
      def call
        raise NotImplementedError
      end

      # @private
      def value?
        !callable?
      end

      # @private
      def callable?
        options[:call]
      end

      # Build a new item with transformation applied
      #
      # @private
      def map(func)
        mapped =
          if callable?
            self.class.new(-> { func.(item.call) }, options)
          else
            self.class.new(func.(item), options)
          end
        mapped.cache_state = cache_state
        mapped
      end
    end
  end
end
