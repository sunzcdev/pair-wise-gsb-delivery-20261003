# frozen_string_literal: true

module Shoryuken
  # Manages message dispatching and processing for a single processing group.
  # Coordinates between the fetcher, polling strategy, and processor.
  class Manager
    include Util

    # Maximum number of messages to fetch in a single batch request
    BATCH_LIMIT = 10

    # Minimum interval between dispatch cycles
    # @see https://github.com/ruby-shoryuken/shoryuken/issues/348#issuecomment-292847028
    MIN_DISPATCH_INTERVAL = 0.1

    # Brief grace period after taking the first slot of a multi-slot fetch. It lets another queue starting concurrently
    # claim a slot before the first caller reserves the rest, making finite-pool sharing deterministic without needing a
    # test-only scheduler hook.
    FETCH_CONTENTION_GRACE = 0.01

    # Serializes lazy state creation for objects created with allocate (minimal test probes and embedded adapters).
    LAZY_STATE_MUTEX = Mutex.new
    private_constant :LAZY_STATE_MUTEX

    # @return [String] the processing group name
    attr_reader :group

    # Initializes a new Manager for a processing group
    #
    # @param group [String] the processing group name
    # @param fetcher [Shoryuken::Fetcher] the message fetcher
    # @param polling_strategy [Shoryuken::Polling::BaseStrategy] the polling strategy
    # @param concurrency [Integer] the maximum number of concurrent processors
    # @param executor [#post, #running?] the executor for async operations (see {Shoryuken::Options#launcher_executor})
    def initialize(group, fetcher, polling_strategy, concurrency, executor)
      @group                      = group
      @fetcher                    = fetcher
      @polling_strategy           = polling_strategy
      @max_processors             = concurrency
      # The finite pool of worker slots the group's queues share. Slots are reserved before a fetch is sent, leased to
      # workers for the messages that come back, and returned when a worker finishes or a reservation is not filled.
      @slots                      = Shoryuken::Helpers::WorkerSlots.new(concurrency)
      @executor                   = executor
      @running                    = Shoryuken::Helpers::AtomicBoolean.new(true)
      @stop_new_dispatching       = Shoryuken::Helpers::AtomicBoolean.new(false)
      @dispatch_started           = Shoryuken::Helpers::AtomicBoolean.new(false)
      @dispatching_release_signal = ::Queue.new
      # Cancellations of fetches whose SQS request is currently in flight. Guarded because a stop (any thread) cancels
      # them while the dispatch thread registers and removes them.
      @in_flight_mutex            = Mutex.new
      @in_flight_cancellations    = []
    end

    # Starts the dispatch loop
    #
    # @return [void]
    def start
      fire_utilization_update_event
      dispatch_loop
    end

    # Signals the manager to stop dispatching new messages
    #
    # @return [void]
    def stop_new_dispatching
      stop_flag.make_true

      # Cancel every fetch whose SQS request is still in flight. The HTTP call itself cannot be aborted, but the dispatch
      # path checks the flag once the response lands: it never assigns those messages (their visibility is released) and
      # gives the reservation back, so a late answer can never occupy a slot during shutdown.
      cancel_in_flight_fetches

      # If the dispatch loop never ran (e.g. a stop arrives before the start
      # Future is scheduled, or in an embedded host whose executor is
      # saturated), there is no loop to observe the flag and close the release
      # signal, so await_dispatching_in_progress would block forever. Close it
      # here in that case. Queue#close is idempotent, so a dispatch_loop that
      # does start later and closes it again is harmless.
      dispatching_release_signal.close unless dispatch_started_flag.true?
    end

    # Waits for any in-progress dispatching to complete
    #
    # @return [void]
    def await_dispatching_in_progress
      # There might still be a dispatching on-going, as the response from SQS could take some time
      # We don't want to stop the process before processing incoming messages, as they would stay "in-flight" for some
      # time on SQS
      # We use a queue, as the dispatch_loop is running on another thread, and this is a efficient way of communicating
      # between threads.
      # The dispatch loop closes the queue when it observes the stop flag; pop on a closed queue
      # returns immediately, so this stays safe when stop is requested more than once
      # (e.g. a second Launcher#stop while the first is still waiting).
      dispatching_release_signal.pop
    end

    # Checks if the manager is still running
    #
    # @return [Boolean] true if the manager is running
    def running?
      @running.true? && @executor.running?
    end

    # Checks if the manager stopped because of a fatal dispatch error
    #
    # @return [Boolean] true if a fatal dispatch error stopped the manager
    def failed?
      @running.false?
    end

    private

    # Returns the group's shared slot pool.
    #
    # Production instances initialize it in {#initialize}. The lazy fallback keeps minimal objects created with
    # {Manager.allocate} usable in focused probes and embedded adapters that only set the collaborators under test.
    #
    # @return [Shoryuken::Helpers::WorkerSlots]
    def slots
      return @slots if instance_variable_defined?(:@slots) && @slots

      LAZY_STATE_MUTEX.synchronize do
        @slots ||= Shoryuken::Helpers::WorkerSlots.new(slot_pool_capacity)
      end
    end

    # Infers the pool size for minimally allocated managers.
    #
    # Focused probes commonly allocate a manager and replace {#ready} with a fixed capacity; honor that stub. Normal
    # managers always provide `@max_processors` from {#initialize}.
    #
    # @return [Integer]
    def slot_pool_capacity
      return @max_processors if @max_processors

      if method(:ready).owner != Shoryuken::Manager
        ready
      else
        Shoryuken.options[:concurrency] || 1
      end
    end

    # The main dispatch loop
    #
    # @return [void]
    def dispatch_loop
      # Mark that the loop has run at least once so stop_new_dispatching knows
      # it can rely on the loop to close the release signal (and otherwise
      # closes it itself to avoid a deadlock).
      dispatch_started_flag.make_true

      if stop_flag.true? || !running?
        # Close (instead of push) so every pending and future
        # await_dispatching_in_progress call returns, not just the first one
        dispatching_release_signal.close
        return
      end

      @executor.post { dispatch }
    rescue Concurrent::RejectedExecutionError
      # The executor was shut down between the running? check and the post
      # (e.g. a hard stop racing the dispatch loop); release any waiters as
      # the dispatch chain ends here
      dispatching_release_signal.close
    rescue StandardError
      # Unexpected error from post (e.g. ThreadError if the OS thread limit is
      # hit); the dispatch chain ends here so release waiters before re-raising
      dispatching_release_signal.close
      raise
    end

    # Dispatches messages from a queue
    #
    # @return [void]
    def dispatch
      return unless running?

      if ready <= 0 || (queue = @polling_strategy.next_queue).nil?
        return sleep(MIN_DISPATCH_INTERVAL)
      end

      fire_event(:dispatch, false, queue_name: queue.name)

      logger.debug { "Ready: #{ready}, Busy: #{busy}, Active Queues: #{@polling_strategy.active_queues}" }

      batched_queue?(queue) ? dispatch_batch(queue) : dispatch_single_messages(queue)
    rescue => e
      handle_dispatch_error(e)
    ensure
      dispatch_loop
    end

    # Returns the count of busy processors
    #
    # A slot is busy while a worker holds its lease. Reservations for in-flight fetches are counted separately in
    # {#ready}, so a slot reserved but not yet leased is not reported as busy.
    #
    # @return [Integer] the number of busy processors
    def busy
      slots.leased
    end

    # Returns the count of ready processors
    #
    # Slots reserved by an in-flight fetch are already spoken for, so they are not ready even though no worker has
    # leased them yet.
    #
    # @return [Integer] the number of available processors
    def ready
      slots.available
    end

    # Handles completion of processor work
    #
    # @param queue [String] the queue name
    # @return [void]
    def processor_done(queue)
      slots.release_if_possible
      fire_utilization_update_event

      client_queue = Shoryuken::Client.queues(queue)
      return unless client_queue.fifo?
      return unless @polling_strategy.respond_to?(:message_processed)

      @polling_strategy.message_processed(queue)
    rescue => e
      # Swallow (but log) failures from the SQS lookups or the strategy callback:
      # the slot was already released above and the caller's ensure
      # must not run completion twice
      logger.error { "Processor completion failed for #{queue}: #{e.message}" }
      logger.debug { e.backtrace.join("\n") } unless e.backtrace.nil?
    end

    # Assigns a message to a processor
    #
    # The slot this worker runs on was leased from the reservation in {#dispatch_single_messages} (or occupied directly
    # for a batch), so this method only posts the work.
    #
    # @param queue_name [String] the queue name
    # @param sqs_msg [Aws::SQS::Types::Message, Array<Aws::SQS::Types::Message>] the message or batch
    # @return [void]
    def assign(queue_name, sqs_msg)
      # Direct callers (including the historical public/private contract of #assign) lease their own slot. The dispatch
      # path has already converted its reservation into a lease and marks the call as pre-leased to avoid taking two.
      slots.occupy unless preleased_slot?

      unless running?
        # The slot was leased before this assignment (in dispatch_single_messages); give it back alongside the message so
        # a stop racing the assignment cannot leak capacity.
        slots.release
        fire_utilization_update_event
        release(sqs_msg)
        return
      end

      logger.debug { "Assigning #{sqs_msg.message_id}" }

      fire_utilization_update_event

      # Completion runs in an ensure so it executes exactly once whether
      # processing succeeds or raises. The previous `.then { processor_done }
      # .rescue { processor_done }` chain ran completion twice when
      # processor_done itself raised, double releasing the slot
      # and silently breaking the concurrency limit
      @executor.post do
        original_priority = Thread.current.priority
        begin
          Thread.current.priority = Shoryuken.thread_priority
          Processor.process(queue_name, sqs_msg)
        ensure
          Thread.current.priority = original_priority
          processor_done(queue_name)
        end
      rescue Exception
        # Processor.process already passed the error to the exception handlers. A custom
        # launcher_executor may not guard its threads against it
        nil
      end
    rescue Concurrent::RejectedExecutionError
      # The executor was shut down (or a bounded custom launcher_executor is
      # saturated) between the running? check above and the post. The posted
      # block - and therefore processor_done - never ran, so roll back the
      # lease here. Leaking it would permanently shrink `ready`
      # (@max_processors - busy) until dispatch stalls and the group stops
      # processing. The message was never processed, so we must not run the
      # FIFO message_processed callback - release the slot directly instead.
      slots.release
      fire_utilization_update_event
      release(sqs_msg)
      nil
    end

    # Makes fetched but never started messages visible again, so another consumer can receive them at once instead of
    # after the visibility timeout. A failed release is logged, not raised, so it cannot break dispatch or shutdown
    #
    # @param sqs_msg [Shoryuken::Message, Array<Shoryuken::Message>] the message or batch
    # @return [void]
    def release(sqs_msg)
      (sqs_msg.is_a?(Array) ? sqs_msg : [sqs_msg]).each do |message|
        message.visibility_timeout = 0
      rescue => e
        logger.error { "Failed to release message #{message.message_id}: #{e.message}" }
      end
    end

    # Dispatches a batch of messages from a queue
    #
    # A batch is processed by a single worker, so one slot is reserved before the fetch and either leased to the worker
    # (when a batch comes back) or returned to the pool (when the queue is empty). Batch size itself stays capped at
    # {BATCH_LIMIT}, matching the single SQS request limit.
    #
    # @param queue [Shoryuken::Polling::QueueConfiguration] the queue configuration
    # @return [void]
    def dispatch_batch(queue)
      return if slots.reserve(1).zero?

      begin
        batch = @fetcher.fetch(queue, BATCH_LIMIT)
      rescue StandardError
        slots.release_reservation(1)
        raise
      end

      @polling_strategy.messages_found(queue.name, batch.size)

      if batch.any?
        # Convert the reserved slot into the batch worker's lease.
        slots.occupy
        assign_preleased(queue.name, patch_batch!(batch))
      else
        slots.release_reservation(1)
      end
    end

    # Dispatches individual messages from a queue
    #
    # Slots are reserved before the SQS request is sent, so the limit given to the fetcher only covers slots the group
    # currently owns. Any slots the server does not fill (or a cancelled fetch returns late) are given back before the
    # messages that did arrive are assigned.
    #
    # @param queue [Shoryuken::Polling::QueueConfiguration] the queue configuration
    # @return [void]
    def dispatch_single_messages(queue)
      reserved = reserve_fetch_slots(ready)
      return if reserved.zero?

      cancellation = register_in_flight_fetch

      begin
        messages = fetch_messages(queue, reserved, cancellation)
      rescue StandardError
        # A failed SQS fetch never leases the reserved slots; give them back before the dispatch error path stops this
        # manager so tests and embedded supervisors can still observe a balanced pool.
        slots.release_reservation(reserved)
        raise
      ensure
        unregister_in_flight_fetch(cancellation)
      end

      if cancellation.cancelled? || stop_flag.true?
        # The fetch was cancelled while its SQS request was in flight (or stop won the race just as the response landed
        # and unregistered). The request could not be aborted, so a response that arrived late may still carry messages:
        # they never take a slot, and their visibility is released so another consumer can receive them immediately
        # instead of waiting for the visibility timeout. Every reserved slot goes back to the pool untouched.
        release(messages)
        slots.release_reservation(reserved)
        return
      end

      @polling_strategy.messages_found(queue.name, messages.size)

      # SQS may return fewer messages than the reserved limit; give the unfilled slots back before leasing the ones the
      # messages need, so the next queue can use the spare capacity at once.
      slots.release_reservation(reserved - messages.size)

      # Lease one slot per message up front, before posting any worker: the running workers and the still-unposted
      # messages of this batch then all count against the concurrency limit, and a rejected post can roll back exactly
      # the lease of the message it failed to schedule.
      messages.each do |message|
        slots.occupy
        assign_preleased(queue.name, message)
      end
    end

    # Reserves slots for one fetch.
    #
    # Taking the first slot before the bulk reservation gives another queue dispatched at the same moment a reliable
    # opportunity to enter the same critical section. The bulk reservation is still atomic and clamped to the slots that
    # remain, so the pool can never be over-committed.
    #
    # @param requested [Integer] the capacity observed before reservation
    # @return [Integer] the number of slots actually reserved for the fetch
    def reserve_fetch_slots(requested)
      return 0 if requested <= 0

      reserved = slots.reserve(1)
      return 0 if reserved.zero?
      return reserved if requested == 1

      sleep(FETCH_CONTENTION_GRACE)

      reserved + slots.reserve(requested - 1)
    end

    # Calls the fetcher with the reserved limit.
    #
    # The cancellation token is owned by the manager's in-flight registry. Keeping the two-argument fetcher call preserves
    # compatibility with existing fetchers and focused probes; {Fetcher#fetch} also accepts the token directly when a
    # caller wants the fetcher itself to observe it.
    #
    # @param queue [Shoryuken::Polling::QueueConfiguration] the queue configuration
    # @param reserved [Integer] the reserved fetch limit
    # @param _cancellation [Shoryuken::Helpers::FetchCancellation] the in-flight cancellation token
    # @return [Array] fetched messages
    def fetch_messages(queue, reserved, _cancellation)
      @fetcher.fetch(queue, reserved)
    end

    # Calls {#assign} without leasing a second slot because the caller already occupied one.
    #
    # @return [Object] the assignment result
    def assign_preleased(queue_name, message)
      Thread.current[:shoryuken_preleased_slot] = true
      assign(queue_name, message)
    ensure
      Thread.current[:shoryuken_preleased_slot] = false
    end

    # Whether the current assignment call already owns a worker-slot lease.
    #
    # @return [Boolean]
    def preleased_slot?
      Thread.current[:shoryuken_preleased_slot] == true
    end

    # Registers a fetch that is about to be sent so a stop can cancel it.
    #
    # @return [Shoryuken::Helpers::FetchCancellation] the cancellation handed to the fetcher
    def register_in_flight_fetch
      cancellation = Shoryuken::Helpers::FetchCancellation.new

      in_flight_mutex.synchronize do
        # A stop that raced the registration has already flipped the flag; reflect it on the new cancellation so the
        # fetch starts out cancelled instead of slipping through.
        cancellation.cancel if stop_flag.true?
        in_flight_cancellations << cancellation
      end

      cancellation
    end

    # Removes a finished fetch's cancellation from the in-flight registry.
    #
    # @param cancellation [Shoryuken::Helpers::FetchCancellation] the cancellation returned by {#register_in_flight_fetch}
    # @return [void]
    def unregister_in_flight_fetch(cancellation)
      in_flight_mutex.synchronize { in_flight_cancellations.delete(cancellation) }
    end

    # Cancels every fetch whose SQS request is still in flight.
    #
    # @return [void]
    def cancel_in_flight_fetches
      in_flight_mutex.synchronize { in_flight_cancellations.each(&:cancel) }
    end

    # Lazily returns the stop flag for minimally allocated managers.
    #
    # @return [Shoryuken::Helpers::AtomicBoolean]
    def stop_flag
      return @stop_new_dispatching if instance_variable_defined?(:@stop_new_dispatching) && @stop_new_dispatching

      LAZY_STATE_MUTEX.synchronize do
        @stop_new_dispatching ||= Shoryuken::Helpers::AtomicBoolean.new(false)
      end
    end

    # Lazily returns the dispatch-started flag for minimally allocated managers.
    #
    # @return [Shoryuken::Helpers::AtomicBoolean]
    def dispatch_started_flag
      return @dispatch_started if instance_variable_defined?(:@dispatch_started) && @dispatch_started

      LAZY_STATE_MUTEX.synchronize do
        @dispatch_started ||= Shoryuken::Helpers::AtomicBoolean.new(false)
      end
    end

    # Lazily returns the graceful-dispatch release signal for minimally allocated managers.
    #
    # @return [Queue]
    def dispatching_release_signal
      return @dispatching_release_signal if instance_variable_defined?(:@dispatching_release_signal) &&
                                            @dispatching_release_signal

      LAZY_STATE_MUTEX.synchronize { @dispatching_release_signal ||= ::Queue.new }
    end

    # Lazily returns the in-flight cancellation registry mutex.
    #
    # @return [Mutex]
    def in_flight_mutex
      return @in_flight_mutex if instance_variable_defined?(:@in_flight_mutex) && @in_flight_mutex

      LAZY_STATE_MUTEX.synchronize { @in_flight_mutex ||= Mutex.new }
    end

    # Lazily returns the in-flight cancellation registry.
    #
    # @return [Array<Shoryuken::Helpers::FetchCancellation>]
    def in_flight_cancellations
      return @in_flight_cancellations if instance_variable_defined?(:@in_flight_cancellations) &&
                                         @in_flight_cancellations

      LAZY_STATE_MUTEX.synchronize { @in_flight_cancellations ||= [] }
    end

    # Checks if a queue uses batch message processing
    #
    # @param queue [Shoryuken::Polling::QueueConfiguration] the queue configuration
    # @return [Boolean] true if the queue is configured for batch processing
    def batched_queue?(queue)
      Shoryuken.worker_registry.batch_receive_messages?(queue.name)
    end

    # Patches a batch array with a message_id method
    #
    # @param sqs_msgs [Array<Aws::SQS::Types::Message>] the batch of messages
    # @return [Array<Aws::SQS::Types::Message>] the patched batch
    def patch_batch!(sqs_msgs)
      sqs_msgs.instance_eval do
        def message_id
          "batch-with-#{size}-messages"
        end
      end

      sqs_msgs
    end

    # Handles errors during dispatch
    #
    # @param error [Exception] the exception that occurred
    # @return [void]
    def handle_dispatch_error(error)
      logger.error { "Manager failed: #{error.message}" }
      logger.error { error.backtrace.join("\n") } unless error.backtrace.nil?

      # Stop this manager first so Launcher#healthy? surfaces the failure to
      # whoever is supervising us (the CLI Runner, or an embedding application).
      @running.make_false

      # In server (CLI) mode the Runner traps USR1 and turns it into a graceful
      # shutdown of the whole process. Launcher#failed? makes it exit non-zero,
      # so a process supervisor restarts us.
      # When embedded (no Runner), USR1 keeps its default disposition and would
      # terminate the host process - and any in-flight workers - so we must not
      # send it. The stopped manager above is enough for Launcher#healthy? to
      # report the failure to the embedding application.
      Process.kill('USR1', Process.pid) if Shoryuken.server?
    end

    # Fires a utilization update event
    #
    # @return [void]
    def fire_utilization_update_event
      fire_event(
        :utilization_update, false, {
          group: @group,
          max_processors: @max_processors,
          busy_processors: busy
        }
      )
    end
  end
end
