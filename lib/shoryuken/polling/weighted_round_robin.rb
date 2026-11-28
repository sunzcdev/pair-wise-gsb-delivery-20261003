# frozen_string_literal: true

module Shoryuken
  module Polling
    # A polling strategy that processes queues in weighted round-robin order.
    # Queue weights determine how often each queue is polled relative to others.
    # Queues are temporarily paused when no messages are found.
    #
    # A queue handed out by {#next_queue} can end its turn in two ways that look alike from the strategy's point of view
    # but must not be treated the same:
    #
    # - {#messages_found} reports what SQS answered. Zero means the queue is empty, so it is paused for the delay.
    # - {#poll_skipped} reports that no request was made because the group had no free processor. A shortage of local
    #   capacity says nothing about the queue's contents, so the queue keeps its weight and is not paused.
    #
    # Every selection carries a unique poll id (see {QueueConfiguration#poll_id}), and a report only applies to the
    # queue's current one. The current id moves on when the queue is selected again, when a report is applied or
    # skipped, when its backoff expires and when a processed message resumes it. A report that arrives for an older id
    # describes a state the queue has already left, so it is ignored: a late empty answer cannot pause a queue that has
    # started to recover, restart a backoff that has already passed, or undo the weight a poll recovered.
    class WeightedRoundRobin < BaseStrategy
      # Initializes a new WeightedRoundRobin polling strategy
      #
      # @param queues [Array<String>] array of queue names, with weights indicated by repetition
      # @param delay [Float, nil] delay in seconds before unpausing empty queues
      def initialize(queues, delay = nil)
        @initial_queues = queues
        @queues = queues.dup.uniq
        @paused_queues = []
        @delay = delay
        # Current poll id per queue: handed out with each selection and moved on whenever the queue moves on
        @poll_ids = Hash.new(0)
        # next_queue/messages_found/poll_skipped run on the dispatch thread while
        # message_processed runs on processor-completion threads (for FIFO
        # queues), so all access to the shared state is serialized.
        @mutex = Mutex.new
      end

      # Returns the next queue to poll in round-robin order
      #
      # A new selection starts a new poll id, so a report still in flight for the queue's previous selection becomes
      # stale as soon as the queue is handed out again.
      #
      # @return [QueueConfiguration, nil] the next queue configuration or nil if all paused
      def next_queue
        @mutex.synchronize do
          unpause_queues
          queue = @queues.shift
          if queue.nil?
            nil
          else
            @queues << queue
            QueueConfiguration.new(queue, {}, next_poll_id(queue))
          end
        end
      end

      # Handles the result of polling a queue, adjusting weight if messages were found
      #
      # A report for a poll id the queue has already left is ignored: an empty answer that arrives after the queue was
      # selected again, recovered its weight or resumed must not pause it, and replaying an answer already applied must
      # not restart the backoff it started.
      #
      # @param queue [String, QueueConfiguration] the queue name, or the selection the report answers
      # @param messages_found [Integer] number of messages found
      # @param poll_id [Integer, nil] the poll id of the selection this report answers, nil for callers that do not
      #   track it
      # @return [void]
      def messages_found(queue, messages_found, poll_id = nil)
        queue, poll_id = selection(queue, poll_id)

        @mutex.synchronize do
          return if stale_report?(queue, poll_id)

          if messages_found == 0
            pause(queue)
          else
            increase_weight(queue)
          end

          # The turn is over, so its answer can no longer apply a second time
          next_poll_id(queue)
        end
      end

      # Handles a selection that was not polled because the group had no free processor
      #
      # Nothing is requested from SQS, so nothing is learned about the queue: unlike an empty report this must not pause
      # the queue or lower its weight. The selection also gives its turn back, because next_queue moves the selected
      # queue to the end: the dispatch loop selects a queue on every pass while it waits for capacity, and a turn that
      # was never taken must not move the queue back in the rotation or reorder the queue that is polled once capacity
      # returns. The queue keeps its weight and its place, so it is the queue polled when a processor frees up.
      #
      # @param queue [String, QueueConfiguration] the queue name, or the selection that was skipped
      # @param poll_id [Integer, nil] the poll id of the skipped selection, nil for callers that do not track it
      # @return [void]
      def poll_skipped(queue, poll_id = nil)
        queue, poll_id = selection(queue, poll_id)

        @mutex.synchronize do
          return if stale_report?(queue, poll_id)

          # No request was made for this selection, so no answer can belong to it any more
          next_poll_id(queue)
          restore_selection(queue)
          logger.debug { "Skipped polling #{queue}: no free processor" }
        end
      end

      # Returns the list of active queues with their current weights
      #
      # @return [Array<Array>] array of [queue_name, weight] pairs
      def active_queues
        @mutex.synchronize { unparse_queues(@queues) }
      end

      # Called when a message from a queue has been processed
      #
      # The queue is resumed at once (its pause is dropped), so a report still in flight for an earlier selection
      # describes a state the queue has already left. Moving the poll id on here makes those reports stale, whether or
      # not they carry one.
      #
      # @param queue [String] the queue name
      # @return [void]
      def message_processed(queue)
        @mutex.synchronize do
          next_poll_id(queue)
          resumed_queue = @paused_queues.find { |_time, name| name == queue }
          resumed_queue[0] = Time.at(0) if resumed_queue
        end
      end

      private

      # Normalizes a report's queue argument
      #
      # Callers that kept the {QueueConfiguration} returned by {#next_queue} do not need to repeat its poll id.
      #
      # @param queue [String, QueueConfiguration] the queue name or the selection the report is about
      # @param poll_id [Integer, nil] the explicitly given poll id, when any
      # @return [Array<String, Integer>] the queue name and the poll id to check
      def selection(queue, poll_id)
        if queue.is_a?(QueueConfiguration)
          [queue.name, poll_id || queue.poll_id]
        else
          [queue, poll_id]
        end
      end

      # Issues and records the queue's next poll id
      #
      # Must be called with the mutex held.
      #
      # @param queue [String] the queue name
      # @return [Integer] the new poll id
      def next_poll_id(queue)
        @poll_ids[queue] = @poll_ids[queue] + 1
      end

      # Checks whether a report answers a selection the queue has already left
      #
      # Without a poll id (direct callers that do not track the selection) there is nothing to compare, so the report is
      # applied as before.
      #
      # @param queue [String] the queue name
      # @param poll_id [Integer, nil] the poll id of the selection the report answers
      # @return [Boolean] true when the report must be ignored
      def stale_report?(queue, poll_id)
        !poll_id.nil? && poll_id < @poll_ids[queue]
      end

      # Puts a selected queue back at the head of the rotation
      #
      # next_queue moves the selected queue to the end, so a selection that was never polled gives that turn back with a
      # single rotation. If the queue is no longer at the end (e.g. it was paused or reweighted in between) there is
      # nothing to restore.
      #
      # @param queue [String] the queue name
      # @return [void]
      def restore_selection(queue)
        @queues.unshift(@queues.pop) if @queues.last == queue
      end

      # Raises a queue's weight by one, up to its configured weight
      #
      # Must be called with the mutex held.
      #
      # @param queue [String] the queue name
      # @return [void]
      def increase_weight(queue)
        maximum_weight = maximum_queue_weight(queue)
        current_weight = current_queue_weight(queue)

        return unless maximum_weight > current_weight

        logger.info { "Increasing #{queue} weight to #{current_weight + 1}, max: #{maximum_weight}" }
        @queues << queue
      end

      # Pauses a queue by removing it from active rotation
      #
      # @param queue [String] the queue name to pause
      # @return [void]
      def pause(queue)
        return unless @queues.delete(queue)

        @paused_queues << [Time.now + delay, queue]
        logger.debug "Paused #{queue}"
      end

      # Unpauses the first queue whose delay has expired.
      #
      # Scans for any expired entry rather than only the head of the list: message_processed marks a queue ready by setting
      # its time to the epoch, and that entry may sit behind an earlier-paused queue that is still paused - checking only the
      # head would leave the ready queue stuck.
      #
      # The backoff expiring is a state change of its own, so the queue moves on to a new poll id: the empty answer that
      # caused the pause must not apply again now that the backoff has passed.
      #
      # @return [void]
      def unpause_queues
        index = @paused_queues.index { |time, _name| time <= Time.now }
        return if index.nil?

        paused = @paused_queues.delete_at(index)
        @queues << paused[1]
        next_poll_id(paused[1])
        logger.debug "Unpaused #{paused[1]}"
      end

      # Returns the current weight of a queue in the active rotation
      #
      # @param queue [String] the queue name
      # @return [Integer] the current weight
      def current_queue_weight(queue)
        queue_weight(@queues, queue)
      end

      # Returns the maximum configured weight of a queue
      #
      # @param queue [String] the queue name
      # @return [Integer] the maximum weight
      def maximum_queue_weight(queue)
        queue_weight(@initial_queues, queue)
      end

      # Counts how many times a queue appears in the given array
      #
      # @param queues [Array<String>] the array to count in
      # @param queue [String] the queue name to count
      # @return [Integer] the count
      def queue_weight(queues, queue)
        queues.count { |q| q == queue }
      end
    end
  end
end
