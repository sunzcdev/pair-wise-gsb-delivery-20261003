# frozen_string_literal: true

module Shoryuken
  module Helpers
    # A thread-safe timer task implementation.
    # Drop-in replacement for Concurrent::TimerTask without external dependencies.
    class TimerTask
      # Initializes a new TimerTask
      #
      # @param execution_interval [Float] interval in seconds between task executions
      # @param initial_interval [Float, nil] interval in seconds before the first execution;
      #   defaults to execution_interval. Useful when the schedule is anchored to an earlier
      #   point in time (e.g. a message receipt) so the first run comes sooner than the rest.
      # @param task [Proc] the task to execute on each interval (provided as a block)
      # @return [TimerTask] a new TimerTask instance
      # @raise [ArgumentError] if no block is provided or an interval is not positive
      # @yield the task to execute on each interval
      def initialize(execution_interval:, initial_interval: nil, &task)
        raise ArgumentError, 'A block must be provided' unless block_given?

        @execution_interval = Float(execution_interval)
        raise ArgumentError, 'execution_interval must be positive' if @execution_interval <= 0

        @initial_interval = Float(initial_interval || execution_interval)
        raise ArgumentError, 'initial_interval must be positive' if @initial_interval <= 0

        @task = task
        @mutex = Mutex.new
        @thread = nil
        @running = false
        @killed = false
      end

      # Starts the timer task execution
      #
      # @return [TimerTask] self for method chaining
      def execute
        @mutex.synchronize do
          return self if @running || @killed

          @running = true
          @thread = Thread.new { run_timer_loop }
        end
        self
      end

      # Stops and kills the timer task
      #
      # @return [Boolean] true if killed, false if already killed
      def kill
        thread_to_kill = nil

        @mutex.synchronize do
          return false if @killed

          @killed = true
          @running = false
          thread_to_kill = @thread
        end

        # Kill the thread AFTER releasing the mutex. The timer loop's ensure
        # block calls @mutex.synchronize to clear @running; killing the thread
        # while holding that mutex deadlocks on Ruby 3.2, where Thread#kill
        # yields the GVL to the killed thread for cleanup before returning.
        thread_to_kill&.kill if thread_to_kill&.alive?
        true
      end

      private

      # Runs the timer loop in a separate thread
      #
      # @return [void]
      def run_timer_loop
        # The timer thread inherits the priority of the thread that called
        # #execute. Shoryuken runs workers at a lowered priority
        # (Shoryuken.thread_priority, default -1) and starts the
        # auto-visibility-extension timer from inside that worker thread, so the
        # timer would otherwise inherit -1. A latency-sensitive timer must not
        # run below normal priority: under CPU contention a delayed extension can
        # miss the visibility timeout and let the message be redelivered (double
        # processed). Reset to normal priority.
        Thread.current.priority = 0

        interval = @initial_interval
        until @killed
          sleep(interval)
          interval = @execution_interval
          break if @killed

          begin
            @task.call
          rescue => e
            # Log the error but continue running
            # This matches the behavior of Concurrent::TimerTask
            warn "TimerTask execution error: #{e.message}"
            warn e.backtrace.join("\n") if e.backtrace
          end
        end
      ensure
        # No @mutex here: #kill may kill this thread while it waits for the lock, and a thread killed while waiting on
        # a Mutex can consume the unlock wakeup and leave the other waiters (concurrent #kill callers) asleep forever.
        @running = false
      end
    end
  end
end
