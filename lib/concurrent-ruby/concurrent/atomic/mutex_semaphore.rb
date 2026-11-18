require 'thread'
require 'concurrent/synchronization/lockable_object'
require 'concurrent/utility/native_integer'
require 'concurrent/utility/monotonic_time'

module Concurrent

  # @!macro semaphore
  # @!visibility private
  # @!macro internal_implementation_note
  class MutexSemaphore < Synchronization::LockableObject

    # @!visibility private
    #
    # Admission policies understood by {#initialize}:
    #
    # - `:fifo` (default) admits queued requests strictly in arrival order, so
    #   an older large request cannot be starved by newer small ones.
    # - `:bounded_bypass` admits queued requests in arrival order, but lets a
    #   request for fewer permits than the current head of the queue overtake
    #   it at most `bypass_limit` times while that head is in place.
    QUEUE_MODES = [:fifo, :bounded_bypass].freeze

    # @!visibility private
    #
    # One queued permit request. `permits` is the batch size the caller asked
    # for, `state` is one of `:waiting`, `:granted`, `:done` or `:cancelled`,
    # and `condition` is the waiter's own condition variable. A request is
    # granted as one whole batch or not at all. Every field is guarded by the
    # semaphore's queue mutex.
    Waiter = Struct.new(:permits, :condition, :state)

    # @!visibility private
    OPTION_KEYS = [:queue_mode, :bypass_limit].freeze

    private_constant :Waiter, :OPTION_KEYS

    # @!macro semaphore_method_initialize
    def initialize(count, options = nil)
      Utility::NativeInteger.ensure_integer_and_bounds count
      mode, limit = ns_parse_options(options)

      super()

      # The queue keeps its own mutex: a waiter has to release it while it
      # sleeps, and each waiter is woken through a condition variable of its
      # own. Admission is therefore decided by the queue itself, never by the
      # order in which a single shared condition variable happens to wake
      # threads up.
      @mutex        = ::Mutex.new
      # Permits which are neither acquired nor handed to a waiter. Under the
      # bypass policy a part of it may already be earmarked for the queue head;
      # #available_permits reports the rest.
      @free         = count
      @queue        = []
      @mode         = mode
      @bypass_limit = mode == :fifo ? 0 : limit
      @bypasses     = 0
    end

    # @!macro semaphore_method_acquire
    def acquire(permits = 1)
      Utility::NativeInteger.ensure_integer_and_bounds permits
      Utility::NativeInteger.ensure_positive permits

      granted = ns_acquire(permits, nil)

      return unless block_given?

      begin
        yield
      ensure
        # Only what was actually granted is returned. A zero-permit request
        # never touches the counter, so nothing is released for it either.
        release(granted) if granted && granted > 0
      end
    end

    # @!macro semaphore_method_available_permits
    def available_permits
      @mutex.synchronize { ns_available_permits }
    end

    # @!macro semaphore_method_reserved_permits
    def reserved_permits
      @mutex.synchronize { ns_head_reservation }
    end

    # @!macro semaphore_method_waiting_count
    def waiting_count
      @mutex.synchronize { @queue.size }
    end

    alias_method :queue_length, :waiting_count

    # @!macro semaphore_method_queued_permits
    def queued_permits
      @mutex.synchronize { @queue.inject(0) { |sum, waiter| sum + waiter.permits } }
    end

    # @!macro semaphore_method_queue_mode
    def queue_mode
      @mode
    end

    # @!macro semaphore_method_bypass_limit
    def bypass_limit
      @bypass_limit
    end

    # @!macro semaphore_method_drain_permits
    #
    #   Acquires and returns all permits that are immediately available.
    #
    #   Permits earmarked for the head of the queue are not drained.
    #
    #   @return [Integer]
    def drain_permits
      @mutex.synchronize do
        if @queue.empty?
          # Nothing is promised to a waiter, so drain everything and keep the
          # historical behaviour for a count left negative by #reduce_permits.
          @free.tap { @free = 0 }
        else
          drained = ns_available_permits
          # A negative count can only come from #reduce_permits; a drain must
          # not turn into a release.
          drained = 0 if drained < 0
          @free -= drained
          drained
        end
      end
    end

    # @!macro semaphore_method_try_acquire
    def try_acquire(permits = 1, timeout = nil)
      Utility::NativeInteger.ensure_integer_and_bounds permits
      Utility::NativeInteger.ensure_positive permits
      validate_timeout timeout

      granted =
        if timeout.nil?
          @mutex.synchronize { ns_try_admit(permits) }
        else
          ns_acquire(permits, timeout)
        end

      return !granted.nil? unless block_given?
      return unless granted

      begin
        yield
      ensure
        release(granted) if granted > 0
      end
    end

    # @!macro semaphore_method_release
    def release(permits = 1)
      Utility::NativeInteger.ensure_integer_and_bounds permits
      Utility::NativeInteger.ensure_positive permits

      @mutex.synchronize do
        @free += permits
        ns_dispatch
      end
      nil
    end

    # Shrinks the number of available permits by the indicated reduction.
    #
    # This is a bookkeeping operation, not an acquisition: it does not consume
    # queued requests and it may drive the reported free count below zero.
    # Acquisition, release and draining never do that on their own.
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

      @mutex.synchronize { @free -= reduction }
      nil
    end

    private

    # Parses the optional admission-policy options. A hash is accepted either
    # as a trailing argument or as keywords; unknown keys are rejected so a
    # typo cannot silently fall back to the default policy.
    #
    # @return [Array(Symbol, Integer)] the queue mode and the bypass limit
    #
    # @!visibility private
    def ns_parse_options(options)
      options = {} if options.nil?

      unless options.is_a?(::Hash)
        raise ArgumentError.new("options must be a hash, got #{options.inspect}")
      end

      unknown = options.keys - OPTION_KEYS
      unless unknown.empty?
        raise ArgumentError.new("unknown option(s) #{unknown.inspect}, expected #{OPTION_KEYS.inspect}")
      end

      mode  = validate_queue_mode(options.fetch(:queue_mode, :fifo))
      limit = validate_bypass_limit(options.fetch(:bypass_limit, 1))
      [mode, limit]
    end

    # @!visibility private
    def validate_queue_mode(mode)
      unless QUEUE_MODES.include?(mode)
        raise ArgumentError.new("queue_mode must be one of #{QUEUE_MODES.inspect}, got #{mode.inspect}")
      end
      mode
    end

    # @!visibility private
    def validate_bypass_limit(limit)
      Utility::NativeInteger.ensure_integer_and_bounds limit
      Utility::NativeInteger.ensure_positive limit
      limit
    end

    # @!visibility private
    def validate_timeout(timeout)
      return if timeout.nil?
      unless timeout.is_a?(Numeric)
        raise ArgumentError.new("timeout must be a number or nil, got #{timeout.inspect}")
      end
      timeout
    end

    # Permits a new request could take right now. Permits earmarked for the
    # head of the queue are excluded even though they still sit in the free
    # pool, so this never advertises permits the queue has already promised.
    #
    # @!visibility private
    def ns_available_permits
      @free - ns_head_reservation
    end

    # How many free permits are, in effect, promised to the request at the head
    # of the queue. It is capped by the free pool because that is all the head
    # could receive if nothing else happened.
    #
    # @!visibility private
    def ns_head_reservation
      head = @queue.first
      return 0 unless head

      reservation = head.permits < @free ? head.permits : @free
      reservation > 0 ? reservation : 0
    end

    # Attempts to admit a request that will not queue. Admission without
    # queueing is only possible while the queue is empty; once anything is
    # waiting, the queue owns every permit and a new request either joins it
    # (blocking forms) or fails (non-blocking form). This keeps the arrival
    # order intact and stops a newcomer from slotting in ahead of a queued
    # smaller request.
    #
    # @return [Integer, nil] the granted permit count (`0` for a zero-permit
    #   request) or `nil` when the request cannot be admitted without waiting
    #
    # @!visibility private
    def ns_try_admit(permits)
      return 0 if permits.zero?
      return nil unless @queue.empty?
      return nil unless @free >= permits

      @free -= permits
      permits
    end

    # @!visibility private
    def ns_bypass_allowed?
      @mode == :bounded_bypass && @bypasses < @bypass_limit && !@queue.empty?
    end

    # Queues the request and waits for a hand-off. Returns the number of
    # permits actually granted, or `nil` when the wait timed out or was
    # cancelled.
    #
    # @!visibility private
    def ns_acquire(permits, timeout)
      @mutex.synchronize do
        granted = ns_try_admit(permits)
        return granted unless granted.nil?
        return nil if timeout && timeout <= 0

        waiter = Waiter.new(permits, ::ConditionVariable.new, :waiting)
        # A new head starts with a fresh bypass budget.
        @bypasses = 0 if @queue.empty?
        @queue << waiter
        ns_dispatch

        deadline = timeout && (Concurrent.monotonic_time + timeout)
        granted  = nil

        begin
          loop do
            # A grant is checked before the deadline so a waiter woken with
            # permits never loses them to a timeout expiring at the same time.
            if waiter.state == :granted
              waiter.state = :done
              granted      = waiter.permits
              break
            end

            remaining = deadline && (deadline - Concurrent.monotonic_time)
            break if remaining && remaining <= 0

            waiter.condition.wait(@mutex, remaining)
          end
        ensure
          # Timeouts, asynchronous exceptions and thread kills all have to
          # leave the queue and the free counter consistent.
          ns_withdraw(waiter) if granted.nil?
        end

        granted
      end
    end

    # Removes a request which will not consume its grant and returns any
    # permits already set aside for it to the free pool.
    #
    # @!visibility private
    def ns_withdraw(waiter)
      case waiter.state
      when :waiting
        ns_remove(waiter)
      when :granted
        @free += waiter.permits
        waiter.state = :cancelled
        ns_dispatch
      end
    end

    # @!visibility private
    def ns_remove(waiter)
      index = ns_queue_index(waiter)
      return unless index

      @queue.delete_at(index)
      waiter.state = :cancelled
      # The bypass budget belongs to the head of the queue; a new head starts
      # with a fresh budget.
      @bypasses = 0 if index.zero?
      ns_dispatch
    end

    # Waiters are compared by identity: two requests for the same batch size
    # are still distinct waiters and must never be confused for one another.
    #
    # @!visibility private
    def ns_queue_index(waiter)
      index = 0
      while index < @queue.size
        return index if @queue[index].equal?(waiter)
        index += 1
      end
      nil
    end

    # Hands permits to queued waiters. The head always goes first; only under
    # `:bounded_bypass`, only while the budget for this head lasts, and only
    # when the head cannot be satisfied, may a smaller queued request go ahead.
    # Permits leave the free pool before the waiter is signalled, so a newcomer
    # can never take them in the meantime.
    #
    # @!visibility private
    def ns_dispatch
      loop do
        head = @queue.first
        break unless head

        if @free >= head.permits
          @queue.shift
          ns_grant(head)
          @bypasses = 0
          next
        end

        break unless ns_bypass_allowed?

        candidate = ns_bypass_candidate(head)
        break unless candidate

        @queue.delete_at(ns_queue_index(candidate))
        @bypasses += 1
        ns_grant(candidate)
      end
    end

    # @!visibility private
    def ns_bypass_candidate(head)
      @queue.each do |waiter|
        next if waiter.equal?(head)
        return waiter if waiter.permits < head.permits && waiter.permits <= @free
      end
      nil
    end

    # @!visibility private
    def ns_grant(waiter)
      @free -= waiter.permits
      waiter.state = :granted
      waiter.condition.signal
    end
  end
end