# frozen_string_literal: true

module Dry
  class Container
    # Per-thread state for active resolutions and per-container ambient scope
    # stacks.
    #
    # Ambient scope stacks are stored in a {WeakMap} keyed by the raw container
    # hash so that they are garbage collected together with their container and
    # survive container +dup+/+clone+ without colliding.
    #
    # @api private
    module ScopeState
      # A single item being constructed
      #
      # @api private
      ResolutionFrame = Struct.new(:container, :key, :lifetime, :scope, keyword_init: true)

      # @api private
      FRAME_STACK_KEY = :"dry_container_resolution_frames"

      # @return [WeakMap]
      #
      # @api private
      def self.storage
        Thread.current[:"dry_container_scope_state"] ||= ::ObjectSpace::WeakMap.new
      end

      # @api private
      def self.state_for(container)
        storage[container] ||= {scopes: []}
      end

      # @api private
      def self.scope_stack(container)
        state_for(container)[:scopes]
      end

      # Items currently being constructed, innermost last. The stack is shared
      # across containers so resolutions nested through other containers stay
      # balanced.
      #
      # @api private
      def self.frames
        Thread.current[FRAME_STACK_KEY] ||= []
      end

      # The item currently being constructed, if any.
      #
      # @return [ResolutionFrame, nil]
      #
      # @api private
      def self.current_frame
        frames.last
      end

      # Construct an item within the given frame.
      #
      # @api private
      def self.construct(frame)
        frames.push(frame)
        yield
      ensure
        frames.pop
      end
    end
  end
end
