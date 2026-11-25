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
      @busy_processors            = Shoryuken::Helpers::AtomicCounter.new(0)
      @executor                   = executor
      @running                    = Shoryuken::Helpers::AtomicBoolean.new(true)
      @stop_new_dispatching       = Shoryuken::Helpers::AtomicBoolean.new(false)
      @dispatch_started           = Shoryuken::Helpers::AtomicBoolean.new(false)
      @dispatching_release_signal = ::Queue.new
      @fifo_groups                = FifoGroups.new
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
      @stop_new_dispatching.make_true

      # If the dispatch loop never ran (e.g. a stop arrives before the start
      # Future is scheduled, or in an embedded host whose executor is
      # saturated), there is no loop to observe the flag and close the release
      # signal, so await_dispatching_in_progress would block forever. Close it
      # here in that case. Queue#close is idempotent, so a dispatch_loop that
      # does start later and closes it again is harmless.
      @dispatching_release_signal.close unless @dispatch_started.true?
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
      @dispatching_release_signal.pop
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

    # The main dispatch loop
    #
    # @return [void]
    def dispatch_loop
      # Mark that the loop has run at least once so stop_new_dispatching knows
      # it can rely on the loop to close the release signal (and otherwise
      # closes it itself to avoid a deadlock).
      @dispatch_started.make_true

      if @stop_new_dispatching.true? || !running?
        # Close (instead of push) so every pending and future
        # await_dispatching_in_progress call returns, not just the first one
        @dispatching_release_signal.close
        return
      end

      @executor.post { dispatch }
    rescue Concurrent::RejectedExecutionError
      # The executor was shut down between the running? check and the post
      # (e.g. a hard stop racing the dispatch loop); release any waiters as
      # the dispatch chain ends here
      @dispatching_release_signal.close
    rescue StandardError
      # Unexpected error from post (e.g. ThreadError if the OS thread limit is
      # hit); the dispatch chain ends here so release waiters before re-raising
      @dispatching_release_signal.close
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
    # @return [Integer] the number of busy processors
    def busy
      @busy_processors.value
    end

    # Returns the count of ready processors
    #
    # @return [Integer] the number of available processors
    def ready
      @max_processors - busy
    end

    # Handles completion of processor work
    #
    # @param queue [String] the queue name
    # @return [void]
    def processor_done(queue)
      @busy_processors.decrement
      fire_utilization_update_event

      client_queue = Shoryuken::Client.queues(queue)
      return unless client_queue.fifo?
      return unless @polling_strategy.respond_to?(:message_processed)

      @polling_strategy.message_processed(queue)
    rescue => e
      # Swallow (but log) failures from the SQS lookups or the strategy callback:
      # the busy counter was already decremented above and the caller's ensure
      # must not run completion twice
      logger.error { "Processor completion failed for #{queue}: #{e.message}" }
      logger.debug { e.backtrace.join("\n") } unless e.backtrace.nil?
    end

    # Assigns a message to a processor
    #
    # @param queue_name [String] the queue name
    # @param sqs_msg [Aws::SQS::Types::Message, Array<Aws::SQS::Types::Message>] the message or batch
    # @return [void]
    def assign(queue_name, sqs_msg)
      unless running?
        release(sqs_msg)
        return
      end

      logger.debug { "Assigning #{sqs_msg.message_id}" }

      fifo_lease = register_fifo_lease(queue_name, sqs_msg)

      @busy_processors.increment
      fire_utilization_update_event

      # Completion runs in an ensure so it executes exactly once whether
      # processing succeeds or raises. The previous `.then { processor_done }
      # .rescue { processor_done }` chain ran completion twice when
      # processor_done itself raised, double decrementing the busy counter
      # and silently breaking the concurrency limit
      @executor.post do
        original_priority = Thread.current.priority
        outcome = :acknowledged
        processing_entered = false

        begin
          Thread.current.priority = Shoryuken.thread_priority
          fifo_lease&.wait_for_turn
          processing_entered = true
          Processor.process(queue_name, sqs_msg)
          outcome = :retry_pending if fifo_retry_scheduled?(sqs_msg)
        rescue Exception
          outcome = :retry_pending
          raise
        ensure
          Thread.current.priority = original_priority

          if fifo_lease
            if processing_entered
              outcome = :acknowledged if fifo_acknowledged?(sqs_msg)
              fifo_lease.settle(outcome)
            else
              # The worker never entered (shutdown/interrupt while queued on the group barrier). Remove its waiter slot
              # so a dead lease cannot become the active owner later.
              fifo_lease.cancel
            end
          end

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
      # increment here. Leaking it would permanently shrink `ready`
      # (@max_processors - busy) until dispatch stalls and the group stops
      # processing. The message was never processed, so we must not run the
      # FIFO message_processed callback - decrement directly instead.
      fifo_lease&.cancel
      @busy_processors.decrement
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
    # @param queue [Shoryuken::Polling::QueueConfiguration] the queue configuration
    # @return [void]
    def dispatch_batch(queue)
      batch = @fetcher.fetch(queue, BATCH_LIMIT)
      @polling_strategy.messages_found(queue.name, batch.size)
      assign(queue.name, patch_batch!(batch)) if batch.any?
    end

    # Dispatches individual messages from a queue
    #
    # @param queue [Shoryuken::Polling::QueueConfiguration] the queue configuration
    # @return [void]
    def dispatch_single_messages(queue)
      messages = @fetcher.fetch(queue, ready)
      @polling_strategy.messages_found(queue.name, messages.size)
      messages.each { |message| assign(queue.name, message) }
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

    # Registers the ordered FIFO group slot for a single message.
    #
    # Registration happens on the dispatch thread while messages from one receive are assigned in SQS order. The returned
    # lease blocks processor threads until all earlier messages for the same queue/message group have been acknowledged.
    # Messages from other groups get independent leases and continue in parallel.
    #
    # @param queue_name [String] the queue name
    # @param sqs_msg [Object] the received message
    # @return [FifoGroups::Lease, nil] the lease, or nil when the message is not ordered by a FIFO group
    def register_fifo_lease(queue_name, sqs_msg)
      group_id = fifo_message_group_id(sqs_msg)
      return nil unless group_id

      @fifo_groups.register([queue_name, group_id], fifo_message_id(sqs_msg))
    end

    # Reads the SQS MessageGroupId without assuming whether the message is an AWS type, a Shoryuken::Message, or a test
    # double.
    #
    # @param sqs_msg [Object] the received message
    # @return [String, nil]
    def fifo_message_group_id(sqs_msg)
      Processor.message_group_id(sqs_msg)
    end

    # Reads the stable SQS message id used to recognize a redelivery of the predecessor that is waiting for retry.
    #
    # @param sqs_msg [Object] the received message
    # @return [String, Object, nil]
    def fifo_message_id(sqs_msg)
      return sqs_msg['message_id'] || sqs_msg[:message_id] if sqs_msg.is_a?(Hash)

      sqs_msg.message_id if sqs_msg.respond_to?(:message_id)
    rescue StandardError
      nil
    end

    # Whether the delivery has a successful delete receipt. This takes precedence over a later middleware/error so a
    # true acknowledgement releases same-group successors even if post-delete bookkeeping raises.
    #
    # @param sqs_msg [Object] the processed message
    # @return [Boolean]
    def fifo_acknowledged?(sqs_msg)
      if sqs_msg.respond_to?(:instance_variable_defined?) &&
         sqs_msg.instance_variable_defined?(:@shoryuken_acknowledged)
        return sqs_msg.instance_variable_get(:@shoryuken_acknowledged) == true
      end

      sqs_msg.acknowledged? if sqs_msg.respond_to?(:acknowledged?)
    rescue StandardError
      false
    end

    # ExponentialBackoffRetry swallows the worker exception after scheduling a visibility timeout retry and marks the
    # message. Such a settlement is not an acknowledgement, so its same-group successors must remain held.
    #
    # @param sqs_msg [Object] the processed message
    # @return [Boolean]
    def fifo_retry_scheduled?(sqs_msg)
      sqs_msg.respond_to?(:instance_variable_defined?) &&
        sqs_msg.instance_variable_defined?(:@shoryuken_retry_scheduled) &&
        sqs_msg.instance_variable_get(:@shoryuken_retry_scheduled) == true
    rescue StandardError
      false
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

  # Thread-safe ordered barriers for FIFO message groups.
  #
  # A key is a [queue_name, message_group_id] pair. The first registered lease for an idle group is runnable; later
  # registrations wait in receive order. A failed or delayed-retry predecessor keeps ownership of the barrier, and a
  # redelivery with the same SQS message id is allowed to take that ownership without joining the back of the queue.
  class FifoGroups
    # One scheduled execution slot.
    class Lease
      # @return [Object] stable SQS message identifier
      attr_reader :message_id

      # @param groups [Shoryuken::FifoGroups] the owning registry
      # @param key [Array<String>] queue/group key
      # @param message_id [Object] stable SQS message identifier
      def initialize(groups, key, message_id)
        @groups = groups
        @key = key
        @message_id = message_id
        @state = :waiting
      end

      # Blocks until this slot is the active group execution.
      #
      # @return [void]
      def wait_for_turn
        @groups.wait_for_turn(self)
      end

      # Records the final processing settlement and advances the next slot.
      #
      # @param outcome [Symbol] :acknowledged or :retry_pending
      # @return [void]
      def settle(outcome)
        @groups.settle(self, outcome)
      end

      # Removes a slot whose work was never posted or never started.
      #
      # @return [void]
      def cancel
        @groups.cancel(self)
      end

      # @api private
      def running?
        @state == :running
      end

      # @api private
      def activate!
        @state = :running
      end

      # @api private
      def retry_pending?
        @state == :retry_pending
      end

      # @api private
      def mark_retry_pending!
        @state = :retry_pending
      end

      # @api private
      def cancelled?
        @state == :cancelled
      end

      # @api private
      def cancel!
        @state = :cancelled
      end

      # @api private
      attr_reader :key
    end

    def initialize
      @mutex = Mutex.new
      @condition = ConditionVariable.new
      @groups = {}
    end

    # Registers a message in its group's receive order.
    #
    # @param key [Array<String>] queue/group key
    # @param message_id [Object] stable SQS message identifier
    # @return [Lease]
    def register(key, message_id)
      @mutex.synchronize do
        group = (@groups[key] ||= new_group)
        lease = Lease.new(self, key, message_id)

        if group[:owner].nil? && !group[:retry_pending]
          group[:owner] = lease
          group[:owner_message_id] = message_id
          lease.activate!
        elsif group[:retry_pending] && message_id && message_id == group[:owner_message_id]
          # SQS redelivered the unresolved predecessor. Let this attempt pass the successors that are already waiting.
          group[:owner] = lease
          group[:retry_pending] = false
          lease.activate!
        else
          group[:waiters] << lease
        end

        lease
      end
    end

    # Waits until a lease becomes the active execution or is cancelled.
    #
    # @param lease [Lease]
    # @return [void]
    def wait_for_turn(lease)
      @mutex.synchronize do
        @condition.wait(@mutex) until lease.running? || lease.cancelled?
      end
    end

    # Settles an active lease.
    #
    # @param lease [Lease]
    # @param outcome [Symbol] :acknowledged or :retry_pending
    # @return [void]
    def settle(lease, outcome)
      @mutex.synchronize do
        group = @groups[lease.key]
        next unless group && group[:owner].equal?(lease)

        if outcome == :retry_pending
          lease.mark_retry_pending!
          group[:retry_pending] = true
        else
          promote_next(group)
        end

        delete_if_idle(lease.key, group)
        @condition.broadcast
      end
    end

    # Cancels a lease that will never run.
    #
    # @param lease [Lease]
    # @return [void]
    def cancel(lease)
      @mutex.synchronize do
        group = @groups[lease.key]
        next unless group

        lease.cancel!

        if group[:owner].equal?(lease)
          promote_next(group)
        else
          group[:waiters].delete(lease)
        end

        delete_if_idle(lease.key, group)
        @condition.broadcast
      end
    end

    private

    def new_group
      { owner: nil, owner_message_id: nil, retry_pending: false, waiters: [] }
    end

    def promote_next(group)
      next_lease = group[:waiters].shift

      if next_lease
        next_lease.activate!
        group[:owner] = next_lease
        group[:owner_message_id] = next_lease.message_id
        group[:retry_pending] = false
      else
        group[:owner] = nil
        group[:owner_message_id] = nil
        group[:retry_pending] = false
      end
    end

    def delete_if_idle(key, group)
      return if group[:owner] || group[:retry_pending] || group[:waiters].any?

      @groups.delete(key)
    end
  end
end
