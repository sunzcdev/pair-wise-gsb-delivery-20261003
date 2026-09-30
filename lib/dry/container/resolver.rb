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
        item = resolve_alias_chain(container, key.to_s) do |missing_key|
          return yield(missing_key) if block_given?

          raise KeyError.new(
            %(key not found: "#{missing_key}"),
            key: missing_key.to_s,
            receiver: container
          )
        end

        item.call
      end

      # Follow any alias chain starting at +key+ and return the terminal
      # registered item. When the requested key itself is unregistered,
      # yields with the missing key instead.
      #
      # @raise [Dry::Container::Error] when the alias chain is circular or
      #   ends at an unregistered key
      #
      # @return [Dry::Container::Item]
      #
      # @private
      def resolve_alias_chain(container, key, &block)
        _, item = resolve_alias_chain_with_key(container, key, &block)
        item
      end

      # Same as {#resolve_alias_chain} but returns the terminal key together
      # with its registered item.
      #
      # @return [Array(String, Dry::Container::Item)]
      #
      # @private
      def resolve_alias_chain_with_key(container, key)
        chain = []
        current = key

        loop do
          item = container.fetch(current) do
            if chain.empty?
              return [current, yield(current)]
            else
              chain << current
              raise Error, "alias target not found: #{chain.join(" -> ")}"
            end
          end

          return [current, item] unless item.is_a?(Alias)

          chain << current
          target = item.target

          if chain.include?(target)
            chain << target
            raise Error, "circular alias chain detected: #{chain.join(" -> ")}"
          end

          current = target
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
        container.map { |key, value|
          value =
            if value.is_a?(Alias)
              resolve_alias_chain(container, key) do |missing|
                raise KeyError.new(
                  %(key not found: "#{missing}"), key: missing.to_s, receiver: container
                )
              end
            else
              value
            end

          [key, value.call]
        }.each(&block)
      end
    end
  end
end
