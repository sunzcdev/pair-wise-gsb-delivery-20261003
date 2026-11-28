# frozen_string_literal: true

module Shoryuken
  module Worker
    class InlineExecutor
      # Result returned instead of running the worker when FIFO deduplication
      # suppresses a repeated submission. It stands in for the
      # Aws::SQS::Types::SendMessageResult of the original accepted send: in
      # production SQS accepts the duplicate but never delivers it, so the
      # worker never runs and there is no perform result to return.
      #
      # The scope mirrors {FifoDeduplicator}: the queue, group and deduplication
      # ids that identified the suppressed send, plus when it was observed.
      SuppressedMessage = Struct.new(
        :queue_name,
        :message_group_id,
        :message_deduplication_id,
        :observed_at,
        keyword_init: true
      ) do
        # @return [Boolean] always true - marks inline results SQS would have dropped
        def deduplicated?
          true
        end
      end
    end
  end
end
