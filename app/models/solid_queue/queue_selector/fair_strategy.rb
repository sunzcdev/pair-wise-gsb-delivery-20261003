# frozen_string_literal: true

module SolidQueue
  class QueueSelector
    # Fair strategy: keep queue-internal order and the worker claim limit, but
    # rotate the queue each round so a queue further down the configured order
    # is guaranteed a chance to be served within `window` claim rounds, even when
    # the queues ahead of it never run dry.
    #
    # The rotation cursor is shared by every claim made through the same selector
    # (one per worker), and it advances per round, not per matched queue entry:
    # duplicate or wildcard declarations that resolve to the same queue are
    # deduplicated first by the QueueSelector, so they never get counted twice.
    #
    # With `n` distinct queues, over any `min(window, n)` consecutive rounds each
    # queue occupies the leading position exactly once. The default window of 2
    # alternates the first two queues, which is enough to protect the immediate
    # successor of a permanently busy front queue.
    class FairStrategy
      DEFAULT_WINDOW = 2

      attr_reader :window

      def initialize(window: DEFAULT_WINDOW, **)
        window = Integer(window)
        raise ArgumentError, "Queue fair strategy window must be a positive number" unless window.positive?

        @window = window
        @mutex = Mutex.new
        @offset = 0
      end

      def order(queue_names)
        return queue_names if queue_names.size <= 1

        @mutex.synchronize do
          rotated = queue_names.rotate(@offset % queue_names.size)
          @offset = (@offset + 1) % [ window, queue_names.size ].min
          rotated
        end
      end
    end
  end
end
