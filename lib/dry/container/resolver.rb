# frozen_string_literal: true

module Dry
  class Container
    # Default resolver for resolving items from container
    #
    # @api public
    class Resolver
      # Resolve an item from the container
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] key
      #   The key for the item you wish to resolve
      # @yield
      #   Fallback block to call when a key is missing. Its result will be returned
      # @yieldparam [Mixed] key Missing key
      #
      # @raise [KeyError]
      #   If the given key is not registered with the container (and no block provided)
      #
      #
      # @return [Mixed]
      #
      # @api public
      def call(container, key)
        item, _terminal_key, decorator_layers = follow(container, key) do |missing|
          if block_given?
            return yield(missing)
          else
            raise KeyError.new(
              %(key not found: "#{missing}"), key: missing, receiver: container
            )
          end
        end

        value = item.call

        # Layers are collected from the requested name towards the terminal,
        # so apply them in reverse: the alias closest to the consumer wraps
        # decorations introduced further down the chain.
        decorator_layers.reverse.each do |decorators|
          decorators.each { |decorator| value = decorator.(value) }
        end

        value
      end

      # Follow an alias chain and return the terminal registered item
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] key
      #   The key to start the lookup from
      #
      # @yield [String] called when the requested key itself is missing;
      #   the block must either return a terminal or raise
      #
      # @raise [KeyError]
      #   When an alias chain has no terminal registration
      # @raise [Dry::Container::Error]
      #   When an alias chain contains a cycle
      #
      # @return [Array(Dry::Container::Item, String, Array<Array<#call>>)]
      #   The terminal item, its key and the alias-scoped decorator layers
      #
      # @api private
      def follow(container, key)
        current = key.to_s
        chain = []
        decorator_layers = []

        loop do
          entry = container[current]

          if entry.nil?
            if chain.empty?
              yield(current)
            end

            raise KeyError.new(
              %(alias chain "#{chain.join(" -> ")} -> #{current}" has no terminal registration),
              key: current,
              receiver: container
            )
          end

          if entry.is_a?(::Dry::Container::Alias)
            if chain.include?(current)
              cycle = (chain + [current])
              loop_start = cycle.index(current)

              raise ::Dry::Container::Error,
                    "circular alias chain detected: #{cycle.join(" -> ")} " \
                    "(loop: #{cycle[loop_start..].join(" -> ")})"
            end

            chain << current
            decorator_layers << entry.decorators
            current = entry.target
          else
            return [entry, current, decorator_layers]
          end
        end
      end

      # Check whether an items is registered under the given key
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] key
      #   The key you wish to check for registration with
      #
      # @return [Bool]
      #
      # @api public
      def key?(container, key)
        container.key?(key.to_s)
      end

      # An array of registered names for the container
      #
      # @return [Array]
      #
      # @api public
      def keys(container)
        container.keys
      end

      # Calls block once for each key in container, passing the key as a parameter.
      #
      # If no block is given, an enumerator is returned instead.
      #
      # @return Hash
      #
      # @api public
      def each_key(container, &block)
        container.each_key(&block)
      end

      # Calls block once for each key in container, passing the key and
      # the registered item parameters.
      #
      # If no block is given, an enumerator is returned instead.
      #
      # @return Key, Value
      #
      # @api public
      # @note In discussions with other developers, it was felt that being able
      #       to iterate over not just the registered keys, but to see what was
      #       registered would be very helpful. This is a step toward doing that.
      def each(container, &block)
        container.map { |key, _value| [key, call(container, key)] }.each(&block)
      end
    end
  end
end
