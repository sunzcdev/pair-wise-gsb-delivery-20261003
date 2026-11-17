require 'concurrent/concern/dereferenceable'
require 'concurrent/errors'
require 'concurrent/synchronization/object'

module Concurrent

  # An `MVar` is a synchronized single element container. They are empty or
  # contain one item. Taking a value from an empty `MVar` blocks, as does
  # putting a value into a full one. You can either think of them as blocking
  # queue of length one, or a special kind of mutable variable.
  #
  # On top of the fundamental `#put` and `#take` operations, we also provide a
  # `#modify` that is atomic with respect to operations on the same instance.
  # These operations all support timeouts.
  #
  # We also support non-blocking operations `#try_put!` and `#try_take!`, a
  # `#set!` that ignores existing values, a `#value` that returns the value
  # without removing it or returns `MVar::EMPTY`, and a `#modify!` that yields
  # `MVar::EMPTY` if the `MVar` is empty and can be used to set `MVar::EMPTY`.
  # You shouldn't use these operations in the first instance.
  #
  # `MVar` is a [Dereferenceable](Dereferenceable).
  #
  # `MVar` is related to M-structures in Id, `MVar` in Haskell and `SyncVar` in Scala.
  #
  # Note that unlike the original Haskell paper, our `#take` is blocking. This is how
  # Haskell and Scala do it today.
  #
  # @!macro copy_options
  #
  # ## Commit boundaries
  #
  # An `MVar` is often used as the single cell holding a platform configuration
  # which is replaced as a whole. Every operation therefore has exactly one
  # point at which the cell changes, and observers only ever see the state
  # before or after that point, never an intermediate value:
  #
  # * read (`#value`, `#borrow`, `#empty?`, `#full?`) never changes the cell.
  # * `#take` removes the value in one step; the cell becomes empty at that
  #   single point and empty waiters are signalled.
  # * `#put` and `#set!` install the new reference in one step; the cell becomes
  #   full at that single point and full waiters are signalled.
  # * `#modify`, `#modify!`, `#update` and `#try_update` run in two phases. The
  #   *prepare* phase reads the reference without changing the cell, then the
  #   block runs with the cell untouched, then the *commit* phase installs the
  #   value returned by the block. Nothing is published between the phases.
  #
  # If the block raises the update is *not* committed: the cell keeps the exact
  # reference it had before the block ran (the same object, not a copy or a
  # half-modified one) and the exception propagates from the raising methods.
  # `#update` and `#try_update` report the failure in an {UpdateResult} instead.
  # Mutations the block performed on the business object it was given are not
  # undone - `MVar` does not own that object - but such a partially modified
  # reference is never handed to waiters as a new value.
  #
  # A timeout only limits waiting for the cell to become readable, i.e. the
  # phase before anything is committed. Once the block is running the update
  # completes without a deadline. Timeouts are reported with the `TIMEOUT`
  # sentinel (or a `:timeout` update result) which is distinct from `EMPTY`
  # and from the values returned by the `try_*` methods.
  #
  # ## Re-entrancy
  #
  # While the block of `#modify`, `#modify!`, `#update`, `#try_update` or
  # `#borrow` runs, the current thread owns the `MVar`. Any nested operation on
  # the same instance from that thread is rejected with
  # {Concurrent::ReentrantOperationError} instead of blocking on the lock the
  # thread already holds.
  #
  # ## See Also
  #
  # 1. P. Barth, R. Nikhil, and Arvind. [M-Structures: Extending a parallel, non- strict, functional language with state](http://dl.acm.org/citation.cfm?id=652538). In Proceedings of the 5th
  #    ACM Conference on Functional Programming Languages and Computer Architecture (FPCA), 1991.
  #
  # 2. S. Peyton Jones, A. Gordon, and S. Finne. [Concurrent Haskell](http://dl.acm.org/citation.cfm?id=237794).
  #    In Proceedings of the 23rd Symposium on Principles of Programming Languages
  #    (PoPL), 1996.
  class MVar < Synchronization::Object
    include Concern::Dereferenceable
    safe_initialization!

    # Unique value that represents that an `MVar` was empty
    EMPTY = ::Object.new

    # Unique value that represents that an `MVar` timed out before it was able
    # to produce a value.
    TIMEOUT = ::Object.new

    # Result of an explicit update, returned by {#update} and {#try_update}.
    #
    # The status tells what happened to the cell:
    #
    # * `:updated` - the block completed and its result was committed.
    # * `:failed` - the block raised; nothing was committed and the cell still
    #   holds `#previous_value`, the reference the block was given.
    # * `:timeout` - the cell did not become readable within the timeout, the
    #   block never ran and nothing was committed.
    # * `:empty` - (`#try_update` only) the cell was empty, the block never ran.
    #
    # A rejected re-entrant update raises {Concurrent::ReentrantOperationError}
    # and is never reported as a result.
    class UpdateResult
      # @return [Symbol] one of `:updated`, `:failed`, `:timeout`, `:empty`
      attr_reader :status

      # @return [Object, nil] the value the block was given; `nil` unless the
      #   block ran (`:updated` and `:failed`)
      attr_reader :previous_value

      # @return [Object, nil] the value committed by the block; `nil` unless
      #   the status is `:updated`
      attr_reader :value

      # @return [Exception, nil] the exception raised by the block, present only
      #   for the `:failed` status
      attr_reader :error

      # @!visibility private
      def initialize(status, previous_value = nil, value = nil, error = nil)
        @status         = status
        @previous_value = previous_value
        @value          = value
        @error          = error
        freeze
      end

      # @return [Boolean] true when the block ran and its result was committed
      def updated?
        @status == :updated
      end
      alias_method :success?, :updated?
      alias_method :committed?, :updated?

      # @return [Boolean] true when the block raised and the original reference
      #   is still in the cell
      def failed?
        @status == :failed
      end

      # @return [Boolean] true when the cell did not become readable in time
      def timed_out?
        @status == :timeout
      end

      # @return [Boolean] true when `#try_update` found the cell empty
      def empty_cell?
        @status == :empty
      end
      alias_method :missed?, :empty_cell?

      # The value the cell holds after the operation. On failure this is the
      # restored original reference; it is `nil` when nothing readable was
      # available (`:timeout` and `:empty`).
      #
      # @return [Object, nil]
      def current_value
        @status == :timeout || @status == :empty ? nil : (updated? ? @value : @previous_value)
      end

      alias_method :new_value, :value
      alias_method :reason, :error

      def to_s
        "#<#{self.class.name} status=#{@status.inspect} previous_value=#{@previous_value.inspect} " \
          "value=#{@value.inspect}#{failed? ? " error=#{@error.class}: #{@error.message}" : ''}>"
      end
      alias_method :inspect, :to_s
    end

    # Result used when the update timed out before it was committed.
    TIMEOUT_RESULT = UpdateResult.new(:timeout).freeze

    # Result used when a non-blocking update found the cell empty.
    EMPTY_RESULT = UpdateResult.new(:empty).freeze

    private_constant :TIMEOUT_RESULT, :EMPTY_RESULT

    # Per-thread stack of the `MVar` blocks currently running, used to reject
    # nested access from the same thread.
    OWNERSHIP_STACK_KEY = :__concurrent_mvar_ownership_stack__
    private_constant :OWNERSHIP_STACK_KEY

    # Internal subclass so the rejecting methods can tell their own assertion
    # apart from an error the user block raised. Publicly it is a
    # {Concurrent::ReentrantOperationError}.
    Rejection = Class.new(Concurrent::ReentrantOperationError)
    private_constant :Rejection

    # Create a new `MVar`, either empty or with an initial value.
    #
    # @param [Hash] opts the options controlling how the future will be processed
    #
    # @!macro deref_options
    def initialize(value = EMPTY, opts = {})
      @value = value
      @mutex = Mutex.new
      @empty_condition = ConditionVariable.new
      @full_condition = ConditionVariable.new
      set_deref_options(opts)
    end

    # Remove the value from an `MVar`, leaving it empty, and blocking if there
    # isn't a value. A timeout can be set to limit the time spent waiting for
    # the cell to become readable, in which case it returns `TIMEOUT` if the
    # time is exceeded.
    #
    # @return [Object] the value that was taken, or `TIMEOUT`
    # @raise [Concurrent::ReentrantOperationError] when called from a block run
    #   by an operation on this same instance from this thread
    def take(timeout = nil)
      deadline = deadline_for(timeout)
      ensure_not_reentrant!(:take)

      @mutex.synchronize do
        wait_for_full(deadline)

        # If we timed out we'll still be empty
        if unlocked_full?
          value = @value
          commit(EMPTY) # single transition: full -> empty
          ns_deref(value)
        else
          TIMEOUT
        end
      end
    end

    # Acquires the lock on an `MVar`, yields the value to the provided block,
    # and releases the lock. The cell is not modified by `#borrow`; the block
    # returning or raising leaves the value in place. A timeout can be set to
    # limit the time spent waiting for the cell to become readable, in which
    # case it returns `TIMEOUT` if the time is exceeded.
    #
    # @return [Object] the value returned by the block, or `TIMEOUT`
    # @raise [Concurrent::ReentrantOperationError] when called from the block
    #   itself
    def borrow(timeout = nil)
      deadline = deadline_for(timeout)
      ensure_not_reentrant!(:borrow)

      @mutex.synchronize do
        wait_for_full(deadline)

        # If we timed out we'll still be empty
        if unlocked_full?
          value = @value
          ns_own(:borrow) { yield value }
        else
          TIMEOUT
        end
      end
    end

    # Put a value into an `MVar`, blocking if there is already a value until
    # it is empty. A timeout can be set to limit the time spent waiting for the
    # cell to become free, in which case it returns `TIMEOUT` if the time is
    # exceeded.
    #
    # @return [Object] the value that was put, or `TIMEOUT`
    # @raise [Concurrent::ReentrantOperationError] when called from a block run
    #   by an operation on this same instance from this thread
    def put(value, timeout = nil)
      deadline = deadline_for(timeout)
      ensure_not_reentrant!(:put)

      @mutex.synchronize do
        wait_for_empty(deadline)

        # If we timed out we won't be empty
        if unlocked_empty?
          commit(value) # single transition: empty -> full
          ns_deref(value)
        else
          TIMEOUT
        end
      end
    end

    # Atomically `take`, yield the value to a block for transformation, and then
    # `put` the transformed value. Returns the pre-transform value.
    #
    # The cell is not emptied while the block runs: the block result is
    # installed in one step at the commit boundary, so concurrent readers only
    # see the value before or after the transformation. If the block raises,
    # the exception propagates and the cell keeps the exact reference it had
    # before the block ran - it is never left empty and the reference is not
    # replaced by a copy.
    #
    # A timeout can be set to limit the time spent waiting for the cell to
    # become readable, in which case it returns `TIMEOUT` if the time is
    # exceeded. The timeout does not apply to the block execution.
    #
    # @return [Object] the pre-transform value, or `TIMEOUT`
    # @raise [Concurrent::ReentrantOperationError] when called from the block
    #   itself
    def modify(timeout = nil)
      raise ArgumentError.new('no block given') unless block_given?

      deadline = deadline_for(timeout)
      ensure_not_reentrant!(:modify)

      @mutex.synchronize do
        wait_for_full(deadline)

        # If we timed out we'll still be empty
        if unlocked_full?
          previous = @value
          # The block raises before the commit: the cell still holds `previous`.
          ns_own(:modify) do
            commit(yield previous)
            ns_deref(previous)
          end
        else
          TIMEOUT
        end
      end
    end

    # Atomically transform the value and report the outcome explicitly instead
    # of raising. Blocks like `#modify`, with the same commit boundary and the
    # same failure behaviour: when the block raises nothing is committed and
    # the cell still holds `result.previous_value`.
    #
    # @param [Numeric, nil] timeout limits waiting for the cell to become
    #   readable; it does not limit the block execution
    # @return [MVar::UpdateResult] status `:updated`, `:failed` or `:timeout`
    # @raise [Concurrent::ReentrantOperationError] when called from the block
    #   itself; a rejected update is never reported as a result
    #
    # @example
    #   result = config.update { |cfg| cfg.with(timeout: 5) }
    #   if result.failed?
    #     log(result.error) # the cell still holds result.previous_value
    #   end
    def update(timeout = nil)
      raise ArgumentError.new('no block given') unless block_given?

      deadline = deadline_for(timeout)
      ensure_not_reentrant!(:update)

      @mutex.synchronize do
        wait_for_full(deadline)

        if unlocked_full?
          ns_run_update(:update) { |previous| yield previous }
        else
          TIMEOUT_RESULT
        end
      end
    end

    # Non-blocking version of `#update`. When the cell is empty the block does
    # not run and a result with the `:empty` status is returned, which is
    # distinguishable from `:timeout` and from a value.
    #
    # @return [MVar::UpdateResult] status `:updated`, `:failed` or `:empty`
    # @raise [Concurrent::ReentrantOperationError] when called from the block
    #   itself
    def try_update
      raise ArgumentError.new('no block given') unless block_given?

      ensure_not_reentrant!(:try_update)

      @mutex.synchronize do
        if unlocked_full?
          ns_run_update(:try_update) { |previous| yield previous }
        else
          EMPTY_RESULT
        end
      end
    end

    # Non-blocking version of `take`, that returns `EMPTY` instead of blocking.
    # `EMPTY` is the sentinel for an empty cell and `TIMEOUT` for an expired
    # wait, so the result distinguishes "there was nothing to take" from a
    # stored value, from a timeout, and from `#try_put!`'s `true`/`false`.
    def try_take!
      ensure_not_reentrant!(:try_take!)

      @mutex.synchronize do
        if unlocked_full?
          value = @value
          commit(EMPTY)
          ns_deref(value)
        else
          EMPTY
        end
      end
    end

    # Non-blocking version of `put`, that returns whether or not it was successful.
    def try_put!(value)
      ensure_not_reentrant!(:try_put!)

      @mutex.synchronize do
        if unlocked_empty?
          commit(value)
          true
        else
          false
        end
      end
    end

    # Non-blocking version of `put` that will overwrite an existing value.
    def set!(value)
      ensure_not_reentrant!(:set!)

      @mutex.synchronize do
        old_value = @value
        commit(value)
        ns_deref(old_value)
      end
    end

    # Non-blocking version of `modify` that will yield with `EMPTY` if there is
    # no value yet. If the block raises the exception propagates and the cell
    # keeps the exact reference it had before the block ran.
    def modify!
      raise ArgumentError.new('no block given') unless block_given?

      ensure_not_reentrant!(:modify!)

      @mutex.synchronize do
        previous = @value
        # The block raises before the commit: the cell still holds `previous`.
        ns_own(:modify!) do
          commit(yield previous)
          ns_deref(previous)
        end
      end
    end

    # Returns the value without removing it, or `MVar::EMPTY` when the cell is
    # empty. This is a pure read: it never changes the cell state and never
    # waits. The `MVar::EMPTY` sentinel is returned as-is, so an empty cell is
    # always recognizable by identity regardless of the dereference options.
    #
    # @return [Object] the value or `MVar::EMPTY`
    # @raise [Concurrent::ReentrantOperationError] when called from a block run
    #   by an operation on this same instance from this thread
    def value
      ensure_not_reentrant!(:value)

      @mutex.synchronize do
        unlocked_empty? ? EMPTY : ns_deref(@value)
      end
    end
    alias_method :deref, :value

    # Returns if the `MVar` is currently empty.
    def empty?
      ensure_not_reentrant!(:empty?)

      @mutex.synchronize { unlocked_empty? }
    end

    # Returns if the `MVar` currently contains a value.
    def full?
      ensure_not_reentrant!(:full?)

      @mutex.synchronize { unlocked_full? }
    end

    protected

    def synchronize(&block)
      ensure_not_reentrant!
      @mutex.synchronize(&block)
    end

    private

    # Marks this `MVar` as in use by the current thread for the duration of the
    # block, so nested access from the same thread is rejected instead of
    # blocking on the lock the thread already holds. The cell itself is not
    # touched here; the caller commits the value the block returns.
    def ns_own(operation)
      thread = Thread.current
      stack  = thread.thread_variable_get(OWNERSHIP_STACK_KEY)

      unless stack
        stack = []
        thread.thread_variable_set(OWNERSHIP_STACK_KEY, stack)
      end

      stack.push([self, operation])
      begin
        yield
      ensure
        stack.pop
        thread.thread_variable_set(OWNERSHIP_STACK_KEY, nil) if stack.empty?
      end
    end

    # @return [Symbol, nil] the operation of this instance currently running in
    #   this thread, if any
    def ns_owned_operation
      stack = Thread.current.thread_variable_get(OWNERSHIP_STACK_KEY)
      return nil if stack.nil? || stack.empty?

      index = stack.rindex { |entry| entry[0].equal?(self) }
      index && stack[index][1]
    end

    # Common driver for {#update} and {#try_update}. Must be called with the
    # lock held and the cell full.
    def ns_run_update(operation)
      previous = @value
      begin
        # The block receives the original reference, it is never handed a copy
        # that could be published in a half-modified state.
        ns_own(operation) do
          value = yield previous
          commit(value)
          UpdateResult.new(:updated, previous, value)
        end
      rescue StandardError => error
        raise if error.is_a?(Rejection)

        # Failure is before the commit boundary: the cell still holds the exact
        # reference the block was given, report why the update did not commit.
        UpdateResult.new(:failed, previous, nil, error)
      rescue Exception
        # Interrupts and exits are not reported as a result; the cell was never
        # touched so it already holds the original reference.
        raise
      end
    end

    # Applies the dereference options (copy/dup/freeze callbacks) while the
    # current thread owns the cell, so a callback touching this `MVar` is
    # rejected with {Concurrent::ReentrantOperationError} instead of failing on
    # the lock already held. The sentinels are returned as-is, they are the
    # contract for "empty" and "timed out" and must stay recognizable by
    # identity. Must be called with the lock held.
    def ns_deref(value)
      return value if value.nil? || value.equal?(EMPTY) || value.equal?(TIMEOUT)

      ns_own(:deref_options) { apply_deref_options(value) }
    end

    # Single commit point: installs the new reference and wakes the waiters of
    # the state the cell just reached. Called with the lock held.
    def commit(value)
      @value = value
      if unlocked_empty?
        @empty_condition.signal
      else
        @full_condition.signal
      end
      value
    end

    def ensure_not_reentrant!(operation = nil)
      running = ns_owned_operation
      return if running.nil?

      message = if operation
                  "MVar##{operation} was called from the block of MVar##{running} " \
                    'on the same instance; re-entrant access is not supported'
                else
                  "MVar is already being updated by MVar##{running} in this thread; " \
                    're-entrant access is not supported'
                end
      raise Rejection, message
    end

    # @return [Numeric, nil] the absolute time at which waiting must stop, or
    #   `nil` for an unbounded wait. A non-positive timeout means "do not wait",
    #   matching the historical behaviour of the timed variants.
    def deadline_for(timeout)
      return nil if timeout.nil?

      unless timeout.is_a?(Numeric)
        raise ArgumentError, "timeout must be a number or nil, got #{timeout.inspect}"
      end

      Concurrent.monotonic_time + timeout
    end

    def unlocked_empty?
      EMPTY.equal?(@value)
    end

    def unlocked_full?
      ! unlocked_empty?
    end

    def wait_for_full(deadline)
      wait_while(@full_condition, deadline) { unlocked_empty? }
    end

    def wait_for_empty(deadline)
      wait_while(@empty_condition, deadline) { unlocked_full? }
    end

    def wait_while(condition, deadline)
      if deadline.nil?
        while yield
          condition.wait(@mutex)
        end
      else
        while yield
          remaining = deadline - Concurrent.monotonic_time
          break unless remaining > 0.0
          condition.wait(@mutex, remaining)
        end
      end
    end
  end
end
