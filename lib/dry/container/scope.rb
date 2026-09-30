# frozen_string_literal: true

require "monitor"

module Dry
  class Container
    # An explicit, closeable resolution scope.
    #
    # Cached values for items registered with the +:scope+ lifetime live on the
    # scope itself and are discarded when the scope is closed. A closed scope
    # can never be activated or resolved through again.
    #
    # @example
    #
    #   scope = container.enter_scope
    #   container.resolve(:current_user)
    #   scope.close!
    #
    # @api public
    class Scope
      # @return [Mixed] optional label of the scope
      attr_reader :name

      # @api private
      def initialize(container, name = nil)
        @container = container
        @container_hash = container._container
        @name = name
        @monitor = ::Monitor.new
        @cache = {}
        @closed = false
      end

      # @return [Boolean] whether this scope belongs to the given raw container
      #
      # @api private
      def active_for_container?(container_hash)
        @container_hash.equal?(container_hash)
      end

      # Resolve an item through this scope.
      #
      # @param [Mixed] key
      #   The key for the item you wish to resolve
      #
      # @raise [Dry::Container::ScopeClosedError]
      #   If the scope has already been closed
      #
      # @return [Mixed]
      #
      # @api public
      def resolve(key, &block)
        @monitor.synchronize do
          raise_scope_closed! if @closed
        end

        @container.config.resolver.call_in_scope(@container._container, self, key, &block)
      end

      # Close the scope, discarding its cache. Closing is idempotent.
      #
      # @return [Dry::Container::Scope] self
      #
      # @api public
      def close!
        @monitor.synchronize do
          unless @closed
            @closed = true
            @cache = nil
            ScopeState.scope_stack(@container._container).delete(self)
          end
        end

        self
      end

      # @return [Boolean] whether the scope has been closed
      #
      # @api public
      def closed?
        @monitor.synchronize { @closed }
      end

      # Cache a scope-level item value. The monitor is reentrant so a factory
      # resolving another scope-level key on the same scope cannot deadlock.
      #
      # @api private
      def resolve_cached(key)
        @monitor.synchronize do
          raise_scope_closed! if @closed

          @cache.fetch(key) { @cache[key] = yield }
        end
      end

      private

      # @api private
      def raise_scope_closed!
        label = name ? " #{name.inspect}" : ""
        raise ScopeClosedError, "scope#{label} has already been closed and cannot be reactivated"
      end
    end
  end
end
