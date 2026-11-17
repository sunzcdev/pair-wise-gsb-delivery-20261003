require 'concurrent/synchronization/lockable_object'
require 'concurrent/executor/abstract_executor_service'
require 'concurrent/atomic/event'

module Concurrent
  # Deterministic testing helpers for {Concurrent::TimerTask}. Neither class is
  # part of the public API: they exist so that scheduling behaviour can be
  # exercised against an injectable, manually advanced clock instead of sleeping
  # through real long intervals.
  module TimerTaskTest
    # A virtual monotonic clock with a task queue. Time only moves when
    # {#advance} (or {#run_pending}) is called, so a test can simulate many
    # scheduling cycles instantly and with full determinism.
    #
    # Both one-shot and periodic schedules are supported. Scheduled blocks are
    # invoked when their virtual due time is reached; this class is only
    # responsible for *when* things become due, never for running the user task
    # (the TimerTask still dispatches that to its own executor).
    class VirtualScheduler < Synchronization::LockableObject
      # A single scheduled due-time / block pair.
      class Entry
        attr_reader :block, :period
        attr_accessor :due_at, :cancelled

        def initialize(due_at, period, block)
          @due_at = due_at
          @period = period
          @block = block
          @cancelled = false
        end

        def periodic?
          !@period.nil?
        end
      end

      def initialize(now = 0.0)
        super()
        @now = now.to_f
        @entries = []
      end

      # Current virtual monotonic time.
      #
      # @return [Float]
      def now
        synchronize { @now }
      end

      # Schedule `block` to run `delay` virtual seconds in the future. When
      # `period` is given the entry re-arms itself every `period` virtual
      # seconds after its first due time, carrying a stable handle identity.
      #
      # @return [Handle] a handle whose `#cancel` prevents future firings
      def schedule(delay, period: nil, &block)
        synchronize do
          entry = Entry.new(@now + delay.to_f, period, block)
          @entries << entry
          Handle.new(entry)
        end
      end

      # All live (not cancelled) scheduled entries, soonest first.
      #
      # @return [Array<Entry>]
      def pending
        synchronize { @entries.reject(&:cancelled).sort_by(&:due_at) }
      end

      # Move the virtual clock forward by `seconds`, firing every entry whose
      # due time is reached, in due order. Periodic entries whose period elapses
      # multiple times fire once per due instant, so no ticks are silently
      # merged by the clock itself. Entries scheduled while firing participate
      # on the same time line.
      #
      # @return [Array] the values returned by the fired blocks
      def advance(seconds)
        synchronize { @now += seconds.to_f }
        fire_due
      end

      # Fire every currently due entry without moving the clock. Useful for
      # zero-delay schedules (e.g. `run_now: true`).
      #
      # @return [Array] the values returned by the fired blocks
      def run_pending
        fire_due
      end

      private

      def fire_due
        results = []
        target = synchronize { @now }
        loop do
          due = synchronize do
            entry = @entries.reject(&:cancelled).sort_by(&:due_at).first
            entry && entry.due_at <= target ? entry : nil
          end
          break unless due

          if due.periodic?
            # Re-arm before invoking so a firing that shuts the task down can
            # still cancel the freshly armed entry through its handle.
            synchronize do
              due.due_at += due.period unless due.cancelled
            end
            results << due.block.call
          else
            synchronize { @entries.delete(due) }
            next if due.cancelled
            results << due.block.call
          end
        end
        results
      end

      # Cancellation handle returned by {VirtualScheduler#schedule}.
      class Handle
        def initialize(entry)
          @entry = entry
        end

        def cancel
          @entry.cancelled = true
        end
      end
    end

    # An executor which stores posted jobs instead of running them, letting a
    # test decide exactly when a task execution starts and finishes. Combine it
    # with {VirtualScheduler} to simulate an execution that spans several ticks.
    class ManualExecutor < AbstractExecutorService
      def initialize(_opts = {})
        @stopped = Concurrent::Event.new
        @jobs = Queue.new
      end

      # @!macro executor_service_method_post
      def post(*args, &task)
        raise ArgumentError.new('no block given') unless block_given?
        return false unless running?
        @jobs << [task, args]
        true
      end

      # Number of executions waiting to be performed.
      #
      # @return [Integer]
      def pending_count
        @jobs.size
      end

      # Whether an execution has been posted but not yet performed.
      #
      # @return [Boolean]
      def any_pending?
        !@jobs.empty?
      end

      # Perform one queued execution on the calling thread.
      #
      # @return [Boolean] true when a job was performed, false when idle
      def run_one
        task, args = @jobs.pop(true)
      rescue ThreadError
        false
      else
        task.call(*args)
        true
      end

      # Perform every currently queued execution on the calling thread.
      #
      # @return [Integer] number of executions performed
      def run_all
        count = 0
        count += 1 while run_one
        count
      end

      # @!macro executor_service_method_running_question
      def running?
        !shutdown?
      end

      # @!macro executor_service_method_shuttingdown_question
      def shuttingdown?
        false
      end

      # @!macro executor_service_method_shutdown_question
      def shutdown?
        @stopped.set?
      end

      # @!macro executor_service_method_shutdown
      def shutdown
        @stopped.set
        true
      end
      alias_method :kill, :shutdown

      # @!macro executor_service_method_wait_for_termination
      def wait_for_termination(timeout = nil)
        @stopped.wait(timeout)
      end
    end
  end
end
