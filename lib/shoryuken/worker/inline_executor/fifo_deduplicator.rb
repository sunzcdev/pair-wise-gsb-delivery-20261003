# frozen_string_literal: true

module Shoryuken
  module Worker
    class InlineExecutor
      # Thread-safe, time-bounded in-memory mirror of the FIFO deduplication SQS
      # performs server-side. The {InlineExecutor} consults it before running a
      # worker so CI/local runs suppress the same re-executions a FIFO queue
      # would drop in production instead of executing them twice.
      #
      # An observed key is the triple SQS actually scopes dedup on for a given
      # send: the queue the message is submitted to, its message group id, and
      # its message deduplication id. Each entry is remembered for a bounded
      # window (SQS's default is five minutes), measured with a monotonic clock
      # so changing the system clock cannot resurrect or expire entries early.
      class FifoDeduplicator
        # SQS retains a message deduplication id for five minutes (the queue's
        # DeduplicationDuration, whose minimum and default is 300 seconds).
        DEFAULT_WINDOW = 5 * 60

        # @return [Numeric] seconds an observed key stays suppressed
        attr_reader :window

        # @param window [Numeric] seconds an observed key stays suppressed
        # @param clock [#call] monotonic time source, injectable for testing
        def initialize(window: DEFAULT_WINDOW, clock: -> { Process.clock_gettime(Process::CLOCK_MONOTONIC) })
          raise ArgumentError, 'window must be positive' unless window.positive?

          @window = window
          @clock = clock
          @observed = {}
          @mutex = Mutex.new
        end

        # Atomically records the scope for a submitted FIFO message and reports
        # whether SQS would have suppressed it. A first submission is recorded
        # and allowed through; a repeat within the window is rejected. Entries
        # older than the window have expired, so they are treated as first
        # submissions again and re-recorded.
        #
        # @param queue_name [String] queue the message is actually submitted to
        # @param message_group_id [String] FIFO message group id
        # @param message_deduplication_id [String] FIFO message deduplication id
        # @return [Boolean] true when the send is a duplicate and must be suppressed
        def duplicate?(queue_name, message_group_id, message_deduplication_id)
          key = [queue_name, message_group_id, message_deduplication_id]
          now = @clock.call

          @mutex.synchronize do
            prune_expired(now)

            if @observed.key?(key)
              true
            else
              @observed[key] = now
              false
            end
          end
        end

        # @return [Integer] number of unexpired keys currently observed
        def size
          @mutex.synchronize do
            prune_expired(@clock.call)
            @observed.size
          end
        end

        # @return [Array<Array(String, String, String)>] the unexpired observed
        #   scopes as [queue name, message group id, deduplication id] triples
        def keys
          @mutex.synchronize do
            prune_expired(@clock.call)
            # dup each triple so callers cannot mutate the stored scope arrays
            @observed.keys.map(&:dup)
          end
        end

        # Forgets every observed key, restoring first-submission behavior.
        #
        # @return [void]
        def reset
          @mutex.synchronize { @observed.clear }
        end

        private

        # Drops entries whose window has elapsed. Must be called under the mutex.
        #
        # @param now [Numeric] the current monotonic time
        # @return [void]
        def prune_expired(now)
          @observed.delete_if { |_, observed_at| (now - observed_at) >= @window }
        end
      end
    end
  end
end
