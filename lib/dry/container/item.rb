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

      # @api abstract
      def initialize(item, options = {})
        @item = item
        @options = {
          call: item.is_a?(::Proc) && item.parameters.empty?
        }.merge(options)
      end

      # @api abstract
      def call(_context = nil)
        raise NotImplementedError
      end

      # Keys of dependencies that must be resolved before this item
      #
      # @return [Array<String>]
      #
      # @api private
      def dependencies
        Array(options[:deps]).map(&:to_s)
      end

      # Keys of dependencies handed over as lazy references
      #
      # @return [Array<String>]
      #
      # @api private
      def lazy_dependencies
        Array(options[:lazy_deps]).map(&:to_s)
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
        if callable?
          self.class.new(->(context = nil) { func.(item.call(context)) }, options)
        else
          self.class.new(func.(item), options)
        end
      end
    end
  end
end
