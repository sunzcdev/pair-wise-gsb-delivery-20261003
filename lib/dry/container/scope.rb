# frozen_string_literal: true

require "concurrent/hash"

module Dry
  class Container
    # An explicit, nestable resolution scope.
    #
    # Objects registered with `lifecycle: :scope` are memoized per scope
    # instance: every scope gets its own instances, and closing a scope
    # releases all of its cached objects.
    #
    # @api public
    class Scope
      # @return [String, Symbol, nil] optional name of the scope
      attr_reader :name

      # @api private
      def initialize(manager, name = nil)
        @manager = manager
        @name = name
        @cache = ::Concurrent::Hash.new
        @locks = ::Concurrent::Hash.new
        @state_mutex = ::Mutex.new
        @active_fibers = ::Hash.new(0)
        @closed = false
      end

      # Whether this scope has already been closed
      #
      # @return [Boolean]
      #
      # @api public
      def closed?
        @state_mutex.synchronize { @closed }
      end

      # Activate the scope.
      #
      # With a block, activates the scope for the duration of the block and
      # closes it afterwards. Without a block, the scope stays active until
      # {#close!} is called explicitly.
      #
      # @raise [Dry::Container::ScopeClosedError] if the scope is already closed
      #
      # @return [Dry::Container::Scope]
      #
      # @api public
      def activate
        activate!

        return self unless block_given?

        begin
          yield self
        ensure
          close!
        end
      end

      # Close the scope and release everything cached inside of it.
      #
      # @raise [Dry::Container::ScopeClosedError] if the scope is already closed
      # @raise [Dry::Container::ScopeError] if the scope is still active
      #   elsewhere (e.g. another fiber)
      #
      # @return [Dry::Container::Scope]
      #
      # @api public
      def close!
        fiber = Fiber.current

        @state_mutex.synchronize do
          ensure_open!

          current_count = @active_fibers[fiber].to_i

          if current_count > 1
            raise ScopeError,
                  "cannot close #{inspect_name}: it is still active in the current context"
          end

          active_elsewhere = @active_fibers.keys.any? { |active_fiber|
            active_fiber != fiber && active_fiber.alive?
          }

          if active_elsewhere
            raise ScopeError,
                  "cannot close #{inspect_name}: it is still active in another context"
          end

          @active_fibers.clear
          @closed = true
          @cache.clear
          @locks.clear
        end

        @manager._remove_scope(self)

        self
      end

      # Memoize the block result for the given key within this scope only
      #
      # @api private
      def memoize(key)
        lock = @state_mutex.synchronize do
          ensure_open!
          @locks[key] ||= ::Mutex.new
        end

        lock.synchronize do
          if @cache.key?(key)
            @cache[key]
          else
            @cache[key] = yield
          end
        end
      end

      # @api private
      def activate!
        @state_mutex.synchronize do
          ensure_open!

          active_elsewhere = @active_fibers.keys.any? { |active_fiber|
            active_fiber != Fiber.current && active_fiber.alive?
          }

          if active_elsewhere
            raise ScopeError,
                  "#{inspect_name} is already active in another context"
          end

          @active_fibers[Fiber.current] += 1
        end

        @manager._push_scope(self)

        self
      end

      # @api private
      def inspect_name
        name.nil? ? "scope" : "scope #{name.inspect}"
      end

      private

      # @private
      def ensure_open!
        return unless @closed

        raise ScopeClosedError,
              "#{inspect_name} is closed and cannot be reactivated"
      end
    end
  end
end
