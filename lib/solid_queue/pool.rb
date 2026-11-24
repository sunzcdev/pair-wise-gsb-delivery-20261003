# frozen_string_literal: true

module SolidQueue
  class Pool
    include AppExecutor

    def self.build(type:, size:, on_idle: nil, on_unrecoverable_error: nil)
      SolidQueue.const_get("#{type.to_s.camelize}Pool").new(
        size,
        on_idle: on_idle,
        on_unrecoverable_error: on_unrecoverable_error
      )
    end

    attr_reader :size

    def initialize(size, on_idle: nil, on_unrecoverable_error: nil)
      @size = size
      @on_idle = on_idle
      @on_unrecoverable_error = on_unrecoverable_error
      @available_capacity = size
      @mutex = Mutex.new
      # Execution objects are used as identity keys. Active Record overrides
      # equality, and two different claim instances for the same row must never
      # share lifecycle state.
      @execution_states = {}.compare_by_identity
    end

    def type
      self.class.name.demodulize.delete_suffix("Pool").underscore.to_sym
    end

    def post(execution)
      reserve_capacity!
      track_execution(execution)

      begin
        schedule(execution)
      rescue Exception
        untrack_execution(execution)
        restore_capacity
        raise
      end
    end

    # Marks queued executions as cancelled before they can enter +perform+.
    # Executions already performing are never selected. Cancelled executions
    # release their pool capacity immediately; their place in the subclass's
    # scheduler is harmlessly skipped when it reaches the lifecycle gate.
    def cancel_pending_executions(&filter)
      candidates = synchronize do
        execution_states.filter_map { |execution, state| execution if state == :queued }
      end

      candidates = candidates.select(&filter) if filter

      cancelled = synchronize do
        candidates.select! { |execution| execution_states[execution] == :queued }
        candidates.each { |execution| execution_states[execution] = :cancelled }
        candidates.dup
      end

      cancelled.each { restore_capacity }
      cancelled
    end

    alias release_pending_executions cancel_pending_executions
    alias reject_pending_executions cancel_pending_executions

    def available_capacity
      mutex.synchronize { @available_capacity }
    end

    def performing_executions
      synchronize do
        execution_states.filter_map { |execution, state| execution if state == :performing }
      end
    end

    def idle?
      available_capacity.positive?
    end

    private
      attr_reader :mutex, :execution_states, :on_idle, :on_unrecoverable_error

      def synchronize(&block)
        mutex.synchronize(&block)
      end

      def track_execution(execution)
        synchronize { execution_states[execution] = :queued }
      end

      def untrack_execution(execution)
        synchronize { execution_states.delete(execution) }
      end

      # Returns true only for the caller that atomically transitions an admitted
      # execution from queued to performing. A cancelled execution removes its
      # marker here; its capacity was already restored by
      # +cancel_pending_executions+.
      def begin_execution?(execution)
        synchronize do
          state = execution_states[execution]

          if state == :queued
            execution_states[execution] = :performing
            true
          elsif state == :cancelled
            execution_states.delete(execution)
            false
          else
            false
          end
        end
      end

      def finish_execution(execution)
        untrack_execution(execution)
        restore_capacity
      end

      def schedule(execution)
        raise NotImplementedError
      end

      def perform_execution(execution)
        started = begin_execution?(execution)
        return unless started

        wrap_in_app_executor { execution.perform }
      rescue Exception => error
        handle_unrecoverable_error(error)
        handle_thread_error(error)
      ensure
        finish_execution(execution) if started
      end

      def handle_unrecoverable_error(error)
        return unless error.is_a?(Processes::UnrecoverableError)

        # Only signal shutdown — do not join the worker from this pool thread,
        # or wait_for_termination during worker shutdown would deadlock.
        on_unrecoverable_error&.call
      end

      def reserve_capacity!
        mutex.synchronize do
          raise RuntimeError, "Execution pool is at capacity" if @available_capacity <= 0

          @available_capacity -= 1
        end
      end

      def restore_capacity
        should_notify = mutex.synchronize do
          @available_capacity += 1
          @available_capacity.positive?
        end

        on_idle&.call if should_notify
      end
  end
end
