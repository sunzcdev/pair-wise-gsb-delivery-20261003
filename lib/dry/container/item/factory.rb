# frozen_string_literal: true

require "dry/container/item/memoizable"
require "dry/container/item/callable"

module Dry
  class Container
    class Item
      # Factory for create an Item to register inside of container
      #
      # @api public
      class Factory
        # Creates an Item Memoizable or Callable
        # @param [Mixed] item
        # @param [Hash] options
        #
        # @raise [Dry::Container::Error]
        #
        # @return [Dry::Container::Item::Base]
        def call(item, options = {})
          lifecycle = options[:lifecycle]

          if !options[:memoize].nil? && !lifecycle.nil?
            lifecycle_memoize = lifecycle.to_sym == :application

            if options[:memoize] != lifecycle_memoize
              raise ScopeError,
                    "conflicting options: memoize: #{options[:memoize].inspect} " \
                    "and lifecycle: #{lifecycle.to_sym.inspect}"
            end
          end

          effective_lifecycle = lifecycle || (options[:memoize] ? :application : nil)

          if effective_lifecycle == :application
            Memoizable.new(item, options)
          else
            Callable.new(item, options.merge(lifecycle: effective_lifecycle || :resolution))
          end
        end
      end
    end
  end
end
