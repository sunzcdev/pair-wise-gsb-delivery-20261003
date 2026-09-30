# frozen_string_literal: true

require "dry/container/item"

module Dry
  class Container
    class Item
      # Callable backed by a factory block that receives its explicitly
      # declared dependencies as keyword arguments
      #
      # @api public
      class Dependent < Item
        # @param [Proc] item factory block
        # @param [Hash] options
        #
        # @raise [Dry::Container::Error] if the declaration is not supported
        #
        # @api public
        def initialize(item, options = {})
          options = options.dup

          if options[:call] == false && has_dependencies?(options)
            raise ::Dry::Container::Error,
                  "Dependencies are not supported together with `call: false`"
          end

          options[:call] = true if has_dependencies?(options)
          super
          validate!
        end

        # Resolves the declared dependencies and invokes the factory
        #
        # @param [Dry::Container::Resolver::Context, nil] context
        #
        # @return [Mixed]
        #
        # @api private
        def call(context = nil)
          kwargs = {}

          dependencies.each do |key|
            kwargs[key.to_sym] = context.fetch(key)
          end

          lazy_dependencies.each do |key|
            kwargs[key.to_sym] = context.lazy(key)
          end

          item.call(**kwargs)
        end

        private

        # @private
        def has_dependencies?(options)
          Array(options[:deps]).any? || Array(options[:lazy_deps]).any?
        end

        # @private
        def validate!
          unless item.is_a?(::Proc)
            raise ::Dry::Container::Error,
                  "Dependencies can only be declared for a block or a proc"
          end

          overlap = dependencies & lazy_dependencies
          if overlap.any?
            raise ::Dry::Container::Error,
                  "Keys declared both as eager and lazy dependencies: #{overlap.inspect}"
          end
        end
      end
    end
  end
end
