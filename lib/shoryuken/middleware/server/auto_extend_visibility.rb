# frozen_string_literal: true

module Shoryuken
  module Middleware
    module Server
      # Middleware that automatically extends message visibility timeout during processing.
      # Prevents messages from becoming visible to other consumers while still being processed.
      class AutoExtendVisibility
        include Util

        # Number of seconds before timeout to extend visibility
        EXTEND_UPFRONT_SECONDS = 5

        # SQS's maximum visibility timeout: a message can stay in flight for at most 12 hours
        # from when its current receipt was received, no matter how often it is extended
        MAX_VISIBILITY_TIMEOUT = 43_200

        # Batch worker classes already warned that visibility auto extension is skipped for them
        BATCH_WARNED = Helpers::AtomicHash.new
        private_constant :BATCH_WARNED

        # Processes a message with automatic visibility timeout extension
        #
        # @param worker [Object] the worker instance
        # @param queue [String] the queue name
        # @param sqs_msg [Shoryuken::Message, Array<Shoryuken::Message>] the message or batch
        # @param body [Object] the parsed message body
        # @yield continues to the next middleware in the chain
        # @return [void]
        def call(worker, queue, sqs_msg, body)
          return yield unless worker.class.auto_visibility_timeout?

          if sqs_msg.is_a?(Array)
            warn_batch_unsupported(worker.class)
            return yield
          end

          timer = auto_visibility_timer(worker, queue, sqs_msg, body)
          yield
        ensure
          timer&.kill
        end

        private

        # Helper class for extending message visibility
        class MessageVisibilityExtender
          include Util

          # Creates a timer task that extends message visibility
          #
          # @param _worker [Object] the worker instance (unused)
          # @param queue [String] the queue name
          # @param sqs_msg [Shoryuken::Message] the message
          # @param _body [Object] the parsed message body (unused)
          # @return [Shoryuken::Helpers::TimerTask] the timer task
          def auto_extend(_worker, queue, sqs_msg, _body)
            visibility_timeout = message_visibility_timeout(queue)
            execution_interval = extension_interval(visibility_timeout)

            unless execution_interval
              logger.warn do
                "Not auto-extending visibility for #{queue}/#{sqs_msg.message_id}: visibility " \
                "timeout (#{visibility_timeout}s) is too short. Increase it to use auto_visibility_timeout."
              end
              return nil
            end

            # The receipt's visibility clock started when SQS delivered the message, but it may
            # have waited in the local dispatch queue since. Anchor the first extension to the
            # time left on the receipt, not to the full timeout, or it would fire after expiry.
            initial_interval = extension_interval(remaining_visibility(visibility_timeout, sqs_msg))

            unless initial_interval
              logger.warn do
                "Not auto-extending visibility for #{queue}/#{sqs_msg.message_id}: its visibility " \
                "timeout (#{visibility_timeout}s) already expired while the message waited locally."
              end
              return nil
            end

            timer = nil
            timer = Shoryuken::Helpers::TimerTask.new(
              execution_interval: execution_interval,
              initial_interval: initial_interval
            ) do
              if (timeout = extension_timeout(visibility_timeout, sqs_msg))
                logger.debug do
                  "Extending message #{queue}/#{sqs_msg.message_id} visibility timeout by #{timeout}s"
                end

                sqs_msg.change_visibility(visibility_timeout: timeout)
              else
                # The receipt reached SQS's 12-hour in-flight ceiling: any further extension is
                # rejected by the server, so stop the timer instead of computing one.
                logger.warn do
                  "Not auto-extending visibility for #{queue}/#{sqs_msg.message_id}: the message reached " \
                  "the 12-hour SQS in-flight limit. Delete it or let it expire."
                end
                timer&.kill
              end
            rescue => e
              logger.error do
                "Could not auto extend the message #{queue}/#{sqs_msg.message_id} visibility timeout. Error: #{e.message}"
              end
            end
            timer
          end

          private

          # Returns the visibility timeout that applies to a message of the queue: the receive-time visibility_timeout
          # when it is set, else the queue's VisibilityTimeout attribute
          #
          # @param queue [String] the queue name
          # @return [Integer] the visibility timeout in seconds
          def message_visibility_timeout(queue)
            Shoryuken.receive_message_opts_for(queue, queue_group(queue))[:visibility_timeout] ||
              Shoryuken::Client.queues(queue).visibility_timeout
          end

          # Finds the group a queue belongs to, whose receive options apply when the queue has none of its own
          #
          # @param queue [String] the queue name
          # @return [String, nil] the name of the group the queue belongs to
          def queue_group(queue)
            Shoryuken.groups.find { |_group, options| options[:queues].include?(queue) }&.first
          end

          # Returns how much of the receipt's visibility timeout is still left. Without a known
          # receipt time (e.g. an inline message) the full timeout is assumed to remain.
          #
          # @param visibility_timeout [Integer] the message's visibility timeout in seconds
          # @param sqs_msg [Shoryuken::Message] the message
          # @return [Float] the remaining visibility in seconds
          def remaining_visibility(visibility_timeout, sqs_msg)
            received_at = sqs_msg.received_at if sqs_msg.respond_to?(:received_at)
            return visibility_timeout unless received_at

            visibility_timeout - (Time.now - received_at)
          end

          # Returns the timeout to ask SQS for on the next extension, capped so the receipt's total
          # in-flight time stays within SQS's 12-hour ceiling (with a one-second safety margin, as a
          # request computed at the exact limit can arrive late and be rejected). Returns nil when the
          # ceiling is already reached and no acceptable extension remains.
          #
          # @param visibility_timeout [Integer] the message's visibility timeout in seconds
          # @param sqs_msg [Shoryuken::Message] the message
          # @return [Integer, nil] the visibility timeout to set, or nil when none is acceptable
          def extension_timeout(visibility_timeout, sqs_msg)
            received_at = sqs_msg.received_at if sqs_msg.respond_to?(:received_at)
            return visibility_timeout unless received_at

            max_timeout = MAX_VISIBILITY_TIMEOUT - (Time.now - received_at).ceil - 1
            return nil if max_timeout <= 0

            [visibility_timeout, max_timeout].min
          end

          # Returns a positive interval at which to re-extend the message's visibility before it expires, or nil when the
          # visibility timeout is too short to schedule one. Normally this is EXTEND_UPFRONT_SECONDS before expiry, but for
          # short timeouts (<= EXTEND_UPFRONT_SECONDS) it falls back to half the timeout so the timer still fires in time
          # instead of TimerTask raising on a non-positive interval.
          #
          # @param visibility_timeout [Integer, Float] the (remaining) visibility timeout in seconds
          # @return [Float, Integer, nil] the execution interval, or nil if too short
          def extension_interval(visibility_timeout)
            return nil if visibility_timeout <= 0

            interval = visibility_timeout - EXTEND_UPFRONT_SECONDS
            interval = visibility_timeout / 2.0 if interval <= 0
            interval
          end
        end

        # Logs once per worker class that its batches do not get their visibility auto extended
        #
        # @param worker_class [Class] the batch worker class
        # @return [void]
        def warn_batch_unsupported(worker_class)
          # Keyed by name: a code reload defines a new class object for the same worker
          key = worker_class.name || worker_class
          return if BATCH_WARNED[key]

          BATCH_WARNED[key] = true
          logger.warn { "Auto extend visibility isn't supported for batch workers, skipping it for #{worker_class}" }
        end

        # Creates and starts a visibility extension timer
        #
        # @param worker [Object] the worker instance
        # @param queue [String] the queue name
        # @param sqs_msg [Shoryuken::Message] the message
        # @param body [Object] the parsed message body
        # @return [Shoryuken::Helpers::TimerTask] the started timer
        def auto_visibility_timer(worker, queue, sqs_msg, body)
          timer = MessageVisibilityExtender.new.auto_extend(worker, queue, sqs_msg, body)
          timer&.execute
          timer
        end
      end
    end
  end
end
