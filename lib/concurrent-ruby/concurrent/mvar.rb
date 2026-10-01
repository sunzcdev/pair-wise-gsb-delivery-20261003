require 'concurrent/concern/dereferenceable'
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

    # @!visibility private
    #
    # Book-keeping for one in-flight `MVar.select` call. A waiter is
    # registered on every MVar taking part in the select; the first MVar
    # that becomes full *claims* the waiter, but the value itself stays in
    # the MVar until the selecting thread takes it. This two-phase scheme
    # (claim, then consume) guarantees that a select consumes exactly one
    # slot, that a racing `#take` can never leave a select stuck with a
    # half-consumed value, and that a timed-out select leaves nothing
    # behind that could steal future values.
    class SelectWaiter
      def initialize
        @mutex     = Mutex.new
        @condition = ConditionVariable.new
        @ready     = nil # the MVar that claimed this waiter, if any
      end

      # Called by an MVar that just became full, while that MVar holds its
      # own lock. The first claiming MVar wins; later claims are ignored.
      def claim(mvar)
        @mutex.synchronize do
          @ready ||= mvar
          @condition.signal
        end
      end

      # Waits until some MVar claims this waiter or the deadline passes.
      # Returns the claiming MVar (and clears the claim), or nil on
      # timeout. The waiter's mutex is never held by the caller afterwards,
      # so MVar locks may be acquired without risking lock-order inversion.
      def wait(deadline)
        @mutex.synchronize do
          loop do
            if @ready
              ready  = @ready
              @ready = nil
              return ready
            end
            if deadline
              remaining = deadline - Concurrent.monotonic_time
              return nil if remaining <= 0.0
              @condition.wait(@mutex, remaining)
            else
              @condition.wait(@mutex)
            end
          end
        end
      end
    end

    # Blocks until any one of the given `MVar`s becomes full, removes the
    # value from exactly that one slot, and returns the winning slot's
    # index (position in the argument list) together with the value.
    #
    # Only the winning slot is consumed: every other `MVar` keeps its
    # value, which can still be retrieved with a normal `#take`. If
    # several slots are full at the same time the earliest one in argument
    # order wins; if several `#put`s race a blocked select, the first
    # `put` to claim the select wins. In both cases there is exactly one
    # winner. A select also competes fairly with ordinary `#take` calls:
    # whichever acquires the value first gets it, the other keeps waiting.
    #
    # A `timeout` (in seconds) can be given to limit the time spent
    # blocked, in which case `TIMEOUT` is returned if the time is
    # exceeded. After a timeout no slot is consumed and no waiter
    # registration is left behind, so values put later are never stolen
    # by a dead select.
    #
    # @param [Array<MVar>] mvars the slots to select over
    # @param [Numeric, nil] timeout maximum time to block, or nil to block forever
    # @return [Array(Integer, Object)] the winning slot index and its value,
    #   or `TIMEOUT`
    def self.select(*mvars, timeout: nil)
      mvars = mvars.flatten
      raise ArgumentError, 'at least one MVar is required' if mvars.empty?

      waiter   = SelectWaiter.new
      deadline = timeout && Concurrent.monotonic_time + timeout

      # Register on every MVar first, so no #put can slip through between
      # the scan below and the wait.
      mvars.each { |mvar| mvar.send(:register_select_waiter, waiter) }

      begin
        loop do
          # Consume any full slot; the first full slot in argument order
          # wins, giving deterministic single-winner semantics.
          mvars.each_with_index do |mvar, index|
            value = mvar.try_take!
            return [index, value] unless value == EMPTY
          end

          # Sleep until some slot claims the waiter. The claim is only a
          # wakeup: the value is consumed by the scan above, so a racing
          # #take stealing it just sends us around the loop again.
          return TIMEOUT unless waiter.wait(deadline)
        end
      ensure
        # Whatever happened - win, timeout or exception - leave no
        # registration behind that could observe (or steal) future values.
        mvars.each { |mvar| mvar.send(:deregister_select_waiter, waiter) }
      end
    end

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
      @select_waiters = []
      set_deref_options(opts)
    end

    # Remove the value from an `MVar`, leaving it empty, and blocking if there
    # isn't a value. A timeout can be set to limit the time spent blocked, in
    # which case it returns `TIMEOUT` if the time is exceeded.
    # @return [Object] the value that was taken, or `TIMEOUT`
    def take(timeout = nil)
      @mutex.synchronize do
        wait_for_full(timeout)

        # If we timed out we'll still be empty
        if unlocked_full?
          value = @value
          @value = EMPTY
          @empty_condition.signal
          apply_deref_options(value)
        else
          TIMEOUT
        end
      end
    end

    # acquires lock on the from an `MVAR`, yields the value to provided block,
    # and release lock. A timeout can be set to limit the time spent blocked,
    # in which case it returns `TIMEOUT` if the time is exceeded.
    # @return [Object] the value returned by the block, or `TIMEOUT`
    def borrow(timeout = nil)
      @mutex.synchronize do
        wait_for_full(timeout)

        # If we timed out we'll still be empty
        if unlocked_full?
          yield @value
        else
          TIMEOUT
        end
      end
    end

    # Put a value into an `MVar`, blocking if there is already a value until
    # it is empty. A timeout can be set to limit the time spent blocked, in
    # which case it returns `TIMEOUT` if the time is exceeded.
    # @return [Object] the value that was put, or `TIMEOUT`
    def put(value, timeout = nil)
      @mutex.synchronize do
        wait_for_empty(timeout)

        # If we timed out we won't be empty
        if unlocked_empty?
          @value = value
          unlocked_notify_full
          apply_deref_options(value)
        else
          TIMEOUT
        end
      end
    end

    # Atomically `take`, yield the value to a block for transformation, and then
    # `put` the transformed value. Returns the pre-transform value. A timeout can
    # be set to limit the time spent blocked, in which case it returns `TIMEOUT`
    # if the time is exceeded.
    # @return [Object] the pre-transform value, or `TIMEOUT`
    def modify(timeout = nil)
      raise ArgumentError.new('no block given') unless block_given?

      @mutex.synchronize do
        wait_for_full(timeout)

        # If we timed out we'll still be empty
        if unlocked_full?
          value = @value
          @value = yield value
          unlocked_notify_full
          apply_deref_options(value)
        else
          TIMEOUT
        end
      end
    end

    # Non-blocking version of `take`, that returns `EMPTY` instead of blocking.
    def try_take!
      @mutex.synchronize do
        if unlocked_full?
          value = @value
          @value = EMPTY
          @empty_condition.signal
          apply_deref_options(value)
        else
          EMPTY
        end
      end
    end

    # Non-blocking version of `put`, that returns whether or not it was successful.
    def try_put!(value)
      @mutex.synchronize do
        if unlocked_empty?
          @value = value
          unlocked_notify_full
          true
        else
          false
        end
      end
    end

    # Non-blocking version of `put` that will overwrite an existing value.
    def set!(value)
      @mutex.synchronize do
        old_value = @value
        @value = value
        unlocked_notify_full
        apply_deref_options(old_value)
      end
    end

    # Non-blocking version of `modify` that will yield with `EMPTY` if there is no value yet.
    def modify!
      raise ArgumentError.new('no block given') unless block_given?

      @mutex.synchronize do
        value = @value
        @value = yield value
        if unlocked_empty?
          @empty_condition.signal
        else
          unlocked_notify_full
        end
        apply_deref_options(value)
      end
    end

    # Returns if the `MVar` is currently empty.
    def empty?
      @mutex.synchronize { @value == EMPTY }
    end

    # Returns if the `MVar` currently contains a value.
    def full?
      !empty?
    end

    protected

    def synchronize(&block)
      @mutex.synchronize(&block)
    end

    private

    # @!visibility private
    # Registers a SelectWaiter; called by MVar.select before it scans.
    def register_select_waiter(waiter)
      @mutex.synchronize { @select_waiters << waiter }
    end

    # @!visibility private
    # Removes a SelectWaiter; called by MVar.select exactly once per select.
    def deregister_select_waiter(waiter)
      @mutex.synchronize { @select_waiters.delete(waiter) }
    end

    # Must be called with @mutex held, whenever the MVar becomes full.
    # Wakes ordinary #take waiters and claims any registered select
    # waiters. Lock ordering is always MVar's lock -> waiter's lock; a
    # selecting thread never holds those locks in the opposite order.
    def unlocked_notify_full
      @full_condition.signal
      @select_waiters.each { |waiter| waiter.claim(self) } unless @select_waiters.empty?
    end

    def unlocked_empty?
      @value == EMPTY
    end

    def unlocked_full?
      ! unlocked_empty?
    end

    def wait_for_full(timeout)
      wait_while(@full_condition, timeout) { unlocked_empty? }
    end

    def wait_for_empty(timeout)
      wait_while(@empty_condition, timeout) { unlocked_full? }
    end

    def wait_while(condition, timeout)
      if timeout.nil?
        while yield
          condition.wait(@mutex)
        end
      else
        stop = Concurrent.monotonic_time + timeout
        while yield && timeout > 0.0
          condition.wait(@mutex, timeout)
          timeout = stop - Concurrent.monotonic_time
        end
      end
    end
  end
end
