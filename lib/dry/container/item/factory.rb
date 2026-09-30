# frozen_string_literal: true

require "dry/container/item/memoizable"
require "dry/container/item/callable"
require "dry/container/item/scoped"

module Dry
  class Container
    class Item
      # Factory for create an Item to register inside of container
      #
      # @api public
      class Factory
        # @api private
        LIFETIMES = %i[application scope resolution].freeze

        # Creates an Item Memoizable or Callable
        # @param [Mixed] item
        # @param [Hash] options
        #
        # @raise [Dry::Container::Error]
        #
        # @return [Dry::Container::Item::Base]
        def call(item, options = {})
          options = normalize_options(options)

          case options[:lifetime]
          when :application
            Memoizable.new(item, options)
          when :scope
            Scoped.new(item, options)
          else
            Callable.new(item, options)
          end
        end

        private

        # @api private
        def normalize_options(options)
          options = options.dup

          lifetime = options[:lifetime] || (options[:memoize] ? :application : :resolution)

          unless LIFETIMES.include?(lifetime)
            raise ::ArgumentError,
                  "invalid lifetime #{lifetime.inspect} " \
                  "(expected one of #{LIFETIMES.map(&:inspect).join(", ")})"
          end

          options[:lifetime] = lifetime
          options
        end
      end
    end
  end
end
