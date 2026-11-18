require 'concurrent/atomic/mutex_semaphore'

module Concurrent

  ###################################################################

  # @!macro semaphore_method_initialize
  #
  #   Create a new `Semaphore` with the initial `count`.
  #
  #   @param [Fixnum] count the initial count
  #
  #   @param [Hash] options admission-policy options
  #
  #   @option options [Symbol] :queue_mode (`:fifo`) how concurrent requests
  #     are admitted
  #     * `:fifo` queued requests are admitted in arrival order, so an older
  #       large request cannot be starved by newer small ones
  #     * `:bounded_bypass` queued requests are admitted in arrival order, but
  #       a request smaller than the current head of the queue may overtake it
  #       at most `:bypass_limit` times while that head is in place
  #
  #   @option options [Fixnum] :bypass_limit (`1`) the maximum number of
  #     smaller requests that may overtake the current head of the queue;
  #     ignored in `:fifo` mode
  #
  #   @raise [ArgumentError] if `count` is not an integer
  #   @raise [ArgumentError] if `:queue_mode` is not a known policy
  #   @raise [ArgumentError] if `:bypass_limit` is not a non-negative integer
  #   @raise [ArgumentError] if `options` contains an unknown key

  # @!macro semaphore_method_acquire
  #
  #   Acquires the given number of permits from this semaphore,
  #     blocking until all are available. If a block is given,
  #     yields to it and releases the permits afterwards.
  #
  #   The permits behind a request are granted as one batch or not at all;
  #   a large request never takes a partial grant that would leave it holding
  #   permits it cannot use. Requests are queued according to the semaphore's
  #   {#queue_mode} instead of relying on whichever thread a condition variable
  #   happens to wake.
  #
  #   Queuing means the head of the queue is served before newer requests, so
  #   in `:fifo` mode a request larger than the semaphore's total capacity
  #   blocks every later request indefinitely. Use the `:bounded_bypass` policy
  #   when smaller requests must still make progress behind such a head.
  #
  #   If the block raises, only the permits that were actually granted are
  #   released; a zero-permit request returns nothing to the semaphore.
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
  #   Together with {#reserved_permits} this partitions the permits the
  #   semaphore is still holding: `available_permits + reserved_permits` is the
  #   free pool. Permits which have already been granted to a waiter belong to
  #   that waiter until it releases them and are counted in neither.
  #
  #   @return [Integer]

  # @!macro semaphore_method_reserved_permits
  #
  #   Returns the number of free permits the queue has set aside for the
  #   request at the head of the queue.
  #
  #   The head is the only waiter with a reservation; later waiters do not have
  #   one. The reservation is capped by the head's own request and by the free
  #   pool, so a head which is granted immediately never holds one. In
  #   `:fifo` mode these permits are not available to anyone else. In
  #   `:bounded_bypass` mode a smaller request may still use them while the
  #   bypass budget lasts, so a non-zero value does not make admission
  #   impossible.
  #
  #   @return [Integer]

  # @!macro semaphore_method_waiting_count
  #
  #   Returns the number of requests currently waiting in the queue.
  #
  #   @return [Integer]

  # @!macro semaphore_method_queued_permits
  #
  #   Returns the total number of permits requested by all queued waiters.
  #
  #   @return [Integer]

  # @!macro semaphore_method_queue_mode
  #
  #   Returns the admission policy in effect, either `:fifo` or
  #   `:bounded_bypass`.
  #
  #   @return [Symbol]

  # @!macro semaphore_method_bypass_limit
  #
  #   Returns how many smaller requests may overtake the current head of the
  #   queue. Always `0` in `:fifo` mode.
  #
  #   @return [Integer]

  # @!macro semaphore_method_drain_permits
  #
  #   Acquires and returns all permits that are immediately available.
  #
  #   Permits reserved for the head of the queue are not drained.
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
  #   A request which cannot be admitted joins the queue, so it respects
  #   {#queue_mode} and can be overtaken only as that policy allows. A waiter
  #   which reaches its timeout is removed from the queue, and any permits set
  #   aside for it go back to the semaphore. A `timeout` of `0` behaves like no
  #   timeout at all: only an immediately available batch is taken.
  #
  #   @param [Fixnum] permits the number of permits to acquire
  #
  #   @param [Fixnum] timeout the number of seconds to wait for the counter
  #     or `nil` to return immediately
  #
  #   @raise [ArgumentError] if `permits` is not an integer or is less than zero
  #   @raise [ArgumentError] if `timeout` is given and is not a number
  #
  #   @return [true, false, nil, BasicObject] `false` if no permits are
  #     available, `true` when acquired a permit. If a block is given, the
  #     block's return value is returned if the permits were acquired; if not,
  #     `nil` is returned.

  # @!macro semaphore_method_release
  #
  #   Releases the given number of permits, returning them to the semaphore.
  #
  #   Released permits are handed to the queue in admission order, never past
  #   the head, and the free counter is only ever increased here so concurrent
  #   releases cannot drive it negative.
  #
  #   @param [Fixnum] permits Number of permits to return to the semaphore.
  #
  #   @raise [ArgumentError] if `permits` is not a number or is less than zero
  #
  #   @return [nil]

  ###################################################################

  # @!macro semaphore_public_api
  #
  #   @!method initialize(count, options = {})
  #     @!macro semaphore_method_initialize
  #
  #   @!method acquire(permits = 1)
  #     @!macro semaphore_method_acquire
  #
  #   @!method available_permits
  #     @!macro semaphore_method_available_permits
  #
  #   @!method reserved_permits
  #     @!macro semaphore_method_reserved_permits
  #
  #   @!method waiting_count
  #     @!macro semaphore_method_waiting_count
  #
  #   @!method queue_length
  #     @!macro semaphore_method_waiting_count
  #
  #   @!method queued_permits
  #     @!macro semaphore_method_queued_permits
  #
  #   @!method queue_mode
  #     @!macro semaphore_method_queue_mode
  #
  #   @!method bypass_limit
  #     @!macro semaphore_method_bypass_limit
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
  #   A single request may ask for more than one permit and is granted as one
  #   batch. Because a large request can be starved by a stream of smaller ones,
  #   the semaphore queues unanswered requests and makes the admission policy
  #   explicit through the `:queue_mode` constructor option instead of waking
  #   waiters opportunistically from a shared condition variable. See
  #   {#queue_mode} and {#reserved_permits} for the observable consequences.
  #
  #   The pure-Ruby implementation backs this class on every Ruby engine so the
  #   queueing and reservation contract is identical everywhere.
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
  #   # prints two acquisitions, then "Thread 4 releasing semaphore", then the
  #   # third acquisition. Which two run first depends on which threads reach
  #   # the semaphore first; once queued, waiters are served in queue order.
  #
  # @example Queue mode with bounded bypass
  #   semaphore = Concurrent::Semaphore.new(3)
  #   semaphore.acquire(2)                               # 1 permit left
  #   semaphore.queue_mode                               #=> :fifo
  #
  #   big   = Thread.new { semaphore.acquire(3) }        # queued first, waits
  #   sleep(0.01)
  #   small = Thread.new { semaphore.acquire(1) }        # queued behind it
  #   sleep(0.01)
  #
  #   semaphore.waiting_count                            #=> 2
  #   semaphore.queued_permits                           #=> 4
  #
  #   # Both requests wait: in FIFO mode `small` may not overtake `big`.
  #
  #   semaphore = Concurrent::Semaphore.new(3, queue_mode: :bounded_bypass,
  #                                            bypass_limit: 1)
  #   semaphore.acquire(2)                               # 1 permit left
  #   big   = Thread.new { semaphore.acquire(3) }        # queued first
  #   sleep(0.01)
  #   semaphore.reserved_permits                         #=> 1, earmarked for big
  #   small = Thread.new { semaphore.acquire(1) }        # overtakes once
  #   sleep(0.01)
  #   semaphore.waiting_count                            #=> 1
  #
  #   # `small` is admitted because it asks for fewer permits than `big` and
  #   # the bypass budget allows it. `big` stays at the head of the queue.
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
  class Semaphore < MutexSemaphore
  end
end