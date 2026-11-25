# frozen_string_literal: true

module Shoryuken
  module Middleware
    module Server
      # Middleware that implements exponential backoff retry for failed messages. When a job fails, the message visibility
      # timeout is adjusted based on configured retry intervals.
      class ExponentialBackoffRetry
        include Util

        # SQS's maximum visibility timeout: a message can stay in flight for at most 12 hours
        # from when its current receipt was received
        MAX_VISIBILITY_TIMEOUT = 43_200

        # Batch worker classes already warned that exponential backoff is skipped for them
        BATCH_WARNED = Helpers::AtomicHash.new
        private_constant :BATCH_WARNED

        # Processes a message with exponential backoff retry on failure
        #
        # @param worker [Object] the worker instance
        # @param _queue [String] the queue name (unused)
        # @param sqs_msg [Shoryuken::Message, Array<Shoryuken::Message>] the message or batch
        # @param _body [Object] the parsed message body (unused)
        # @yield continues to the next middleware in the chain
        # @return [void]
        # @raise [StandardError] re-raises the original exception if retry intervals are not configured,
        #   if retry limit is exceeded, or if the exception is classified as non-retryable
        def call(worker, _queue, sqs_msg, _body)
          return yield unless worker.class.exponential_backoff?

          if sqs_msg.is_a?(Array)
            warn_batch_unsupported(worker.class)
            return yield
          end

          # SQS's 12-hour in-flight ceiling runs from when the message was received, not from
          # when processing started - the message may have waited in the local dispatch queue
          # first. Anchor the cap to the receipt so a long local wait doesn't produce a
          # visibility timeout the server will reject.
          received_at = sqs_msg.received_at if sqs_msg.respond_to?(:received_at)
          received_at ||= Time.now

          # Scope the rescue to the single-message yield only. A method-level
          # rescue would also catch the batch `return yield` above, then route the
          # Array into handle_failure (which calls #attributes/#message_id on it),
          # raising a NoMethodError that masks the original worker error.
          begin
            yield
          rescue => e
            worker_options = worker.class.get_shoryuken_options
            retry_intervals = worker_options['retry_intervals']

            # Non-retryable exceptions must not be backoff retried; re-raise so the
            # NonRetryableException middleware can delete the message immediately.
            raise if NonRetryableException.non_retryable?(e, worker_options['non_retryable_exceptions'])

            if retry_intervals.nil? || !handle_failure(sqs_msg, received_at, retry_intervals)
              # Re-raise the exception if the job is not going to be exponential backoff retried.
              # This allows custom middleware (like exception notifiers) to be aware of the unhandled failure.
              raise
            end

            logger.warn { "Message #{sqs_msg.message_id} will attempt retry due to error: #{e.message}" }
            # since we didn't raise, lets log the backtrace for debugging purposes.
            logger.debug { e.backtrace.join("\n") } unless e.backtrace.nil?
          end
        end

        private

        # Logs once per worker class that its batches are not backoff retried
        #
        # @param worker_class [Class] the batch worker class
        # @return [void]
        def warn_batch_unsupported(worker_class)
          # Keyed by name: a code reload defines a new class object for the same worker
          key = worker_class.name || worker_class
          return if BATCH_WARNED[key]

          BATCH_WARNED[key] = true
          logger.warn { "Exponential backoff isn't supported for batch workers, skipping it for #{worker_class}" }
        end

        # Gets the retry interval for a given attempt number
        #
        # @param retry_intervals [Array<Integer>, #call] the configured intervals or callable
        # @param attempts [Integer] the current attempt number
        # @return [Integer, nil] the interval in seconds or nil
        def get_interval(retry_intervals, attempts)
          return retry_intervals.call(attempts) if retry_intervals.respond_to?(:call)

          if attempts <= (retry_intervals = Array(retry_intervals)).size
            retry_intervals[attempts - 1]
          else
            retry_intervals.last
          end
        end

        # Calculates the next visibility timeout capped at SQS maximum
        #
        # @param interval [Integer] the desired interval
        # @param received_at [Time] when the message was received (the 12h ceiling runs from here)
        # @return [Integer] the capped visibility timeout (never negative)
        def next_visibility_timeout(interval, received_at)
          max_timeout = MAX_VISIBILITY_TIMEOUT - (Time.now - received_at).ceil - 1
          interval = max_timeout if interval > max_timeout
          # Never go negative: a message whose receipt is older than the 12h SQS ceiling
          # drives max_timeout below zero, and change_message_visibility rejects
          # a negative timeout. Clamp to 0 (retry as soon as possible) instead.
          [interval.to_i, 0].max
        end

        # Handles a message failure by adjusting visibility timeout
        #
        # @param sqs_msg [Shoryuken::Message] the failed message
        # @param received_at [Time] when the message was received
        # @param retry_intervals [Array<Integer>, #call] the configured intervals
        # @return [Boolean] true if retry was scheduled, false otherwise
        def handle_failure(sqs_msg, received_at, retry_intervals)
          receive_count = sqs_msg.attributes['ApproximateReceiveCount'].to_i

          begin
            interval = get_interval(retry_intervals, receive_count)
          rescue => e
            logger.warn { "retry_intervals raised for #{sqs_msg.message_id}: #{e.class}: #{e.message}" }
            return false
          end

          return false unless interval

          sqs_msg.change_visibility(visibility_timeout: next_visibility_timeout(interval.to_i, received_at))

          logger.info { "Message #{sqs_msg.message_id} failed, will be retried in #{interval} seconds" }

          true
        rescue => e
          # Rescheduling itself failed (e.g. an expired receipt handle, or a
          # transient SQS error). Don't let that exception escape and mask the
          # original worker failure - log it and report "not retried" so the
          # caller re-raises the original error and the message falls back to the
          # queue's default visibility timeout.
          logger.warn { "Failed to set backoff visibility timeout for #{sqs_msg.message_id}: #{e.message}" }
          false
        end
      end
    end
  end
end
