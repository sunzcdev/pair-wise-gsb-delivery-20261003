require 'concurrent/future'
require 'concurrent/errors'
require 'concurrent/synchronization/lockable_object'

module Concurrent

  # A stable, position-addressed account of the inputs of a dataflow node and of
  # the node itself.
  #
  # Dependencies are identified by their position among the node's inputs, in
  # that same order, and keep that identity no matter in which order they
  # settled. Each dependency carries its state (`:fulfilled` or `:rejected`) and,
  # when rejected, the reason. A fulfilled dependency is never considered
  # missing, not even when its value is `nil`; the dependency's value itself is
  # not retained.
  #
  # The explanation also records whether the node's block actually started. That
  # is what separates a node rejected because a dependency failed (the block
  # never ran) from a node whose own block raised (the block started and failed).
  #
  # The explanation keeps only copied state. It holds no reference to the input
  # objects and none to the node, so a settled node does not keep its dependency
  # graph alive merely for the sake of an explanation. The node's own task
  # likewise closes over the block, not over the array of inputs.
  #
  # @example
  #   a = Concurrent::dataflow { raise 'a failed' }
  #   b = Concurrent::dataflow { 2 }
  #   c = Concurrent::dataflow!(a, b) { |av, bv| av + bv }
  #
  #   c.reason                              #=> #<Concurrent::DataflowDependencyError ...>
  #   c.reason.failed_indexes               #=> [0]
  #   c.reason.reasons.map(&:message)       #=> ["a failed"]
  #   c.dataflow_explanation.started?       #=> false
  #   c.dataflow_explanation[1].fulfilled?  #=> true
  class DataflowExplanation < Synchronization::LockableObject

    # The state of one input at a given position.
    class Dependency

      # @return [Integer] position of the dependency among the node's inputs
      attr_reader :index

      # @return [Symbol] `:fulfilled`, `:rejected`, or `:pending`
      attr_reader :state

      # @return [Exception, nil] rejection reason, `nil` otherwise
      attr_reader :reason

      # @param [Integer] index position among the node's inputs
      # @param [Symbol] state the dependency's state
      # @param [Exception, nil] reason the rejection reason
      def initialize(index, state, reason = nil)
        @index  = index
        @state  = state
        @reason = reason
        freeze
      end

      # @return [Boolean] true when the dependency fulfilled, even with `nil`
      def fulfilled?
        @state == :fulfilled
      end

      # @return [Boolean] true when the dependency was rejected
      def rejected?
        @state == :rejected
      end

      # @return [Boolean] true when the dependency has not settled yet
      def pending?
        @state == :pending
      end

      # @return [Boolean] true when the dependency fulfilled or was rejected
      def complete?
        fulfilled? || rejected?
      end

      # @return [Hash] position, state, and rejection reason
      def to_h
        { index: @index, state: @state, reason: @reason }
      end

      # @return [String]
      def inspect
        detail = @reason.nil? ? '' : format(' reason=%s: %s', @reason.class, @reason.message)
        format('#<%s index=%d state=%s%s>', self.class, @index, @state, detail)
      end
    end

    # @param [Integer] size the number of inputs of the node
    def initialize(size)
      super()
      synchronize do
        @started      = false
        @state        = :pending
        @reason       = nil
        @failure_kind = nil
        @dependencies = Array.new(size) { |index| Dependency.new(index, :pending) }
      end
    end

    # Records the outcome of the dependency at the given position.
    #
    # @param [Integer] index position among the node's inputs
    # @param [Symbol] state `:fulfilled` or `:rejected`
    # @param [Exception, nil] reason the rejection reason
    # @return [DataflowExplanation] self
    #
    # @api private
    def record_dependency(index, state, reason = nil)
      synchronize { @dependencies[index] = Dependency.new(index, state, reason) }
      self
    end

    # Marks that the node's block was actually started.
    #
    # @return [DataflowExplanation] self
    #
    # @api private
    def mark_started
      synchronize { @started = true }
      self
    end

    # Records the node's own terminal state.
    #
    # @param [Symbol] state `:fulfilled` or `:rejected`
    # @param [Exception, nil] reason the rejection reason
    # @param [Symbol, nil] failure_kind how the node came to be rejected:
    #   `:dependency` when a dependency failed before the block ran,
    #   `:block` when the block itself raised, `nil` when it did not reject
    # @return [DataflowExplanation] self
    #
    # @api private
    def mark_settled(state, reason = nil, failure_kind = nil)
      synchronize do
        # The first terminal transition wins, mirroring the node's own
        # single-assignment state; a later transition never rewrites it.
        if @state == :pending
          @state        = state
          @reason       = reason
          @failure_kind = failure_kind
        end
      end
      self
    end

    # Did the node's block actually start executing?
    #
    # A node rejected because of a failed dependency never starts, so this stays
    # `false` for it even though the node is complete.
    #
    # @return [Boolean]
    def started?
      synchronize { @started }
    end

    # @return [Integer] the number of inputs of the node
    def size
      synchronize { @dependencies.size }
    end

    # @param [Integer] index position among the node's inputs
    # @return [Dependency, nil] the dependency at that position
    def [](index)
      synchronize { @dependencies[index] }
    end

    # @return [Array<Dependency>] every dependency, ordered by input position
    def dependencies
      synchronize { @dependencies.dup.freeze }
    end

    # @return [Array<Dependency>] the fulfilled dependencies, in input order
    def fulfilled_dependencies
      dependencies.select(&:fulfilled?)
    end

    # @return [Array<Dependency>] the rejected dependencies, in input order
    def failed_dependencies
      dependencies.select(&:rejected?)
    end
    alias_method :rejected_dependencies, :failed_dependencies

    # @return [Array<Dependency>] the unsettled dependencies, in input order
    def pending_dependencies
      dependencies.select(&:pending?)
    end

    # @return [Array<Integer>] positions of the rejected dependencies, ascending
    #   regardless of the order in which they settled
    def failed_indexes
      failed_dependencies.map(&:index)
    end

    # @return [Symbol] the node's own state: `:pending`, `:fulfilled`, or `:rejected`
    def state
      synchronize { @state }
    end

    # @return [Boolean] has the node fulfilled?
    def fulfilled?
      state == :fulfilled
    end

    # @return [Boolean] has the node been rejected?
    def rejected?
      state == :rejected
    end

    # @return [Boolean] is the node still waiting for its dependencies or block?
    def pending?
      state == :pending
    end

    # @return [Boolean] has the node completed?
    def complete?
      rejected? || fulfilled?
    end
    alias_method :settled?, :complete?

    # @return [Exception, nil] the node's rejection reason, `nil` otherwise
    def reason
      synchronize { @reason }
    end

    # Was the node rejected without its block ever running because a dependency
    # failed?
    #
    # @return [Boolean]
    def dependency_failure?
      rejected? && failure_kind == :dependency
    end

    # Was the node rejected because its own block raised?
    #
    # @return [Boolean]
    def block_failure?
      rejected? && failure_kind == :block
    end

    # How did the node come to be rejected? `:dependency` when a dependency
    # failed before the block ran, `:block` when the block itself raised,
    # `:cancelled` when the node was cancelled, and `nil` when the node did not
    # reject.
    #
    # @return [Symbol, nil]
    def failure_kind
      synchronize { @failure_kind }
    end

    # Was the node cancelled before it completed?
    #
    # @return [Boolean]
    def cancelled?
      rejected? && failure_kind == :cancelled
    end

    # @return [Hash] the node's state and every dependency's state, in a plain
    #   nested Hash
    def to_h
      { started: started?, state: state, reason: reason,
        failure_kind: failure_kind, dependencies: dependencies.map(&:to_h) }
    end

    # @return [String]
    def inspect
      format('#<%s started=%s state=%s failure_kind=%s dependencies=%s>',
             self.class, started?, state, failure_kind.inspect, dependencies.inspect)
    end
  end

  # Mixed into the `Future` returned by {Concurrent.dataflow},
  # {Concurrent.dataflow_with}, {Concurrent.dataflow!}, and
  # {Concurrent.dataflow_with!}.
  module DataflowResult

    # The optional account of what happened to this dataflow node and to each of
    # its dependencies. It is available immediately; while the node still waits
    # for dependencies they are reported as `:pending`.
    #
    # @return [DataflowExplanation]
    def dataflow_explanation
      @__dataflow_explanation__
    end

    # Cancels the node and records the cancellation in the explanation, so the
    # explanation never disagrees with the `Future` it belongs to.
    #
    # @return [Boolean] was the operation successfully cancelled
    def cancel
      cancelled = super
      @__dataflow_explanation__.mark_settled(:rejected, reason, :cancelled) if cancelled
      cancelled
    end
  end

  # Waits for all inputs of a dataflow node and then runs the node's block
  # exactly once.
  #
  # Each input's outcome is stored at the input's own position, so the order in
  # which the inputs settle never changes the reported order. The callback runs
  # once, outside the lock, when the last input has settled.
  #
  # @!visibility private
  class DataflowBarrier < Synchronization::LockableObject # :nodoc:

    def initialize(size, &block)
      super()
      synchronize do
        @remaining = size
        @states    = Array.new(size)
        @reasons   = Array.new(size)
        @values    = Array.new(size)
        @callback  = block
        @settled   = false
      end
    end

    # Notes the outcome of the dependency at the given position.
    #
    # @param [Integer] index position among the node's inputs
    # @param [Symbol] state `:fulfilled` or `:rejected`
    # @param [Object] value the dependency's value when it fulfilled
    # @param [Exception, nil] reason the dependency's rejection reason
    # @return [DataflowBarrier] self
    def record(index, state, value, reason)
      callback = nil
      synchronize do
        unless @settled
          @states[index]  = state
          @reasons[index] = reason
          @values[index]  = value
          @remaining     -= 1
          if @remaining <= 0
            @settled  = true
            callback  = @callback
            @callback = nil
            ns_broadcast
          end
        end
      end
      callback.call(self) if callback
      self
    end

    # A single consistent snapshot of every dependency's state and reason, in
    # input order.
    #
    # @return [Array<Array(Symbol, Exception)>]
    def outcomes
      synchronize do
        states  = @states || []
        reasons = @reasons || []
        states.each_with_index.map { |state, index| [state, reasons[index]] }
      end
    end

    # @return [Boolean] was any dependency rejected?
    def rejected?
      synchronize { (@states || []).any? { |state| state == :rejected } }
    end

    # The dependencies' values in input order. The values are handed over once
    # and released, so a settled node keeps no reference to them.
    #
    # When the node is executed before every dependency has settled — for
    # example because it was executed by hand — this waits for the remaining
    # dependencies instead of handing over partially filled values.
    #
    # @return [Array<Object>]
    def take_values
      values = nil
      synchronize do
        ns_wait_until { @settled }
        values   = @values || []
        @values  = nil
        @states  = nil
        @reasons = nil
      end
      values
    end

    # Releases everything the barrier still holds.
    #
    # @return [nil]
    def discard
      synchronize do
        @values   = nil
        @states   = nil
        @reasons  = nil
        @callback = nil
      end
      nil
    end
  end

  # Dataflow allows you to create a task that will be scheduled when all of its data dependencies are available.
  # {include:file:docs-source/dataflow.md}
  #
  # The returned `Future` responds to {DataflowResult#dataflow_explanation},
  # which reports each dependency by its input position and whether this node's
  # block actually started.
  #
  # The `!` variants never run the block when a dependency was rejected. The
  # returned `Future` is then rejected with {DataflowDependencyError}, which
  # names the failed dependencies by position instead of re-raising one of their
  # exceptions as if the block had raised it.
  #
  # @param [Future] inputs zero or more `Future` operations that this dataflow depends upon
  #
  # @yield The operation to perform once all the dependencies are met
  # @yieldparam [Future] inputs each of the `Future` inputs to the dataflow
  # @yieldreturn [Object] the result of the block operation
  #
  # @return [Future] a `Future` with a {DataflowResult#dataflow_explanation}
  #
  # @raise [ArgumentError] if no block is given
  # @raise [ArgumentError] if any of the inputs are not `IVar`s
  def dataflow(*inputs, &block)
    dataflow_with(Concurrent.global_io_executor, *inputs, &block)
  end
  module_function :dataflow

  def dataflow_with(executor, *inputs, &block)
    call_dataflow(:value, executor, *inputs, &block)
  end
  module_function :dataflow_with

  def dataflow!(*inputs, &block)
    dataflow_with!(Concurrent.global_io_executor, *inputs, &block)
  end
  module_function :dataflow!

  def dataflow_with!(executor, *inputs, &block)
    call_dataflow(:value!, executor, *inputs, &block)
  end
  module_function :dataflow_with!

  private

  def call_dataflow(method, executor, *inputs, &block)
    raise ArgumentError.new('an executor must be provided') if executor.nil?
    raise ArgumentError.new('no block given') unless block_given?
    unless inputs.all? { |input| input.is_a? IVar }
      raise ArgumentError.new("Not all dependencies are IVars.\nDependencies: #{ inputs.inspect }")
    end

    strict      = method == :value!
    explanation = DataflowExplanation.new(inputs.size)

    # The node and its barrier refer to each other, so both are created empty
    # and filled through this holder. Building them in their own methods keeps
    # the node's task and callback from closing over `inputs`, which is why a
    # settled node never keeps the dependency graph alive.
    holder = { barrier: nil, result: nil }

    result = Future.new(executor: executor, &dataflow_task(block, holder, explanation))
    result.extend(DataflowResult)
    result.instance_variable_set(:@__dataflow_explanation__, explanation)
    holder[:result] = result

    if inputs.empty?
      result.execute
    else
      barrier = DataflowBarrier.new(
        inputs.size, &dataflow_completion(explanation, strict, holder))
      holder[:barrier] = barrier

      inputs.each_with_index do |input, index|
        input.add_observer do |_time, value, dependency_reason|
          # The observer's `reason` is `nil` for a fulfilled dependency, but an
          # `IVar` may also be rejected with a `nil` reason, so the source's
          # state — not the reason — decides the dependency's identity.
          barrier.record(index, input.state, value, dependency_reason)
        end
      end
    end

    result
  end
  module_function :call_dataflow

  # Builds the task a dataflow node runs once its inputs are available.
  #
  # The task closes over the block, the holder, and the explanation — never over
  # the array of inputs.
  #
  # @return [Proc]
  #
  # @!visibility private
  def dataflow_task(block, holder, explanation)
    lambda do
      explanation.mark_started
      begin
        barrier = holder[:barrier]
        holder[:barrier] = nil
        # `take_values` releases the collected values as it hands them over, so
        # once the block has run nothing of the dependencies is retained.
        values  = barrier ? barrier.take_values : []
        value   = block.call(*values)
        explanation.mark_settled(:fulfilled)
        value
      rescue Exception => ex
        # Record the block's own failure before re-raising, so it is never
        # confused with a dependency failure.
        explanation.mark_settled(:rejected, ex, :block)
        raise
      end
    end
  end
  module_function :dataflow_task

  # Builds the callback a dataflow node runs when its last input settles.
  #
  # The callback closes over the explanation, the strictness, and the holder —
  # never over the array of inputs.
  #
  # @return [Proc]
  #
  # @!visibility private
  def dataflow_completion(explanation, strict, holder)
    lambda do |completed|
      completed.outcomes.each_with_index do |(state, reason), index|
        explanation.record_dependency(index, state, reason)
      end

      if strict && completed.rejected?
        # A dependency failed: do not schedule this node. Reject it here so the
        # failure is attributed to the dependency and not to the node's block.
        completed.discard
        holder[:barrier] = nil
        reject_due_to_dependency(holder[:result], explanation)
      else
        holder[:result].execute
      end
    end
  end
  module_function :dataflow_completion

  # Rejects a node that must not run because one of its dependencies failed.
  #
  # The node is never posted to its executor, so failure propagation cannot
  # schedule it a second time. If the node was already executed by hand and its
  # block therefore already started (or the node already settled), it is left
  # alone rather than relabelled as a dependency failure it is not.
  #
  # @param [Future] result the node
  # @param [DataflowExplanation] explanation the node's explanation
  # @return [Boolean] true when the node was rejected here
  #
  # @!visibility private
  def reject_due_to_dependency(result, explanation)
    return false if result.complete? || explanation.started?
    reason = DataflowDependencyError.new(explanation)
    explanation.mark_settled(:rejected, reason, :dependency)
    result.fail(reason)
    true
  end
  module_function :reject_due_to_dependency
end
