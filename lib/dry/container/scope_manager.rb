# frozen_string_literal: true

require "concurrent/hash"
require "dry/container/scope"
require "dry/container/resolution_context"

module Dry
  class Container
    # Owns the scope stack and construction-chain state for a single container.
    #
    # All state is stored fiber-locally: nested scopes share the fiber that
    # opened them, while interleaved fibers (e.g. concurrent requests) each
    # get isolated scope stacks.
    #
    # @api private
    class ScopeManager
      # @api private
      def initialize
        @context = ResolutionContext.new(self)
        @fiber_mutex = ::Mutex.new
        @fiber_states = ::Concurrent::Hash.new
      end

      # @return [Dry::Container::ResolutionContext]
      attr_reader :context

      # Open a detached scope. The caller is responsible for activating and
      # closing it.
      #
      # @return [Dry::Container::Scope]
      #
      # @api public
      def open_scope(name = nil)
        Scope.new(self, name)
      end

      # Open, activate and automatically close a scope around the block
      #
      # @return [Mixed] the block result
      #
      # @api public
      def with_scope(name = nil)
        scope = open_scope(name)
        scope.activate!

        begin
          yield scope
        ensure
          scope.close! unless scope.closed?
        end
      end

      # @return [Dry::Container::Scope, nil]
      def current_scope
        _scopes.last
      end

      # Fiber-local stack of active scopes, outermost first
      #
      # @return [Array<Dry::Container::Scope>]
      def _scopes
        fiber_state[:scopes]
      end

      # Fiber-local construction chain for the resolution in progress
      #
      # @return [Array<Hash>]
      def _frames
        fiber_state[:frames]
      end

      # @api private
      def _push_scope(scope)
        _scopes << scope
      end

      # @api private
      def _remove_scope(scope)
        index = _scopes.rindex(scope)
        _scopes.delete_at(index) if index
      end

      private

      # @private
      def fiber_state
        fiber = Fiber.current

        @fiber_states[fiber] ||= begin
          prune_dead_fibers
          {scopes: [], frames: []}
        end
      end

      # @private
      def prune_dead_fibers
        @fiber_mutex.synchronize do
          @fiber_states.delete_if { |active_fiber, state|
            active_fiber != Fiber.current && !active_fiber.alive? && state[:scopes].empty?
          }
        end
      end
    end
  end
end
