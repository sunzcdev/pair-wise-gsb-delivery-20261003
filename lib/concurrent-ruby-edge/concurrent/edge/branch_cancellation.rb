require 'concurrent/promises'
require 'concurrent/edge/cancellation'
require 'concurrent/atomic/atomic_fixnum'
require 'concurrent/atomic/atomic_boolean'
require 'concurrent/synchronization/object'

module Concurrent
  module Promises

    # @!visibility private
    #
    # Internal terminal state used to propagate a branch cancellation through the
    # ownership graph. It behaves as a rejection carrying {CancelledOperationError},
    # but it is distinguishable so that not-started continuations (+rescue+/+chain+)
    # can be skipped instead of executing their tasks.
    class CancelledResolution < InternalStates::Rejected
      DEFAULT_REASON = Concurrent::CancelledOperationError.new(
          'branch was cancelled before its work was started').freeze

      def initialize(reason = DEFAULT_REASON)
        super
      end
    end

    # A node of a branch-cancellation ownership graph.
    #
    # The node owns its incoming edges (one per parent dependency), its task and
    # the cooperative cancellation signal of any running work. Ownership rules:
    #
    # * Cancelling a node never cancels its parents - predecessors still needed by
    #   other branches keep running. Only incoming edges are detached.
    # * A single-parent node whose parent terminates cancelled follows the
    #   cancellation (it is an exclusive continuation) and skips its task.
    # * A multi-parent (combination) node is forced cancelled only if all its
    #   inputs terminated cancelled; otherwise the cancelled edge is abandoned and
    #   the combination still resolves from the remaining (terminal) inputs, so it
    #   can never wait forever for a cancelled input.
    #
    # @!visibility private
    class BranchCancellationNode < Synchronization::Object
      safe_initialization!

      PENDING   = 0 # inputs not all ready, task not scheduled
      SCHEDULED = 1 # task posted to an executor, not started
      RUNNING   = 2 # task started
      FINISHED  = 3 # terminal state published

      # A live edge leading to one parent dependency.
      class Edge < Synchronization::Object
        safe_initialization!
        def initialize(future, source_node)
          super()
          @Future     = future
          @SourceNode = source_node
          @Handle     = nil
          @Dead       = AtomicBoolean.new(false)
          @Consumed   = AtomicBoolean.new(false)
        end

        # @return [AbstractEventFuture]
        def future
          @Future
        end

        # @return [BranchCancellationNode, nil] owning node when internal, nil for external futures
        def source_node
          @SourceNode
        end

        def handle=(value)
          @Handle = value
        end

        # Claims the single right to deliver the parent's resolution.
        # @return [true, false]
        def claim
          @Consumed.make_true
        end

        # Detaches the registration from the parent callback stack, abandoning
        # both the notification and the strong references it keeps.
        def abandon
          return false unless @Dead.make_true
          @Consumed.make_true
          @Handle.call if @Handle
          true
        end

        def dead?
          @Dead.true?
        end
      end

      def future
        @Future
      end

      def cancellation
        @Cancellation
      end

      def default_executor
        @DefaultExec
      end

      # @param [Symbol] kind +:then+, +:rescue+, +:chain+, +:zip+, +:root+
      # @param [Array<Array(AbstractEventFuture, BranchCancellationNode)>] parents
      #   each entry is the parent future together with its owning node (nil for
      #   an externally supplied future)
      def initialize(kind, parents, executor, args, task, default_executor, source = nil)
        super()
        @DefaultExec  = default_executor || (parents.first && parents.first.first.default_executor) || :io
        @Future       = source || Concurrent::Promises.resolvable_future_on(@DefaultExec)
        @Kind         = kind
        @Executor     = executor
        @Args         = args
        @Task         = task
        @Mutex        = Mutex.new
        @Lifecycle    = AtomicFixnum.new(PENDING)
        @Remaining    = AtomicFixnum.new(parents.size)
        @Inputs       = ::Array.new(parents.size)
        @Edges        = parents.map { |future, node| Edge.new(future, node) }
        @SignalEvent  = Concurrent::Promises.resolvable_event
        @Cancellation = Concurrent::Cancellation.new(@SignalEvent)

        if kind == :root
          @Lifecycle.value = (FINISHED) if source.resolved?
          source.send(:add_callback, :callback_notify_branch, self, :root) unless source.resolved?
        else
          register_edges
        end
      end

      # Builds a root node mirroring an externally owned (possibly shared) future.
      # Root nodes are never force-cancelled - cancelling only signals cooperative
      # cancellation to running work, the upstream computation stays alive.
      def self.root(source, default_executor = :io)
        new(:root, [], nil, nil, nil, default_executor, source)
      end

      def canceled?
        @Future.internal_state.is_a?(CancelledResolution)
      end

      # User entry point.
      # @return [true, false] true if this branch was cancelled, false if already terminal
      def cancel(reason = CancelledResolution::DEFAULT_REASON)
        # A root mirrors an externally owned (possibly shared) future. It is never
        # force-cancelled: cancelling only signals cooperative cancellation to the
        # running upstream task, the shared computation keeps its own terminal state.
        return signal_root if @Kind == :root

        @Mutex.synchronize do
          return false if @Lifecycle.value == FINISHED

          case @Lifecycle.value
          when PENDING
            signal
            cancel_while_pending(reason)
          when SCHEDULED
            # posted but not started yet: it still belongs to this branch
            signal
            publish(CancelledResolution.new(reason))
          when RUNNING
            # running code is only notified cooperatively, never Thread#kill;
            # it keeps the right to publish its own completion
            signal
          end
        end
        # notify running exclusive predecessors that their result is no longer
        # needed by this branch; advisory only, it never cancels them
        signal_exclusive_ancestors
        true
      end

      # Called by the removable callback installed on each parent.
      def on_input_resolution(parent, index)
        if index == :root
          @Lifecycle.compare_and_set(PENDING, FINISHED)
          return
        end

        edge = @Edges[index]
        # The edge was abandoned by an ownership decision; do not consume it.
        return unless edge.claim

        consume_input(index, parent.internal_state)
      end

      private

      def register_edges
        @Edges.each_with_index do |edge, index|
          parent = edge.future
          if parent.resolved?
            next unless edge.claim
            consume_input(index, parent.internal_state)
          else
            edge.handle = parent.send(:add_removable_callback, :callback_notify_branch, self, index)
            # re-check after publishing the handle to close the registration window
            if parent.resolved? && edge.claim
              consume_input(index, parent.internal_state)
            end
          end
        end
      end

      def consume_input(index, internal_state)
        @Inputs[index] = internal_state
        @Remaining.decrement

        if internal_state.is_a?(CancelledResolution)
          # A cancelled input is a terminal state; the combination must not wait
          # for it. Ownership is decided before the edge is abandoned.
          handle_cancelled_input(index)
        elsif @Remaining.value.zero?
          schedule
        end
      end

      def handle_cancelled_input(index)
        @Mutex.synchronize do
          @Edges[index].abandon
          return if @Lifecycle.value == FINISHED

          others = @Edges.each_with_index.reject { |_, i| i == index }
          if others.all? { |edge, _| edge.dead? || edge.future.resolved? }
            # every other input is terminal too; this node is exclusively owned
            # by the cancelled branch -> cancel its not-started work
            publish(combined_cancelled_state) if @Lifecycle.value == PENDING
          elsif @Remaining.value <= 0
            # only detached edges remain unsettled; resolve from what we have
            publish_zip_like
          end
        end
      end

      def combined_cancelled_state
        cancelled = @Inputs.select { |state| state.is_a?(CancelledResolution) }
        cancelled.size == 1 ? cancelled.first : CancelledResolution.new
      end

      def schedule
        @Mutex.synchronize do
          return unless @Lifecycle.compare_and_set(PENDING, SCHEDULED)
          execute
        end
      end

      def execute
        first = @Inputs.first
        case @Kind
        when :then
          if first.fulfilled?
            post_task { @Task.call(first.value, *@Args) }
          else
            publish(first)
          end
        when :rescue
          if first.fulfilled? || first.is_a?(CancelledResolution)
            publish(first)
          else
            post_task { @Task.call(first.reason, *@Args) }
          end
        when :chain
          if first.is_a?(CancelledResolution)
            publish(first)
          elsif first.fulfilled?
            post_task { @Task.call(true, first.value, nil, *@Args) }
          else
            post_task { @Task.call(false, nil, first.reason, *@Args) }
          end
        when :zip
          publish_zip_like
        end
      end

      def post_task
        Concurrent.executor(@Executor).post do
          @Mutex.synchronize do
            # a scheduled task which lost against cancellation never runs
            return unless @Lifecycle.compare_and_set(SCHEDULED, RUNNING)
          end

          begin
            value = yield
            # only the first terminal transition wins
            if @Lifecycle.compare_and_set(RUNNING, FINISHED)
              publish_state(InternalStates::Fulfilled.new(value))
            end
          rescue Exception => error # rubocop:disable Lint/RescueException
            # Covers a cooperative check! raising CancelledOperationError as well
            # as any other task failure; publishing rejects with the reason. No
            # Thread is ever killed and only the first terminal transition wins.
            if @Lifecycle.compare_and_set(RUNNING, FINISHED)
              publish_state(InternalStates::Rejected.new(error))
            end
          end
        end
      end

      def publish_zip_like
        states        = @Inputs
        all_fulfilled = true
        values        = ::Array.new(states.size)
        reasons       = ::Array.new(states.size)
        states.each_with_index do |state, i|
          fulfilled, values[i], reasons[i] = state.result
          all_fulfilled &&= fulfilled
        end
        if all_fulfilled
          publish(InternalStates::FulfilledArray.new(values))
        else
          publish(InternalStates::PartiallyRejected.new(values, reasons))
        end
      end

      def signal
        @SignalEvent.resolve(false) if @SignalEvent.pending?
      end

      # Signals a root's cooperative cancellation without touching the shared
      # upstream future. Returns false once the root has resolved.
      def signal_root
        return false if @Future.resolved?
        signal
        true
      end

      # Delivers the cooperative signal to running predecessors reachable through
      # edges owned exclusively by this branch. The traversal is advisory only:
      # it never cancels, publishes, or Thread#kill anything; shared nodes (roots
      # over external futures, or combinations with other live consumers) are not
      # crossed, since another branch may still need their result.
      def signal_exclusive_ancestors
        @Edges.each do |edge|
          node = edge.source_node
          next unless node
          next unless node.send(:running?)

          node.send(:notify_cooperative_cancellation)
          node.signal_exclusive_ancestors
        end
      end

      def running?
        @Lifecycle.value == RUNNING
      end

      def notify_cooperative_cancellation
        @SignalEvent.resolve(false) if @SignalEvent.pending?
      end

      def cancel_while_pending(reason)
        # Detach every incoming edge: the not-started continuation is abandoned.
        # Parents are NOT cancelled here, each stays alive for its other branches.
        @Edges.each(&:abandon)
        publish(CancelledResolution.new(reason))
      end

      # All callers hold @Mutex; sets the single terminal state.
      def publish(state)
        @Lifecycle.value = (FINISHED)
        publish_state(state)
      end

      def publish_state(state)
        @Future.resolve_with(state, false)
        cleanup_references
      end

      def cleanup_references
        @Edges.each(&:abandon)
        @Inputs.fill(nil)
        @Args     = nil
        @Task     = nil
        @Executor = nil
      end
    end

    # A {Future} which belongs to an explicit, cancellable branch.
    #
    # All combinators ({#then}, {#rescue}, {#chain}, {#zip}) build nodes with
    # tracked ownership. Cancelling a branch detaches its not-started
    # continuations and clears the strong references kept on its predecessors'
    # callback stacks, without cancelling predecessors shared with other branches.
    class CancellableFuture
      # @!visibility private
      def initialize(node)
        @Node = node
      end

      # @!visibility private
      def node
        @Node
      end

      # @return [Future] the underlying future
      def to_future
        @Node.future
      end
      alias_method :future, :to_future

      # Cancels this branch.
      # @return [true, false]
      def cancel(reason = CancelledResolution::DEFAULT_REASON)
        @Node.cancel(reason)
      end

      # Cooperative signal delivered to running work. Tasks capture it before
      # cancelling and call `cancellation.check!` at safe points.
      # @return [Cancellation]
      def cancellation
        @Node.cancellation
      end

      # @return [true, false]
      def canceled?
        @Node.canceled?
      end

      def then(*args, &task)
        then_on(@Node.default_executor, *args, &task)
      end

      def then_on(executor, *args, &task)
        build(:then, executor, args, task)
      end

      def rescue(*args, &task)
        rescue_on(@Node.default_executor, *args, &task)
      end

      def rescue_on(executor, *args, &task)
        build(:rescue, executor, args, task)
      end

      def chain(*args, &task)
        chain_on(@Node.default_executor, *args, &task)
      end

      def chain_on(executor, *args, &task)
        build(:chain, executor, args, task)
      end

      def zip(other)
        if other.is_a?(CancellableFuture)
          pair = [other.to_future, other.node]
        else
          pair = [other, nil]
        end
        default = @Node.default_executor
        node    = BranchCancellationNode.new(
            :zip, [[to_future, @Node], pair], nil, nil, nil, default)
        CancellableFuture.new(node)
      end

      # @!group Delegated inspection and waiting
      def state;            @Node.future.state;            end
      def pending?;         @Node.future.pending?;         end
      def resolved?;        @Node.future.resolved?;        end
      def fulfilled?;       @Node.future.fulfilled?;       end
      def rejected?;        @Node.future.rejected?;        end
      def value(*args);     @Node.future.value(*args);     end
      def value!(*args);    @Node.future.value!(*args);    end
      def reason(*args);    @Node.future.reason(*args);    end
      def result(*args);    @Node.future.result(*args);    end
      def wait(*args);      @Node.future.wait(*args);      end
      def wait!(*args);     @Node.future.wait!(*args);     end
      def touch;            @Node.future.touch; self;      end
      def default_executor; @Node.default_executor;        end
      def on_resolution!(*args, &cb);  @Node.future.on_resolution!(*args, &cb);  self; end
      def on_fulfillment!(*args, &cb); @Node.future.on_fulfillment!(*args, &cb); self; end
      def on_rejection!(*args, &cb);   @Node.future.on_rejection!(*args, &cb);   self; end
      def to_s;             @Node.future.to_s;             end
      alias_method :inspect, :to_s
      # @!endgroup

      private

      def build(kind, executor, args, task)
        executor ||= @Node.default_executor
        node = BranchCancellationNode.new(
            kind, [[to_future, @Node]], executor, args, task, executor)
        CancellableFuture.new(node)
      end
    end

    # Entry point of the branch-cancellation mechanism. A branch is an ownership
    # scope rooted at a shared upstream computation.
    #
    # @example diamond dependency graph
    #   branch = Promises.branch
    #   shared = Promises.resolvable_future
    #   origin = branch.wrap(shared)
    #   left   = origin.then { 1 }
    #   right  = origin.then { 2 }
    #   merge  = left.zip(right)
    #
    #   left.cancel                 # only cancels the left branch
    #   shared.fulfill(:ok)
    #   right.value!                # => 2
    #   merge.result                # => [false, nil, [CancelledOperationError, nil]]
    class BranchCancellation
      def initialize(default_executor = :io)
        @DefaultExecutor = default_executor
      end

      # Attaches ownership tracking to an existing future/event without changing
      # its computation. Cancelling the returned head only sends the cooperative
      # signal, it never cancels the wrapped (possibly shared) upstream.
      #
      # @param [AbstractEventFuture] future
      # @return [CancellableFuture]
      def wrap(future)
        CancellableFuture.new(BranchCancellationNode.root(future, @DefaultExecutor))
      end
    end

    module FactoryMethods
      # Creates a new ownership branch.
      # @!macro promises.param.default_executor
      # @return [BranchCancellation]
      def branch(default_executor = self.default_executor)
        BranchCancellation.new(default_executor)
      end
    end
  end
end
