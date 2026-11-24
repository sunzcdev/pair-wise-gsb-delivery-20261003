# frozen_string_literal: true

module SolidQueue
  class Job
    # Outcome of a bulk enqueue. Holds the jobs that were actually written
    # (#enqueued) separately from the ones that already existed for the same
    # business key (#duplicated), so callers can tell new work apart from
    # retried requests.
    class EnqueueAllResult
      attr_reader :enqueued, :duplicated

      def initialize(enqueued:, duplicated:)
        @enqueued = enqueued
        @duplicated = duplicated
      end

      # Backwards-compatible size of the whole successful enqueue, matching
      # the integer SolidQueue::Job.enqueue_all used to return.
      def size
        enqueued.size + duplicated.size
      end

      # Adapters and instrumentation historically received an integer count.
      # Keep behaving like one where a Numeric is expected.
      def to_i
        size
      end
      alias_method :to_int, :to_i
    end
  end
end
