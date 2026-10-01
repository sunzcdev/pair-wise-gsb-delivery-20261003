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
  # Finally, the class method {MVar.select} waits on several `MVar`s at once
  # and atomically takes the value from exactly one of them, without polling
  # and without starting a thread per `MVar`.
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

    # Unique internal value that represents that a {Selector} has not claimed
    # any slot yet. Never stored in an `MVar`.
    # @!visibility private
    NOT_CLAIMED = ::Object.new

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
      # Entries are [selector, slot_index], only present while the MVar is
      # empty. Protected by @mutex.
      @select_waiters = []
      set_deref_options(opts)
    end

    # Waits for at least one of the given `MVar`s to become full and atomically
    # takes the value from exactly one of them, without polling and without
    # starting a thread per `MVar`.
    #
    # The winning slot is determined deterministically:
    #
    # * if several `MVar`s are already full when called, the earliest one in
    #   the argument order wins,
    # * if several become full concurrently, the one on which a `#put`
    #   (or `#try_put!`, `#set!`, `#modify`, `#modify!`) commits first wins,
    # * between a waiting selection and ordinary `#take` calls blocked on the
    #   same empty `MVar`, registered selections are offered the new value
    #   first; an ordinary `#take` only receives the value if no selection
    #   takes it.
    #
    # Exactly one value is ever consumed by one call. The non-winning full
    # `MVar`s keep their values and can be taken by later ordinary `#take`
    # calls or selections.
    #
    # @param [MVar] mvars the `MVar`s to select from
    # @param [Numeric, nil] timeout seconds to wait at most, `nil` waits
    #   forever
    # @return [Array(Integer, Object)] a two element array `[index, value]`
    #   with the zero based index of the winning slot (its position in the
    #   `mvars` arguments) and the taken value
    # @return [TIMEOUT] if no `MVar` became full within `timeout`
    #
    # @raise [ArgumentError] if no `MVar` is given or an argument is not an
    #   `MVar`
    #
    # @example
    #   index, value = Concurrent::MVar.select(a, b, c)
    def self.select(*mvars, timeout: nil)
      if mvars.empty?
        raise ArgumentError.new('at least one MVar has to be given')
      end
      mvars.each_with_index do |mvar, index|
        unless mvar.is_a? MVar
          raise ArgumentError.new("argument at position #{index} is not an MVar")
        end
      end

      Selector.new(mvars, timeout).select
    end

    # Alias for {MVar.select}.
    # @see .select
    def self.take_any(*mvars, timeout: nil)
      select(*mvars, timeout: timeout)
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
          unlocked_store_and_offer!(value)
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
          new_value = yield value
          @value = new_value
          unlocked_offer_or_signal_full!
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
          unlocked_store_and_offer!(value)
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
        unlocked_offer_or_signal_full!
        apply_deref_options(old_value)
      end
    end

    # Non-blocking version of `modify` that will yield with `EMPTY` if there is no value yet.
    def modify!
      raise ArgumentError.new('no block given') unless block_given?

      @mutex.synchronize do
        value = @value
        new_value = yield value
        @value = new_value
        if unlocked_empty?
          @empty_condition.signal
        else
          unlocked_offer_or_signal_full!
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

    # Registers a pending {MVar.select} waiter on this (known to be empty)
    # `MVar`. Returns `true` if the registration succeeded, `false` if the
    # selector had already won elsewhere or timed out. Only meant for internal
    # use by {Selector}.
    #
    # @!visibility private
    def register_selector(selector, slot_index)
      @mutex.synchronize do
        if unlocked_full?
          if selector.__claim__(slot_index, @value)
            @value = EMPTY
            @empty_condition.signal
            true
          else
            false
          end
        elsif selector.__closed__?
          false
        else
          @select_waiters << [selector, slot_index]
          true
        end
      end
    end

    # Removes a previously registered pending selection. Only meant for
    # internal use by {Selector}.
    #
    # @!visibility private
    def unregister_selector(selector)
      @mutex.synchronize do
        @select_waiters.delete_if { |entry| entry.first.equal?(selector) }
      end
    end

    # Performs a {MVar.select}. It is registered on every participating empty
    # `MVar`, and the first `MVar` that offers it a value wins. The offer is
    # accepted or rejected under the selector's own lock, so exactly one
    # `MVar` can ever win.
    #
    # The only lock order ever taken is `MVar#@mutex` then `Selector#@mutex`,
    # which prevents deadlocks; the selector lock is never held while calling
    # into an `MVar`.
    #
    # @!visibility private
    class Selector
      def initialize(mvars, timeout)
        @mvars = mvars
        @timeout = timeout
        @mutex = Mutex.new
        @condition = ConditionVariable.new
        # Integer index of the winning slot, `:timeout` on timeout, or
        # NOT_CLAIMED while still waiting.
        @won_index = NOT_CLAIMED
        @won_value = nil
        # The MVars on which this selection is currently registered. Only
        # mutated while holding @mutex.
        @registered = []
      end

      def select
        stop = Concurrent.monotonic_time + @timeout unless @timeout.nil?

        # Register on the slots in argument order. A slot that is already full
        # is atomically claimed by register_selector; the first such slot (or
        # the first later offer) deterministically wins.
        @mvars.each_with_index do |mvar, index|
          registered = mvar.register_selector(self, index)

          won = @mutex.synchronize do
            @registered << mvar if registered
            @won_index != NOT_CLAIMED
          end

          return finish(won) if won || !registered
        end

        timed_out = false
        @mutex.synchronize do
          loop do
            break unless @won_index == NOT_CLAIMED

            if stop
              remaining = stop - Concurrent.monotonic_time
              if remaining <= 0.0
                # Closing before unregistering makes a put that commits at the
                # same instant deterministic: it either won before we closed or
                # its offer is rejected and the value stays in its MVar.
                @won_index = :timeout
                timed_out = true
                break
              end
              @condition.wait(@mutex, remaining)
              # A spurious wake-up (or a broadcast on the condition) simply
              # loops back: re-check the claim and the remaining time.
            else
              @condition.wait(@mutex)
            end
          end
        end

        finish(!timed_out)
      end

      # Called by an `MVar` that has a value available. Atomically claims the
      # selection if it has not been claimed yet. Returns whether this `MVar`
      # won. Always called while holding the MVar's mutex, with the MVar full.
      def __accept_offer__(slot_index, value)
        @mutex.synchronize do
          return false unless @won_index == NOT_CLAIMED
          @won_index = slot_index
          @won_value = value
          @condition.signal
          true
        end
      end
      alias __claim__ __accept_offer__

      # Whether the selection has already won elsewhere or timed out.
      def __closed__?
        @mutex.synchronize { @won_index != NOT_CLAIMED }
      end

      private

      # Removes every pending registration (idempotent: the winning slot's
      # entry has already been consumed by its MVar) and returns the result.
      # Never called while holding @mutex, so the MVar-then-selector lock
      # order is preserved.
      def finish(success)
        @registered.each { |mvar| mvar.unregister_selector(self) }

        return TIMEOUT unless success

        index = nil
        value = nil
        @mutex.synchronize do
          index = @won_index
          value = @won_value
        end
        [index, @mvars[index].send(:apply_deref_options, value)]
      end
    end

    protected

    def synchronize(&block)
      @mutex.synchronize(&block)
    end

    private

    def unlocked_empty?
      @value == EMPTY
    end

    def unlocked_full?
      ! unlocked_empty?
    end

    # Stores `value` (the MVar is known to be empty) and offers it to waiting
    # selectors first. A selector that accepts the offer consumes the value,
    # otherwise ordinary blocked `#take` callers are signaled. Must be called
    # while holding `@mutex`.
    def unlocked_store_and_offer!(value)
      @value = value
      return if unlocked_offer_to_selectors!
      @full_condition.signal
    end

    # Offers the current value to registered selectors first; if none accepts
    # it, signals ordinary blocked `#take` callers. Used when the MVar may
    # already be full (e.g. after `#set!` or `#modify!`). Must be called while
    # holding `@mutex`.
    def unlocked_offer_or_signal_full!
      return if unlocked_offer_to_selectors!
      @full_condition.signal
    end

    # Offers the current value to registered selectors in registration order.
    # Returns `true` if one of them consumed it (leaving the MVar empty),
    # `false` otherwise. Must be called while holding `@mutex`.
    def unlocked_offer_to_selectors!
      waiters = @select_waiters
      until waiters.empty?
        selector, slot_index = waiters.shift
        if selector.__accept_offer__(slot_index, @value)
          @value = EMPTY
          # A selector consumed the value immediately, so a blocked `#put` may
          # now proceed.
          @empty_condition.signal
          return true
        end
      end
      false
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
