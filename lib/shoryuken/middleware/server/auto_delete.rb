# frozen_string_literal: true

module Shoryuken
  module Middleware
    module Server
      # Middleware that automatically deletes messages after successful processing.
      # Only deletes messages when the worker has auto_delete enabled.
      #
      # SQS can answer delete_message_batch with a successful HTTP response while listing
      # individual receipts in +failed+. The delete is settled per receipt (see
      # {Shoryuken::DeletionResult}): only failures that clearly name a temporary condition
      # are deleted again, in a follow-up batch containing just those receipts. Expired or
      # invalid receipts (e.g. +ReceiptHandleIsInvalid+) are not retried.
      #
      # The retry loop only wraps the deletion call - the worker block has already run to
      # completion and is never invoked again.
      class AutoDelete
        # Processes a message and deletes it if auto_delete is enabled
        #
        # @param worker [Object] the worker instance
        # @param queue [String] the queue name
        # @param sqs_msg [Shoryuken::Message, Array<Shoryuken::Message>] the message or batch
        # @param _body [Object] the parsed message body (unused)
        # @yield continues to the next middleware in the chain
        # @return [Shoryuken::DeletionResult, Object] the grouped deletion settlement when the queue
        #   exposes the settlement API; otherwise the raw queue +#delete_messages+ result
        def call(worker, queue, sqs_msg, _body)
          yield

          return unless worker.class.auto_delete?

          entries = [sqs_msg].flatten.map.with_index { |message, i| { id: i.to_s, receipt_handle: message.receipt_handle } }

          delete_settlement(Shoryuken::Client.queues(queue), entries)
        end

        private

        # Deletes the entries with targeted retries for clearly transient failures.
        #
        # Real {Shoryuken::Queue} instances expose {Queue#delete_messages_settlement}, which owns
        # the whole retry loop and returns the grouped settlement. Test doubles and alternate
        # queue adapters that only implement the historical +#delete_messages+ boolean seam go
        # through a compatible loop here when their return value carries per-entry results.
        #
        # @param queue_client [Object] the queue-like deletion target
        # @param entries [Array<Hash>] the batch entries
        # @return [Shoryuken::DeletionResult, Object] the settlement or the raw seam result
        def delete_settlement(queue_client, entries)
          if queue_client.respond_to?(:delete_messages_settlement)
            return queue_client.delete_messages_settlement(entries: entries)
          end

          delete_via_boolean_seam(queue_client, entries)
        end

        # Drives targeted retries through +#delete_messages+ for queue-like objects without the
        # settlement API. Plain boolean/+nil+ results carry no per-receipt detail, so they are
        # returned untouched (preserving the historical middleware return value).
        #
        # @param queue_client [Object] the queue-like deletion target
        # @param entries [Array<Hash>] the batch entries
        # @return [Shoryuken::DeletionResult, Object]
        def delete_via_boolean_seam(queue_client, entries)
          settlement = DeletionResult.new(entries)
          pending_entries = entries
          attempts = 0
          recordable_seen = false
          raw_result = nil

          loop do
            raw_result = queue_client.delete_messages(entries: pending_entries)

            if DeletionResult.recordable?(raw_result)
              recordable_seen = true
              settlement.record(raw_result)
            end

            attempts += 1
            pending_entries = recordable_seen ? settlement.retry_entries : []

            break if pending_entries.empty? || attempts >= Queue::DELETE_MAX_ATTEMPTS
          end

          recordable_seen ? settlement : raw_result
        end
      end
    end
  end
end
