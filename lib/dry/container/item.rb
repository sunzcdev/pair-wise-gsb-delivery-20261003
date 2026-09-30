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

      # @return [Symbol] the lifetime of the item (+:application+, +:scope+
      #   or +:resolution+)
      #
      # @api private
      def lifetime
        options.fetch(:lifetime, :resolution)
      end

      # Whether the resolved value is cached for the whole application
      #
      # @api private
      def application?
        lifetime == :application
      end

      # Whether the resolved value belongs to a scope
      #
      # @api private
      def scope?
        lifetime == :scope
      end

      # Build a new item with transformation applied
      #
      # @private
      def map(func)
        if callable?
          self.class.new(-> { func.(item.call) }, options)
        else
          self.class.new(func.(item), options)
        end
      end
    end
  end
end
