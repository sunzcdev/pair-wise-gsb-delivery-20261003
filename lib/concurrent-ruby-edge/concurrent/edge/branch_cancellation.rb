require 'concurrent/synchronization/object'
require 'concurrent/edge/cancellation'
require 'concurrent/promises'

module Concurrent
  module Promises

    # @!macro warn.edge
    #
    # Owned branch cancellation for futures.
    #
    # When one upstream computation fans out into multiple result chains
    # (branches) which later join, cancelling one branch must:
    #
    # * stop only the work which has not started yet in *that* branch,
    # * leave the shared upstream computation (and all other branches) running,
    # * notify running code only cooperatively (no {Thread.kill}),
    # * publish exactly one terminal state (`:cancelled`) even when
    #   cancellation and completion arrive simultaneously,
    # * release the branch's queued callbacks and the strong references they
    #   hold,
    # * never leave a joining combinator waiting forever for a cancelled input.
    #
    # A {Concurrent::Promises::Branch} tracks exactly the nodes which are owned
    # by one result chain. Ownership is explicit: it begins at {Future#branch}
    # and is transferred (not shared) by the chain-building methods
    # ({Branch#then}, {Branch#chain}, {Branch#rescue}, ...). Joining branches
    # with {Concurrent::Promises.zip_futures} (or any other combinator built
    # from plain futures) deliberately ends ownership of both inputs - the join
    # node is a new, unowned node - so a cancelled branch can never pull the
    # shared predecessor or the other branch down with it.
    #
    # @example diamond dependency graph
    #   root       = Concurrent::Promises.resolvable_future
    #   left       = root.branch
    #   right      = root.branch
    #   left_tail  = left.then  { |v| "L:#{v}" }
    #   right_tail = right.then { |v| "R:#{v}" }
    #   join       = Concurrent::Promises.zip_futures(left_tail.future,
    #                                                right_tail.future)
    #
    #   left.cancel                       # cancel only the left branch
    #   root.fulfill(1)                   # the shared upstream still completes
    #   right_tail.future.value!          # => "R:1"
    #   join.wait                         # the join never hangs
    #   left_tail.future.state            # => :cancelled
    class Branch < Concurrent::Synchronization::Object
      safe_initialization!

      class << self

        # @return [Branch] a new owned branch starting at `future`
        # @param [Future, Event] future the shared predecessor the branch forks from
        # @!visibility private
        def start(future)
          new future
        end
      end

      # The current tip of the owned chain.
      # @return [Future, Event]
      def future
        @Future
      end

      # @!visibility private
      def initialize(future, nodes = nil, cancellation = nil)
        super()
        @Future       = future
        # The current tip is the shared predecessor the branch forked from;
        # it is deliberately not part of the owned set so it can never be
        # cancelled with the branch. Only nodes created *after* forking are
        # owned.
        @Nodes        = nodes || []
        @Cancellation = cancellation ||
                        Concurrent::Cancellation.new(Promises.resolvable_event)
        @CancelReason = nil
      end

      # The cooperative cancellation token of the branch. Pass it (or its
      # {Concurrent::Cancellation#origin origin}) into tasks and check
      # {Concurrent::Cancellation#canceled?} or call
      # {Concurrent::Cancellation#check!} to stop work which is already running.
      # Nothing is ever force-killed.
      # @return [Concurrent::Cancellation]
      def cancellation
        @Cancellation
      end

      # Is the branch cancelled?
      # @return [true, false]
      def cancelled?
        @Cancellation.canceled?
      end

      # The current tip as a plain future, ending owned construction on the
      # receiver. Further combinators built from the returned future are
      # unowned and will never be cancelled through this branch.
      # @return [Future, Event]
      def to_future
        @Future
      end

      alias_method :event, :to_future
      alias_method :to_event, :to_future

      # @return [Branch]
      def then(*args, &task)
        adopt @Future.then(*args, &wrap(task))
      end

      # @param [Executor, :io, :fast] executor
      # @return [Branch]
      def then_on(executor, *args, &task)
        adopt @Future.then_on(executor, *args, &wrap(task))
      end

      # @return [Branch]
      def chain(*args, &task)
        adopt @Future.chain(*args, &wrap(task))
      end

      # @param [Executor, :io, :fast] executor
      # @return [Branch]
      def chain_on(executor, *args, &task)
        adopt @Future.chain_on(executor, *args, &wrap(task))
      end

      # @return [Branch]
      def rescue(*args, &task)
        adopt @Future.rescue(*args, &wrap(task))
      end

      # @param [Executor, :io, :fast] executor
      # @return [Branch]
      def rescue_on(executor, *args, &task)
        adopt @Future.rescue_on(executor, *args, &wrap(task))
      end

      # @return [Branch]
      def flat_future(level = 1)
        adopt @Future.flat_future(level)
      end

      alias_method :flat, :flat_future

      # Cancels the whole owned branch.
      #
      # * Every not-yet-started continuation owned by the branch detaches: its
      #   task is neither posted nor retained, it reaches the `:cancelled`
      #   terminal state and the cancellation cascades along the owned chain.
      # * The shared predecessor the branch forked from is never touched, so
      #   other branches depending on it complete normally.
      # * Already running tasks only receive the cooperative {#cancellation}
      #   signal; they finish on their own and may still win the race.
      # * A join node consuming the branch never hangs: it is notified about
      #   the cancelled input and reaches its own terminal state.
      #
      # Cancelling twice or cancelling a branch whose tip already completed is
      # a no-op.
      #
      # @param [Object] reason optional cancellation reason
      # @return [true, false] true if the branch transitioned to cancelled
      def cancel(reason = Concurrent::CancelledFutureError.new('branch cancelled'))
        return false if @Cancellation.canceled?

        # If the entire owned chain already reached a terminal state (e.g. the
        # last task just completed), there is nothing to stop any more.
        affected = @Nodes
        return false if !affected.empty? && affected.all?(&:resolved?)

        # Signal running code first, cooperatively. No thread is ever killed.
        @CancelReason = reason
        @Cancellation.origin.resolve

        # Publish the cancelled terminal state on every still-pending node of
        # the owned chain. Ownership is linear (each node has a single branch
        # owner), so cascading cancellations can never reach a node shared with
        # another branch. Each CAS publishes at most one terminal state.
        @Nodes = []
        affected.each do |node|
          node.send :cancel_with, reason if node.respond_to?(:cancel_with, true)
        end
        true
      end

      # For inspection: the nodes currently owned by the branch.
      # @!visibility private
      def owned_nodes
        @Nodes.dup
      end

      private

      # Runs `task` under the branch's cooperative cancellation token: a task
      # which has not started yet while the branch is cancelled is never
      # invoked, and a running task receives the token as its last argument and
      # may bail out itself.
      def wrap(task)
        return nil unless task
        cancellation = @Cancellation
        # lambdas are strict about arity, only provide the token when requested;
        # non-lambda procs silently ignore extra arguments
        pass_token =
          if task.lambda?
            arity = task.arity
            arity < 0 || arity >= 1
          else
            true
          end
        lambda do |*args|
          cancellation.check! if cancellation.canceled?
          if pass_token
            task.call(*args, cancellation)
          else
            task.call(*args)
          end
        end
      end

      def adopt(future)
        @Future = future
        @Nodes.push future
        # forking a continuation from an already cancelled branch detaches the
        # new node immediately instead of running its task
        future.send(:cancel_with, @CancelReason) if @Cancellation.canceled?
        self
      end
    end

    class Future < AbstractEventFuture
      # Forks a new owned branch from the receiver. The receiver itself is the
      # shared predecessor and is *not* cancelled when the branch is cancelled.
      #
      # @return [Concurrent::Promises::Branch]
      # @!macro warn.edge
      def branch
        Branch.start self
      end
    end

    class Event < AbstractEventFuture
      # Forks a new owned branch from the receiver.
      # @return [Concurrent::Promises::Branch]
      # @!macro warn.edge
      def branch
        Branch.start self
      end
    end
  end
end
