require 'concurrent/atomic/mutex_semaphore'

module Concurrent

  ###################################################################

  # @!macro semaphore_method_initialize
  #
  #   Create a new `Semaphore` with the initial `count`.
  #
  #   @param [Fixnum] count the initial count
  #
  #   @param [Boolean] fair when `true` the semaphore runs in fair mode:
  #     requests for multiple permits are granted strictly in the order they
  #     were enqueued (FIFO) and permits are always handed out as a whole.
  #     A request that cannot be satisfied immediately blocks the requests
  #     behind it, and `try_acquire` never overtakes an already queued
  #     request. When `false` (the default) the original, non-fair behaviour
  #     is preserved. Fair mode is always provided by the pure-Ruby
  #     implementation, even on JRuby.
  #
  #   @raise [ArgumentError] if `count` is not an integer

  # @!macro semaphore_method_acquire
  #
  #   Acquires the given number of permits from this semaphore,
  #     blocking until all are available. If a block is given,
  #     yields to it and releases the permits afterwards.
  #
  #   @param [Fixnum] permits Number of permits to acquire
  #
  #   @raise [ArgumentError] if `permits` is not an integer or is less than zero
  #
  #   @return [nil, BasicObject] Without a block, `nil` is returned. If a block
  #   is given, its return value is returned.

  # @!macro semaphore_method_available_permits
  #
  #   Returns the current number of permits available in this semaphore.
  #
  #   @return [Integer]

  # @!macro semaphore_method_drain_permits
  #
  #   Acquires and returns all permits that are immediately available.
  #
  #   @return [Integer]

  # @!macro semaphore_method_try_acquire
  #
  #   Acquires the given number of permits from this semaphore,
  #     only if all are available at the time of invocation or within
  #     `timeout` interval. If a block is given, yields to it if the permits
  #     were successfully acquired, and releases them afterward, returning the
  #     block's return value.
  #
  #   @param [Fixnum] permits the number of permits to acquire
  #
  #   @param [Fixnum] timeout the number of seconds to wait for the counter
  #     or `nil` to return immediately
  #
  #   @raise [ArgumentError] if `permits` is not an integer or is less than zero
  #
  #   @return [true, false, nil, BasicObject] `false` if no permits are
  #     available, `true` when acquired a permit. If a block is given, the
  #     block's return value is returned if the permits were acquired; if not,
  #     `nil` is returned.

  # @!macro semaphore_method_release
  #
  #   Releases the given number of permits, returning them to the semaphore.
  #
  #   @param [Fixnum] permits Number of permits to return to the semaphore.
  #
  #   @raise [ArgumentError] if `permits` is not a number or is less than zero
  #
  #   @return [nil]

  ###################################################################

  # @!macro semaphore_public_api
  #
  #   @!method initialize(count, fair = false)
  #     @!macro semaphore_method_initialize
  #
  #   @!method fair?
  #     Returns `true` when this semaphore grants queued requests in strict
  #       FIFO order (fair mode), `false` otherwise.
  #     @return [Boolean]
  #
  #   @!method acquire(permits = 1)
  #     @!macro semaphore_method_acquire
  #
  #   @!method available_permits
  #     @!macro semaphore_method_available_permits
  #
  #   @!method drain_permits
  #     @!macro semaphore_method_drain_permits
  #
  #   @!method try_acquire(permits = 1, timeout = nil)
  #     @!macro semaphore_method_try_acquire
  #
  #   @!method release(permits = 1)
  #     @!macro semaphore_method_release

  ###################################################################

  # @!visibility private
  # @!macro internal_implementation_note
  SemaphoreImplementation = if Concurrent.on_jruby?
                              require 'concurrent/utility/native_extension_loader'
                              JavaSemaphore
                            else
                              MutexSemaphore
                            end
  private_constant :SemaphoreImplementation

  # @!macro semaphore
  #
  #   A counting semaphore. Conceptually, a semaphore maintains a set of
  #   permits. Each {#acquire} blocks if necessary until a permit is
  #   available, and then takes it. Each {#release} adds a permit, potentially
  #   releasing a blocking acquirer.
  #   However, no actual permit objects are used; the Semaphore just keeps a
  #   count of the number available and acts accordingly.
  #   Alternatively, permits may be acquired within a block, and automatically
  #   released after the block finishes executing.
  #
  #   A single operation may request more than one permit. By default the
  #   semaphore is *non-fair*: whenever permits are released they may be taken
  #   by whichever request can be satisfied, so a later small request can
  #   repeatedly snatch permits that trickle in while a heavy request that
  #   needs many permits is still waiting.
  #
  #   Pass `fair: true` (or `true` as the second argument) to switch to
  #   *fair* mode. In fair mode requests are granted strictly in enqueue
  #   order (FIFO). Permits are always acquired as a whole — a request never
  #   reserves part of its permits and then waits for the rest. If the
  #   request at the head of the queue cannot be fully satisfied, the permits
  #   it cannot use stay reserved and the requests behind it cannot overtake
  #   it. When the head gives up (times out) the next request is advanced
  #   immediately. Even `try_acquire` respects the queue and cannot cut in
  #   front of requests that are already waiting.
  #
  # @!macro semaphore_public_api
  # @example
  #   semaphore = Concurrent::Semaphore.new(2)
  #
  #   t1 = Thread.new do
  #     semaphore.acquire
  #     puts "Thread 1 acquired semaphore"
  #   end
  #
  #   t2 = Thread.new do
  #     semaphore.acquire
  #     puts "Thread 2 acquired semaphore"
  #   end
  #
  #   t3 = Thread.new do
  #     semaphore.acquire
  #     puts "Thread 3 acquired semaphore"
  #   end
  #
  #   t4 = Thread.new do
  #     sleep(2)
  #     puts "Thread 4 releasing semaphore"
  #     semaphore.release
  #   end
  #
  #   [t1, t2, t3, t4].each(&:join)
  #
  #   # prints:
  #   # Thread 3 acquired semaphore
  #   # Thread 2 acquired semaphore
  #   # Thread 4 releasing semaphore
  #   # Thread 1 acquired semaphore
  #
  # @example
  #   semaphore = Concurrent::Semaphore.new(1)
  #
  #   puts semaphore.available_permits
  #   semaphore.acquire do
  #     puts semaphore.available_permits
  #   end
  #   puts semaphore.available_permits
  #
  #   # prints:
  #   # 1
  #   # 0
  #   # 1
  #
  # @example fair mode: heavy requests are not starved by later small ones
  #   semaphore = Concurrent::Semaphore.new(2, true) # the second argument
  #                                                 # enables fair (FIFO) mode
  #
  #   order = []
  #
  #   # requests 3 permits when only 2 are available, so it must wait
  #   heavy = Thread.new { semaphore.acquire(3); order << :heavy; sleep 0.05 }
  #   sleep(0.01)
  #
  #   # a later request for 1 permit cannot steal the 2 available permits
  #   light = Thread.new { semaphore.acquire(1); order << :light }
  #   sleep(0.05)
  #   order # => [] (the light request is still queued behind the heavy one)
  #
  #   semaphore.release(2) # 4 permits are now available
  #   [heavy, light].each(&:join)
  #   order # => [:heavy, :light] (strict enqueue order)
  class Semaphore < SemaphoreImplementation

    # Creates a semaphore. When `fair` is truthy the pure-Ruby FIFO
    # implementation is used on every platform (including JRuby), otherwise
    # the native platform implementation is used exactly as before.
    #
    # @param [Fixnum] count the initial count
    # @param [Boolean] fair whether to run in fair (FIFO) mode
    #
    # @return [Semaphore]
    def self.new(count, fair = false)
      if fair && !MutexSemaphore.equal?(SemaphoreImplementation)
        MutexSemaphore.new(count, true)
      else
        super(count, fair ? true : false)
      end
    end
  end
end
