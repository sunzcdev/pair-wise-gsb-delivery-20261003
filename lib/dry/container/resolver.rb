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
      def call(container, key, &block)
        resolve(container, key, nil, &block)
      end

      # Resolve an item through an explicit scope
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Dry::Container::Scope] scope
      #   The scope to resolve through
      # @param [Mixed] key
      #   The key for the item you wish to resolve
      #
      # @return [Mixed]
      #
      # @api public
      def call_in_scope(container, scope, key, &block)
        resolve(container, key, scope, &block)
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

      private

      # @api private
      def resolve(container, key, explicit_scope)
        string_key = key.to_s

        item = container.fetch(string_key) do
          if block_given?
            return yield(key)
          else
            raise KeyError.new(%(key not found: "#{key}"), key: string_key, receiver: container)
          end
        end

        construct_item(container, item, string_key, explicit_scope)
      end

      # @api private
      def construct_item(container, item, key, explicit_scope)
        scope_stack = ScopeState.scope_stack(container)
        ambient_scope = scope_stack.last
        scope = explicit_scope || ambient_scope

        if scope && !scope.active_for_container?(container)
          raise ::Dry::Container::Error,
                "scope belongs to a different container and cannot resolve #{key.inspect}"
        end

        if item.scope? && scope.nil?
          raise ::Dry::Container::NoActiveScopeError,
                %(cannot resolve scope-level item "#{key}" outside of an active scope)
        end

        frames = ScopeState.frames
        frame = ScopeState::ResolutionFrame.new(
          container: container,
          key: key,
          lifetime: item.lifetime,
          scope: scope
        )

        if item.scope? && frames.any? { |active_frame| active_frame.lifetime == :application }
          raise_captive_dependency(frames, frame)
        end

        ScopeState.construct(frame) { item.call }
      end

      # @api private
      def raise_captive_dependency(frames, frame)
        chain = frames.map(&:key).append(frame.key)

        raise ::Dry::Container::CaptiveDependencyError,
              "application-level item \"#{frames.first.key}\" captures scope-level " \
              "dependency \"#{frame.key}\"; scope-level objects only live for one " \
              "scope and must not be held as long-lived dependencies " \
              "(construction chain: #{chain.join(" -> ")})"
      end
    end
  end
end
