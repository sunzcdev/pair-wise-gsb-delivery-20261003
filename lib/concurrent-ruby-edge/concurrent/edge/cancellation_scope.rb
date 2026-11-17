require 'concurrent/atomic/atomic_boolean'
require 'concurrent/collection/lock_free_stack'
require 'concurrent/errors'
require 'concurrent/promises'
require 'concurrent/synchronization/object'

module Concurrent
  module Promises

    # An explicit configuration of cancellation relationships between futures
    # created inside its block.
    #
    # The same future can be reused by several branches. When one of the
    # derived results is cancelled it must not silently cancel an upstream
    # future which another branch still needs. Therefore every dependency has
    # an explicit relationship to a scope:
    #
    # * an *owned* relationship means the future's not-yet-started execution is
    #   cancellable through the scope;
    # * a *borrowed* relationship means the scope only observes the future's
    #   result, it can never cancel it.
    #
    # Futures created inside a {CancellationScope} block (directly or through
    # chaining, zipping and flattening) become members of the scope. A member
    # is owned by default; it becomes a borrower when created with
    # `owned: false` (typically an externally shared future).
    #
    # Cancellation is an independent terminal state of a future
    # ({Concurrent::Promises::Future#cancelled?}), distinct from rejection with
    # a business exception. Already running user blocks are never forcibly
    # killed; they run to completion and only not-yet-started tasks are
    # cancelled. On {#cancel} the dependency graph is walked leaves-first:
    # an owned node is cancelled only when none of its still-pending dependents
    # inside the scope needs it, therefore a shared upstream survives as long
    # as another branch needs it.
    #
    # @!macro warn.edge
    #
    # @example borrowed dependency is kept alive by another branch
    #   scope, (branch_a, branch_b) = Concurrent::Promises.cancellation_scope do
    #     head     = future { expensive_value }
    #     branch_a = head.then { |v| use_a v }
    #     branch_b = head.then { |v| use_b v }
    #     [branch_a, branch_b]
    #   end
    #   branch_b.cancel # only this derived result is cancelled;
    #                   # `head` stays alive because branch_a still needs it
    class CancellationScope < Synchronization::Object
      safe_initialization!

      # The thread-local currently active scope. A future created while a scope
      # is active is registered in it as a member.
      # @!visibility private
      def self.current
        scope = Thread.current[:concurrent_promises_cancellation_scope]
        scope.is_a?(self) ? scope : nil
      end

      # Runs the block with the scope installed as the current scope so that
      # every future created inside is registered.
      # @!visibility private
      def self.run(scope, block)
        previous = Thread.current[:concurrent_promises_cancellation_scope]
        Thread.current[:concurrent_promises_cancellation_scope] = scope
        begin
          block.call scope
        ensure
          Thread.current[:concurrent_promises_cancellation_scope] = previous
        end
      end

      def initialize
        super()
        # one entry per registered node: [node, owned?]
        @Nodes     = LockFreeStack.new
        @Cancelled = AtomicBoolean.new false
        @Reason    = nil
      end

      # @return [true, false]
      def cancelled?
        @Cancelled.true?
      end

      # The reason used when the scope was cancelled.
      # @return [Exception, nil]
      def reason
        @Reason
      end

      # Called by {AbstractEventFuture#add_cancellation_edge} when a node
      # establishes its relationship with this scope.
      # @!visibility private
      def register_node(node, owned)
        entry = @Nodes.each.find { |n, _| n.equal? node }
        if entry
          # keep the strongest relationship: an owned edge wins over borrowed
          entry[1] = true if owned
        else
          @Nodes.push [node, owned]
        end
        # a node joining an already cancelled scope follows the same graph rule
        cancel_graph(@Reason) if cancelled?
      end

      # Cancels the scope. Every owned member whose still-pending in-scope
      # dependents are themselves cancelled reaches the independent cancelled
      # terminal state. Borrowed members and already resolved nodes are left
      # untouched. The method is idempotent.
      #
      # @param reason [Exception] optional reason recorded in the cancelled
      #   states, defaults to {Concurrent::CancelledFutureError}
      # @return [self]
      def cancel(reason = nil)
        return self unless @Cancelled.make_true # false if already cancelled
        @Reason = reason
        cancel_graph reason
        self
      end

      # For inspection. Registered members as `[node, owned?]`.
      # @!visibility private
      def nodes
        @Nodes.each.to_a
      end

      # For inspection. Members whose execution this scope may cancel.
      # @!visibility private
      def owned_nodes
        @Nodes.each.select { |_, owned| owned }.map(&:first)
      end

      # For inspection. Members this scope only borrows.
      # @!visibility private
      def borrowed_nodes
        @Nodes.each.reject { |_, owned| owned }.map(&:first)
      end

      private

      def member?(node)
        @Nodes.each.any? { |n, _| n.equal? node }
      end

      def owned_member?(node)
        entry = @Nodes.each.find { |n, _| n.equal? node }
        !!entry && entry.last
      end

      # Events have no cancelled terminal state, they can only be resolved, so
      # a scope cancellation never applies to them.
      def cancellable?(node)
        node.is_a?(Future)
      end

      # Leaves-first fixed point. A node survives (protects its whole upstream
      # chain) when it is borrowed, an Event (which cannot be cancelled),
      # already resolved, or has a surviving in-scope dependent. Everything
      # owned which is not protected is cancelled without interrupting already
      # running blocks.
      def cancel_graph(reason)
        members = @Nodes.map(&:first).uniq
        # a node is protected if it must keep running for the scope
        protected = {}
        visiting  = {}

        protect = lambda do |node|
          return protected[node] if protected.key?(node)
          return true if visiting[node] # cycles are conservatively protected
          visiting[node] = true
          protected[node] =
            if !cancellable?(node) || !owned_member?(node) || node.resolved?
              true
            else
              node.dependent_nodes.any? do |dependent|
                member?(dependent) && protect.call(dependent)
              end
            end
          visiting.delete node
          protected[node]
        end

        members.each { |node| protect.call node }

        members.each do |node|
          node.apply_cancellation self, reason if cancellable?(node) && !protected[node]
        end
      end
    end
  end
end