# frozen_string_literal: true

require "dry/container/item/memoizable"
require "dry/container/item/callable"
require "dry/container/item/dependent"

module Dry
  class Container
    class Item
      # Factory for create an Item to register inside of container
      #
      # @api public
      class Factory
        # Creates a Memoizable, Dependent or Callable item
        #
        # @param [Mixed] item
        # @param [Hash] options
        #
        # @raise [Dry::Container::Error]
        #
        # @return [Dry::Container::Item::Base]
        def call(item, options = {})
          if has_dependencies?(options)
            factory = Dependent.new(item, options)

            if options[:memoize] == false
              factory
            else
              Memoizable.new(factory, factory.options)
            end
          elsif options[:memoize]
            Memoizable.new(item, options)
          else
            Callable.new(item, options)
          end
        end

        private

        # @private
        def has_dependencies?(options)
          Array(options[:deps]).any? || Array(options[:lazy_deps]).any?
        end
      end
    end
  end
end
