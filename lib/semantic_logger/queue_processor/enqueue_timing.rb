module SemanticLogger
  class QueueProcessor
    # Internal: records when each log entry was added to the queue of one queue processor, and turns
    # that into how long the entry waited before its worker thread took it off the queue, in
    # milliseconds (see Log#queue_duration).
    #
    # It belongs to a single QueueProcessor, so the wait it measures is the time on that processor's
    # queue alone. The same Log can be queued by several processors, such as one per async appender,
    # and each one keeps its own reading rather than overwriting another's. The measured duration
    # (Log#duration) and the event time (Log#time) are never touched: the queue wait is recorded
    # separately, and only on the entry just before it is written, on the worker thread.
    #
    # A reading is removed as soon as the entry is taken off the queue, so an entry that is never
    # taken cannot keep its reading in memory. After a fork the child starts with none, since it
    # replaces the queue it inherited (see QueueProcessor#renew_after_fork).
    #
    # The clock is the same monotonic clock SemanticLogger measures block durations with, injectable
    # so a test can drive it deterministically.
    #
    # Readings are keyed by the entry's object id rather than the entry itself: a Log does not define
    # #hash or #eql?, so two different Log instances can collide as plain Hash keys, each overwriting
    # the other's reading. The object id uniquely identifies one object for the life of the process.
    class EnqueueTiming
      # Returns the milliseconds between two monotonic readings given in seconds.
      def self.to_milliseconds(seconds) = 1_000.0 * seconds

      def initialize(clock: -> { SemanticLogger.monotonic_time })
        @clock   = clock
        @queued  = {}
      end

      # Records that the entry has just been added to the queue. Called only once it is on it, so a
      # message dropped because the queue was full, or a request such as a flush, is never recorded.
      def mark(log)
        @queued[log.object_id] = @clock.call
        self
      end

      # Returns [Float] how many milliseconds the entry waited on this queue, and records it on the
      # entry as its queue duration, removing its reading. Returns nil for an entry it has no reading
      # for, such as one queued before a fork replaced this timing, or queued directly in a test that
      # bypasses #log, leaving any queue duration the entry already has untouched.
      def stamp(log)
        enqueued = @queued.delete(log.object_id)
        return unless enqueued

        log.queue_duration = self.class.to_milliseconds(@clock.call - enqueued)
      end

      # Forgets every reading, as after a fork, when the entries they timed belong to the parent
      # process's queue rather than the empty one the child starts with.
      def forget
        @queued.clear
        self
      end
    end
  end
end
