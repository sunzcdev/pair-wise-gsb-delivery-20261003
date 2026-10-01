require 'concurrent/synchronization/lockable_object'
require 'concurrent/utility/native_integer'

module Concurrent

  # @!macro semaphore
  # @!visibility private
  # @!macro internal_implementation_note
  class MutexSemaphore < Synchronization::LockableObject

    # A single request queued while waiting for permits in fair mode.
    #
    # `granted` transitions from `false` to `true` exactly once: it is set by
    # the thread that hands the permits out (a releaser or a cancelling head),
    # and it is observed by the owning thread when it is woken up. This is the
    # mechanism that guarantees a request can never be granted twice.
    #
    # @!visibility private
    class FairWaiter
      attr_reader :permits
      attr_accessor :granted

      def initialize(permits)
        @permits = permits
        @granted = false
      end
    end
    private_constant :FairWaiter

    # @!macro semaphore_method_initialize
    def initialize(count, fair = false)
      Utility::NativeInteger.ensure_integer_and_bounds count
      @fair = fair ? true : false

      super()
      synchronize { ns_initialize count }
    end

    # Indicates whether this semaphore was created in fair mode.
    #
    # @return [Boolean] `true` when requests are granted strictly in FIFO order
    def fair?
      @fair
    end

    # @!macro semaphore_method_acquire
    def acquire(permits = 1)
      Utility::NativeInteger.ensure_integer_and_bounds permits
      Utility::NativeInteger.ensure_positive permits

      acquired = synchronize do
        if @fair
          ns_fair_acquire(permits, nil)
        else
          try_acquire_timed(permits, nil)
        end
      end

      return unless block_given?
      return unless acquired

      begin
        yield
      ensure
        release(permits)
      end
    end

    # @!macro semaphore_method_available_permits
    def available_permits
      synchronize { @free }
    end

    # @!macro semaphore_method_drain_permits
    #
    #   Acquires and returns all permits that are immediately available.
    #
    #   @return [Integer]
    def drain_permits
      synchronize do
        # In fair mode permits that are earmarked for queued requests must not
        # be stolen from them. Draining is therefore only allowed when nobody
        # is queued; otherwise it returns zero.
        if @fair && !@queue.empty?
          0
        else
          @free.tap { |_| @free = 0 }
        end
      end
    end

    # @!macro semaphore_method_try_acquire
    def try_acquire(permits = 1, timeout = nil)
      Utility::NativeInteger.ensure_integer_and_bounds permits
      Utility::NativeInteger.ensure_positive permits

      acquired = synchronize do
        if @fair
          if timeout.nil?
            ns_fair_try_now(permits)
          else
            ns_fair_acquire(permits, timeout)
          end
        elsif timeout.nil?
          try_acquire_now(permits)
        else
          try_acquire_timed(permits, timeout)
        end
      end

      return acquired unless block_given?
      return unless acquired

      begin
        yield
      ensure
        release(permits)
      end
    end

    # @!macro semaphore_method_release
    def release(permits = 1)
      Utility::NativeInteger.ensure_integer_and_bounds permits
      Utility::NativeInteger.ensure_positive permits

      synchronize do
        @free += permits
        if @fair
          # Hand permits out to queued requests while the head can be served
          # in one indivisible step. Permits the head cannot take whole are
          # left untouched in `@free`, so later small requests can never grab
          # them out from under a heavy head request.
          ns_fair_grant_pass
          # Wake every waiting thread; each one re-checks its own `granted`
          # flag and goes back to sleep if it lost the race. Waking everyone
          # is harmless and cannot grant a request twice.
          ns_broadcast
        else
          permits.times { ns_signal }
        end
      end
      nil
    end

    # Shrinks the number of available permits by the indicated reduction.
    #
    # @param [Fixnum] reduction Number of permits to remove.
    #
    # @raise [ArgumentError] if `reduction` is not an integer or is negative
    #
    # @raise [ArgumentError] if `@free` - `@reduction` is less than zero
    #
    # @return [nil]
    #
    # @!visibility private
    def reduce_permits(reduction)
      Utility::NativeInteger.ensure_integer_and_bounds reduction
      Utility::NativeInteger.ensure_positive reduction

      synchronize { @free -= reduction }
      nil
    end

    protected

    # @!visibility private
    def ns_initialize(count)
      @free  = count
      @queue = [] if @fair
    end

    private

    # @!visibility private
    def try_acquire_now(permits)
      if @free >= permits
        @free -= permits
        true
      else
        false
      end
    end

    # @!visibility private
    def try_acquire_timed(permits, timeout)
      ns_wait_until(timeout) { try_acquire_now(permits) }
    end

    # ------------------------------------------------------------------
    # Fair mode. Every method below runs while the object lock is held, so
    # `@free` and `@queue` are always modified together and the permit
    # accounting can never diverge or hand one request out twice.
    # ------------------------------------------------------------------

    # Non-blocking fair attempt. Succeeds only when this request would not
    # cut in front of anyone already queued and all permits are free at once.
    #
    # @!visibility private
    def ns_fair_try_now(permits)
      return false unless @queue.empty?
      try_acquire_now(permits)
    end

    # Enqueue a request and block until it reaches the head and can be
    # granted whole, or until `timeout` seconds have elapsed.
    #
    # @return [Boolean] whether the permits were acquired
    #
    # @!visibility private
    def ns_fair_acquire(permits, timeout)
      waiter = FairWaiter.new(permits)
      @queue.push(waiter)
      ns_fair_grant_pass

      deadline  = timeout ? Concurrent.monotonic_time + timeout : nil
      remaining = timeout

      loop do
        return true if waiter.granted

        if deadline
          now = Concurrent.monotonic_time
          if now >= deadline
            ns_fair_cancel(waiter)
            return false
          end
          remaining = deadline - now
        end

        # Releases the lock while waiting and reacquires it on wake-up, so the
        # state checks above and below always execute while the lock is held.
        ns_wait(remaining)

        # Woken up: either we were granted, or a release / timed-out head
        # happened that requires the queue to be reconsidered.
        next if waiter.granted

        ns_fair_grant_pass
        next if waiter.granted

        if deadline && Concurrent.monotonic_time >= deadline
          ns_fair_cancel(waiter)
          return false
        end
      end
    end

    # Remove a request that gave up waiting. If it was the head its removal
    # lets the following requests advance, so a grant pass runs immediately
    # to keep the queue moving.
    #
    # @!visibility private
    def ns_fair_cancel(waiter)
      was_head = @queue.first.equal?(waiter)
      return unless @queue.delete(waiter)

      ns_fair_grant_pass if was_head
      ns_broadcast
    end

    # Walk the queue from the head granting whole requests while permits
    # last, stopping at the first request that cannot be fully satisfied.
    #
    # @!visibility private
    def ns_fair_grant_pass
      while (waiter = @queue.first) && @free >= waiter.permits
        @free -= waiter.permits
        waiter.granted = true
        @queue.shift
      end
    end
  end
end
