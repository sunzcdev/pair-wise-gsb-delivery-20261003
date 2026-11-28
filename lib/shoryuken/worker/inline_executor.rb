# frozen_string_literal: true

module Shoryuken
  module Worker
    # Executor that processes jobs synchronously in the current thread.
    # Useful for testing and development environments.
    #
    # By default it runs every submission, including repeated FIFO sends that a
    # real FIFO queue would silently drop. {.enable_fifo_deduplication!} opts in
    # to an optional, time-bounded observation of the FIFO deduplication SQS
    # performs in production, so CI runs suppress the same re-executions instead
    # of accepting them locally. Observation is off unless explicitly enabled, so
    # ordinary (non-FIFO) inline jobs behave exactly as before.
    class InlineExecutor
      class << self
        # @return [FifoDeduplicator, nil] the active deduplication observation,
        #   or nil when FIFO deduplication is disabled (the default)
        attr_reader :fifo_deduplicator

        # Enables FIFO deduplication observation. While enabled, a submission
        # carrying a +message_deduplication_id+ is recorded under the queue it is
        # actually submitted to, its message group id and its deduplication id;
        # repeating the same scope within +window+ seconds is suppressed and
        # returns a {SuppressedMessage} instead of running the worker. This
        # mirrors SQS, which accepts the duplicate send but never delivers it.
        #
        # @param window [Numeric] seconds a scope stays suppressed
        #   (defaults to SQS's five-minute DeduplicationDuration)
        # @param clock [#call, nil] monotonic time source, injectable for testing
        # @return [FifoDeduplicator] the newly installed observation store
        def enable_fifo_deduplication!(window: FifoDeduplicator::DEFAULT_WINDOW, clock: nil)
          @fifo_deduplicator =
            if clock
              FifoDeduplicator.new(window: window, clock: clock)
            else
              FifoDeduplicator.new(window: window)
            end
        end

        # Disables FIFO deduplication observation and forgets every observed
        # scope, restoring the default behavior of running every submission.
        #
        # @return [void]
        def disable_fifo_deduplication!
          @fifo_deduplicator = nil
        end

        # @return [Boolean] whether FIFO deduplication observation is enabled
        def fifo_deduplication?
          !@fifo_deduplicator.nil?
        end

        # Forgets every observed scope without disabling observation, so the
        # next submission of each scope is executed again. Intended for test
        # isolation between examples.
        #
        # @return [void]
        def reset_fifo_deduplication!
          @fifo_deduplicator&.reset
        end

        # @return [Array<Array(String, String, String)>] the unexpired observed
        #   scopes as [queue name, message group id, deduplication id] triples,
        #   one per scope whose first submission was actually executed
        def observed_deduplication_keys
          @fifo_deduplicator ? @fifo_deduplicator.keys : []
        end

        # @return [Integer] number of unexpired scopes whose first submission was
        #   actually executed (and that still suppress repeats)
        def observed_deduplication_count
          @fifo_deduplicator ? @fifo_deduplicator.size : 0
        end

        # Processes a job synchronously in the current thread
        #
        # @param worker_class [Class] the worker class that will process the message
        # @param body [Object] the message body
        # @param options [Hash] inline execution options
        # @option options [String] :queue override the default queue name
        # @option options [Hash] :message_attributes custom message attributes
        # @option options [String] :message_group_id FIFO message group id
        # @option options [String] :message_deduplication_id FIFO deduplication id
        # @return [Object, SuppressedMessage] the result of the worker's perform
        #   method, or a {SuppressedMessage} when FIFO deduplication drops the send
        def perform_async(worker_class, body, options = {})
          # Work on a copy so reused/shared options hashes aren't mutated across
          # calls (deleting :queue would reroute later jobs to the default
          # queue). Merge - rather than mutate - the nested message_attributes.
          options = options.dup
          body = JSON.dump(body) if body.is_a?(Hash)
          queue_name = options.delete(:queue) || worker_class.get_shoryuken_options['queue']

          suppression = fifo_suppression(queue_name, options)
          return suppression if suppression

          message_attributes = (options.delete(:message_attributes) || {}).merge(
            'shoryuken_class' => {
              string_value: worker_class.to_s,
              data_type: 'String'
            }
          )

          sqs_msg = InlineMessage.new(
            body: body,
            attributes: nil,
            md5_of_body: nil,
            md5_of_message_attributes: nil,
            message_attributes: message_attributes,
            message_id: nil,
            receipt_handle: nil,
            delete: nil,
            queue_name: queue_name
          )

          call(worker_class, sqs_msg)
        end

        # Processes a job synchronously, ignoring the delay interval
        #
        # @param worker_class [Class] the worker class that will process the message
        # @param _interval [Integer, Float] ignored for inline execution; it does
        #   not move the FIFO deduplication window, which only advances with time
        # @param body [Object] the message body
        # @param options [Hash] inline execution options
        # @option options [String] :queue override the default queue name
        # @option options [Hash] :message_attributes custom message attributes
        # @option options [String] :message_group_id FIFO message group id
        # @option options [String] :message_deduplication_id FIFO deduplication id
        # @return [Object, SuppressedMessage] the result of the worker's perform
        #   method, or a {SuppressedMessage} when FIFO deduplication drops the send
        def perform_in(worker_class, _interval, body, options = {})
          worker_class.perform_async(body, options)
        end

        private

        # Returns a SuppressedMessage when FIFO deduplication observation is
        # enabled and this submission repeats an observed scope inside the
        # window, otherwise records the scope (if observed) and returns nil so
        # the worker runs. Submissions without a message_deduplication_id are
        # never observed: an explicit id is the FIFO dedup key production uses.
        #
        # @param queue_name [String, nil] the queue the message is submitted to
        # @param options [Hash] the caller-supplied inline options
        # @return [SuppressedMessage, nil]
        def fifo_suppression(queue_name, options)
          deduplicator = @fifo_deduplicator
          deduplication_id = options[:message_deduplication_id]
          return nil unless deduplicator && deduplication_id

          queue = queue_name.to_s
          group_id = options[:message_group_id] || Shoryuken::Queue::MESSAGE_GROUP_ID

          return nil unless deduplicator.duplicate?(queue, group_id, deduplication_id)

          SuppressedMessage.new(
            queue_name: queue,
            message_group_id: group_id,
            message_deduplication_id: deduplication_id,
            observed_at: Time.now
          )
        end

        # Instantiates and calls the worker
        #
        # @param worker_class [Class] the worker class
        # @param sqs_msg [Shoryuken::InlineMessage] the message
        # @return [Object] the result of the worker's perform method
        def call(worker_class, sqs_msg)
          parsed_body = BodyParser.parse(worker_class, sqs_msg)
          batch = worker_class.get_shoryuken_options['batch']
          args = batch ? [[sqs_msg], [parsed_body]] : [sqs_msg, parsed_body]
          worker_class.new.perform(*args)
        end
      end
    end
  end
end
