require 'concurrent/synchronization/lockable_object'
require 'concurrent/utility/native_integer'

module Concurrent

  # @!macro semaphore
  # @!visibility private
  # @!macro internal_implementation_note
  class MutexSemaphore < Synchronization::LockableObject

    # A single request waiting in the fair-mode FIFO queue.
    #
    # Each request owns a dedicated condition variable, so notifications go
    # to exactly one waiter. State machine:
    #
    #   :waiting -> :granted  (permits reserved for it, owner may proceed)
    #   :waiting -> :cancelled (timeout or the thread was killed/raised)
    #
    # `blocked`/`must_wake` close the predicate-check/wait gap: a signal
    # delivered after the owner observed the old state but before it blocked
    # on the condition would otherwise be lost (Ruby's ConditionVariable
    # signal is a no-op with nobody blocked).
    #
    # @!visibility private
    FairRequest = ::Struct.new(:permits, :state, :condition, :blocked,
                               :must_wake) do
      # @!visibility private
      def granted?
        state == :granted
      end

      # @!visibility private
      def signal
        if blocked
          condition.signal
        else
          self.must_wake = true
        end
      end
    end
    private_constant :FairRequest

    # @!macro semaphore_method_initialize
    #
    # @param [Boolean, Hash] fair when truthy, or `{ fair: true }`, the
    #   semaphore is created in fair mode: requests are granted in FIFO order,
    #   the whole requested number of permits is granted atomically (no
    #   partial acquisition), and no request (including {#try_acquire}) can
    #   overtake the head of the queue. Defaults to `false` which preserves the
    #   original, non-fair behavior.
    def initialize(count, fair = false)
      Utility::NativeInteger.ensure_integer_and_bounds count
      fair = fair.fetch(:fair, false) if fair.is_a?(::Hash)

      super()
      synchronize { ns_initialize count, fair }
    end

    # Indicates whether this semaphore was created in fair mode.
    #
    # @return [Boolean]
    def fair?
      @fair ? @fair_lock.synchronize { @fair } : synchronize { @fair }
    end

    # @!visibility private
    # Test instrumentation: [free permits, granted-but-not-yet-released
    # permits]. Invariant: free + granted == initial count whenever only
    # balanced acquire/release calls are used.
    def permit_accounting
      @fair_lock.synchronize { [@free, @granted] }
    end

    # @!macro semaphore_method_acquire
    def acquire(permits = 1)
      Utility::NativeInteger.ensure_integer_and_bounds permits
      Utility::NativeInteger.ensure_positive permits

      if @fair
        fair_wait(permits, nil)
      else
        synchronize do
          try_acquire_timed(permits, nil)
        end
      end

      return unless block_given?

      begin
        yield
      ensure
        release(permits)
      end
    end

    # @!macro semaphore_method_available_permits
    def available_permits
      if @fair
        @fair_lock.synchronize { @free }
      else
        synchronize { @free }
      end
    end

    # @!macro semaphore_method_drain_permits
    #
    #   Acquires and returns all permits that are immediately available.
    #
    #   In fair mode this never overtakes a queued request: it returns `0`
    #   whenever the FIFO queue is non-empty (the head waiter owns the claim
    #   on any permits it can use).
    #
    #   @return [Integer]
    def drain_permits
      if @fair
        @fair_lock.synchronize do
          @waiters.empty? ? @free.tap { |_| @free = 0 } : 0
        end
      else
        synchronize do
          @free.tap { |_| @free = 0 }
        end
      end
    end

    # @!macro semaphore_method_try_acquire
    def try_acquire(permits = 1, timeout = nil)
      Utility::NativeInteger.ensure_integer_and_bounds permits
      Utility::NativeInteger.ensure_positive permits

      acquired = if @fair
                   # Even a non-blocking attempt must not overtake an earlier
                   # request.
                   fair_wait(permits, timeout || 0)
                 else
                   synchronize do
                     if timeout.nil?
                       try_acquire_now(permits)
                     else
                       try_acquire_timed(permits, timeout)
                     end
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

      if @fair
        @fair_lock.synchronize do
          @free += permits
          @granted -= permits
          # Grant at most the one head; its woken owner cascades the rest.
          wake_eligible_head
          # A previous head may already be granted but not yet removed by its
          # (just-scheduled) owner; if this release added enough permits for
          # the successor, that owner will cascade itself, so no further
          # granting is done here.
        end
      else
        synchronize do
          @free += permits
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
    # @return [nil]
    #
    # @!visibility private
    def reduce_permits(reduction)
      Utility::NativeInteger.ensure_integer_and_bounds reduction
      Utility::NativeInteger.ensure_positive reduction

      if @fair
        @fair_lock.synchronize { @free -= reduction }
      else
        synchronize { @free -= reduction }
      end
      nil
    end

    protected

    # @!visibility private
    def ns_initialize(count, fair = false)
      @free      = count
      @fair      = fair
      # Dedicated lock for the fair-mode FIFO queue; independent from the
      # lock used by the non-fair waiting machinery.
      @fair_lock = ::Mutex.new
      # FIFO queue of FairRequest instances, used only in fair mode
      @waiters   = []
      # Test instrumentation: permits granted but not released back yet
      @granted   = 0
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

    # Reserves permits for the single head request if its full demand fits,
    # marks it `:granted` and signals its owner. Permits which do not fit the
    # head stay completely untouched (no partial acquisition).
    #
    # If the head is already `:granted` (its owner is scheduled to wake but
    # has not removed itself yet) nothing is granted: the in-flight owner is
    # responsible for the cascade. This is the rule that keeps granting
    # strictly FIFO even when woken threads are scheduled out of order.
    #
    # Exactly one request is granted per external event (release, timeout,
    # thread death). The woken head thread performs the cascade through the
    # rest of the queue itself in {#fair_wait}, which guarantees strict FIFO
    # processing: a later request can never be granted ahead of an earlier
    # one that is still waking up.
    #
    # Must be called while holding `@fair_lock`.
    #
    # @!visibility private
    def wake_eligible_head
      request = @waiters.first
      return if request.nil?
      return unless request.state == :waiting
      return if @free < request.permits

      @free -= request.permits
      @granted += request.permits
      request.state = :granted
      request.signal
    end

    # Enqueues a request at the tail of the FIFO queue and blocks until it is
    # granted as a whole or until `timeout` seconds elapse.
    #
    # @param [Integer] permits number of permits, always positive
    # @param [Numeric, nil] timeout seconds to wait; `nil` waits forever,
    #   `0` performs a single non-blocking attempt
    # @return [Boolean] `true` if the permits were granted, `false` on timeout
    #
    # @!visibility private
    def fair_wait(permits, timeout)
      request = FairRequest.new(permits, :waiting, ::ConditionVariable.new,
                                false, false)
      deadline = timeout.nil? ? nil : Concurrent.monotonic_time + timeout

      @fair_lock.synchronize do
        @waiters.push(request)
        wake_eligible_head

        begin
          until request.granted?
            remaining = deadline.nil? ? nil : deadline - Concurrent.monotonic_time
            if remaining && remaining <= 0
              return false
            end

            # Sticky notification left while this thread was not yet blocked.
            next if request.must_wake

            request.blocked = true
            request.condition.wait(@fair_lock, remaining)
            request.blocked = false
            # The state is re-checked at the top of the loop.
          end
          true
        ensure
          request.blocked = false

          # Exactly one queue exit.
          if request.granted?
            # Take this granted request out of the queue first: it is the
            # head and no other granted requests can precede it (granting is
            # strictly one-head-at-a-time). Then cascade while still holding
            # the lock, granting every following request that fits and
            # signalling each. Its owner is necessarily blocked here because
            # this thread owns the lock, so no signal can be lost and no
            # later request can overtake an earlier one.
            request.must_wake = false
            @waiters.delete(request)
            cascade_grants
          else
            # Timed out, killed or raised while waiting.
            cancel_fair_request(request)
          end
        end
      end
    end

    # Grants as many queued head requests as the currently free permits
    # permit, atomically and strictly in queue order. Runs on the woken head
    # thread while it holds `@fair_lock`, so every signal reaches a blocked
    # owner and strict FIFO order is preserved.
    #
    # Must be called while holding `@fair_lock`.
    #
    # @!visibility private
    def cascade_grants
      while (following = @waiters.first)
        # A granted head still in the queue means its owner is scheduled but
        # has not run its own cascade yet; stop and let it drive the queue.
        break unless following.state == :waiting
        break if @free < following.permits

        @free -= following.permits
        @granted += following.permits
        following.state = :granted
        following.signal
      end
    end

    # Removes a timed-out or abandoned request from the queue and, if it was
    # the head, grants its successor when enough permits are available.
    # Idempotent: does nothing when the request is already granted or no
    # longer queued.
    #
    # Must be called while holding `@fair_lock`.
    #
    # @!visibility private
    def cancel_fair_request(request)
      return if request.granted?
      was_head = @waiters.first.equal?(request)
      request.state = :cancelled
      @waiters.delete(request)
      # Only the head leaving can change which requests are eligible.
      wake_eligible_head if was_head
    end
  end
end
