# frozen_string_literal: true

module ActiveRecord
  module ConnectionAdapters
    class QueryIntent # :nodoc:
      # Raised when attempting to deliver to or reset a finalized intent.
      class FinalizedError < ActiveRecordError # :nodoc:
      end

      # Buffers instrumentation events during background execution for later publishing
      class EventBuffer
        def initialize(intent, instrumenter)
          @intent = intent
          @instrumenter = instrumenter
          @events = []
        end

        def instrument(name, payload = {}, &block)
          event = @instrumenter.new_event(name, payload)
          begin
            event.record(&block)
          ensure
            @events << event
          end
        end

        def build_handle(name, payload)
          DeferredHandle.new(self, @instrumenter, name, payload)
        end

        def add_event(event)
          @events << event
        end

        def flush
          events, @events = @events, []
          events.each do |event|
            event.payload[:lock_wait] = @intent.lock_wait
            ActiveSupport::Notifications.publish_event(event)
          end
        end

        class DeferredHandle
          def initialize(buffer, instrumenter, name, payload)
            @buffer = buffer
            @event = instrumenter.new_event(name, payload)
          end

          def start
            @event.start!
          end

          def finish
            @event.finish!
            @buffer.add_event(@event)
          end

          def payload
            @event.payload
          end
        end
      end

      attr_reader :arel, :name, :prepare, :allow_retry, :allow_async,
                  :materialize_transactions, :batch, :pool, :session, :lock_wait,
                  :event_buffer, :error, :finalized
      alias_method :finalized?, :finalized
      attr_writer :raw_sql, :session
      attr_accessor :adapter, :binds, :ran_async, :notification_payload, :log_handle,
                    :retry_budget

      def initialize(adapter:, arel: nil, raw_sql: nil, processed_sql: nil, name: "SQL", binds: [], prepare: false, allow_async: false,
                     allow_retry: false, materialize_transactions: true, batch: false)
        if arel.nil? && raw_sql.nil? && processed_sql.nil?
          raise ArgumentError, "One of arel, raw_sql, or processed_sql must be provided"
        end

        @adapter = adapter
        @arel = arel
        @raw_sql = raw_sql
        @name = name
        @binds = binds
        @prepare = prepare
        @allow_async = allow_async
        @ran_async = nil
        @allow_retry = allow_retry
        @materialize_transactions = materialize_transactions
        @batch = batch
        @processed_sql = processed_sql
        @type_casted_binds = nil
        @notification_payload = nil
        @raw_result = nil
        @raw_result_available = false
        @warnings = nil
        @outcome_handled = false
        @warning_error = nil
        @executed = false
        @write_query = nil

        # Deferred execution state
        @pool = adapter.pool
        @session = nil
        @mutex = ActiveSupport::Concurrency::NullLock
        @error = nil
        @lock_wait = nil
        @event_buffer = nil
        @log_handle = nil
        @finalized = false

        @retry_budget = nil

        # Lifecycle state. The intent starts pending and moves exactly once to
        # either :complete or :canceled. A pending intent may additionally have
        # an explicit cancel request in flight against a running query.
        @state_mutex = Mutex.new
        @state_condition = ConditionVariable.new
        @state = :pending
        @cancel_requested = false
        @running_connection = nil
        @cancel_listener = nil
      end

      def self.unordered_select?(arel)
        ast = arel.respond_to?(:ast) ? arel.ast : arel

        Arel::Nodes::SelectStatement === ast && ast.orders.empty?
      end

      def self.shuffle_rows?(arel)
        ActiveRecord.shuffle_unordered_selects && unordered_select?(arel)
      end

      # Returns a hash representation of the QueryIntent for debugging/introspection
      def to_h
        {
          arel: arel,
          raw_sql: raw_sql,
          processed_sql: processed_sql,
          name: name,
          binds: binds,
          prepare: prepare,
          allow_async: allow_async,
          allow_retry: allow_retry,
          materialize_transactions: materialize_transactions,
          batch: batch,
          type_casted_binds: type_casted_binds,
          notification_payload: notification_payload
        }
      end

      # Returns a string representation showing key attributes
      def inspect
        "#<#{self.class.name} name=#{name.inspect} allow_retry=#{allow_retry} materialize_transactions=#{materialize_transactions}>"
      end

      # Called by background thread to execute if not already done
      def execute_or_skip
        # A finalized session means the owning unit of work is gone. Don't even
        # borrow a connection: mark the intent canceled and let the worker move
        # on to the next scheduled query.
        unless @session&.active?
          mark_canceled!(reason: :session)
          return
        end

        @session.synchronize do
          # Bail before borrowing a connection if the intent was canceled (or
          # completed) while queued: a query that never started must not hold a
          # pool connection, even for the instant it takes to skip it.
          return unless pending?

          @pool.with_connection do |connection|
            return unless @mutex.try_lock
            previous_instrumenter = ActiveSupport::IsolatedExecutionState[:active_record_instrumenter]
            begin
              # Re-check under the lifecycle lock: another thread may have
              # canceled (or completed) the intent while we waited to start.
              if begin_executing?(connection)
                if connection.sql_notifications?
                  @event_buffer = EventBuffer.new(self, ActiveSupport::Notifications.instrumenter)
                  ActiveSupport::IsolatedExecutionState[:active_record_instrumenter] = @event_buffer
                end

                @adapter = connection
                @ran_async = true
                run_query!
                pump_retries
              end
            rescue => error
              @error = error
            ensure
              finish_executing
              ActiveSupport::IsolatedExecutionState[:active_record_instrumenter] = previous_instrumenter
              @mutex.unlock
            end
          end
        end
      end

      # Is this intent still pending (result not yet available)?
      def pending?
        @state_mutex.synchronize { @state == :pending }
      end

      # Was this intent canceled, either explicitly by its caller or by its
      # asynchronous query session being finalized?
      def canceled?
        @state_mutex.synchronize { @state == :canceled }
      end

      # True when the owning asynchronous query session has been finalized,
      # even if the query itself finished in the background. Such an intent's
      # unobserved result must surface as a cancellation rather than the
      # background outcome.
      def session_finalized?
        @session && !@session.active?
      end

      # Explicitly withdraws the intent before its result is consumed.
      #
      # A query that has not started yet never borrows a connection: it is
      # marked canceled and its background worker simply skips it. A query
      # already running is interrupted through the adapter's native mechanism
      # (when supported), and this method waits for the worker to release the
      # connection it was using.
      #
      # Returns +true+ when the intent was canceled by this call, +false+ when
      # it had already finished. A genuine database error on a finished intent
      # is preserved: callers must be able to tell an explicit withdrawal apart
      # from the database actually raising.
      def cancel
        connection =
          @state_mutex.synchronize do
            return false unless @state == :pending

            @cancel_requested = true
            @running_connection
          end

        if connection.nil?
          # Still queued (or scheduled but never picked up): no connection is
          # borrowed, so there is nothing to interrupt.
          @state_mutex.synchronize { transition_to_canceled_locked }
        else
          # Query in flight: hand off to the adapter's native cancellation
          # mechanism. Keep this outside the lifecycle lock so adapter
          # internals can never deadlock against the worker.
          interrupt_running_query(connection)

          # Wait for the worker to observe the interruption (or simply finish),
          # which also means it has checked its connection back in.
          @state_mutex.synchronize do
            @state_condition.wait(@state_mutex) while @state == :pending
          end
        end

        true
      end

      # Withdraws the intent because its owning asynchronous query session was
      # finalized (the surrounding unit of work ended, e.g. on an exception).
      #
      # Unlike the explicit #cancel, this supersedes any result that has not
      # been observed yet, including a background query that already failed:
      # nobody was going to consume that outcome, so a later #result raises
      # FutureResult::Canceled instead of the background error. An intent whose
      # result was already observed is left untouched.
      def cancel_for_session_finalization!
        @state_mutex.synchronize do
          return if @outcome_handled

          @cancel_requested = true

          if (connection = @running_connection)
            # Running right now: interrupt outside this lock, then wait.
            @state_mutex.unlock
            begin
              interrupt_running_query(connection)
            ensure
              @state_mutex.lock
            end
            @state_condition.wait(@state_mutex) while @state == :pending
          else
            # Queued, or already finished in the background but not observed:
            # finalization wins either way.
            transition_to_canceled_locked(force: true)
          end
        end
      end

      # Set while the background worker holds a checked-out connection and is
      # inside #run_query!. Exposed so adapter-specific cancellation hooks can
      # identify the intent that needs interrupting.
      def cancel_requested?
        @state_mutex.synchronize { @cancel_requested }
      end

      # Returns raw SQL, compiling from arel if needed, memoized
      def raw_sql
        @raw_sql ||
          begin
            compile_arel!
            @raw_sql
          end
      end

      # Returns preprocessed SQL, memoized
      def processed_sql
        @processed_sql ||= preprocess_query
      end

      def type_casted_binds
        @type_casted_binds ||=
          begin
            compile_arel!
            adapter.type_casted_binds(binds)
          end
      end

      def has_binds?
        compile_arel!
        binds && !binds.empty?
      end

      def execute!
        if can_run_async?
          async_schedule!(ActiveRecord::Base.asynchronous_queries_session)
        else
          @ran_async = false
          run_query!
        end
      ensure
        @executed = true
      end

      def future_result
        if pending? || can_run_async?
          FutureResult.new(self)
        else
          FutureResult.wrap(cast_result)
        end
      end

      def finish
        affected_rows # just to consume/close the result
        nil
      end

      def deliver_result(value, warnings: nil)
        raise FinalizedError, "delivering result to a finalized intent" if @finalized

        adapter.lock.synchronize do
          @raw_result = value
          @warnings = warnings
          @error = nil

          mark_transaction_dirty

          @raw_result_available = true
          mark_completed!
        end

        finish_log
      end

      def deliver_failure(exception, warnings: nil)
        raise FinalizedError, "delivering failure to a finalized intent" if @finalized

        adapter.lock.synchronize do
          @error = exception
          @warnings = warnings

          adapter.downgrade_connection_after_error(exception)
          retryable = adapter.retryable_failure?(exception, @retry_budget)
          mark_transaction_dirty unless retryable

          @raw_result_available = !retryable
        end
      end

      def reset_for_retry
        raise FinalizedError, "resetting a finalized intent" if @finalized

        @raw_result = nil
        @raw_result_available = false
        @warnings = nil
        @outcome_handled = false
        @warning_error = nil
        @error = nil
      end

      # Check if result has been populated yet (without blocking)
      def raw_result_available?
        @raw_result_available
      end

      # Access the raw result, ensuring it's available first
      def raw_result
        ensure_result
        @raw_result
      end

      # Ensure the result is available, blocking if necessary
      def ensure_result
        if @outcome_handled
          raise FutureResult::Canceled if canceled? || @session&.active? == false
          raise @error if @error
          raise @warning_error if @warning_error
          return
        end

        if @session
          # Async was scheduled: wait for result (sets lock_wait)
          execute_or_wait
        end

        # An explicit (or session-level) cancellation wins over anything a
        # late background attempt may have stored: callers must be able to
        # tell a withdrawal apart from a genuine database error. Session
        # finalization is checked directly because it can overtake a query
        # that completed in the background but was never observed.
        if canceled? || @session&.active? == false
          @outcome_handled = true
          @event_buffer&.flush
          raise FutureResult::Canceled
        end

        @event_buffer&.flush
        pump_retries
        @event_buffer&.flush

        if @error
          begin
            adapter.handle_warnings(self, @warnings)
          rescue
            # The query failed, so we need to swallow this exception
            # from handle_warnings to avoid masking the original.
          end
          finish_log(exception: @error)
          @outcome_handled = true
          @event_buffer&.flush
          raise @error
        end

        begin
          adapter.handle_warnings(self, @warnings)
        rescue => warning_error
          @warning_error = warning_error
          @outcome_handled = true
          @event_buffer&.flush
          raise
        end

        @outcome_handled = true
        @event_buffer&.flush
      end

      def cast_result
        raise "Cannot call cast_result before query has executed" unless @executed
        raise "Cannot call cast_result after affected_rows has been called" if defined?(@affected_rows)

        ensure_result
        @cast_result ||= adapter.send(:cast_result, @raw_result).shuffle_rows(@arel)
      end

      def affected_rows
        raise "Cannot call affected_rows before query has executed" unless @executed
        raise "Cannot call affected_rows after cast_result has been called" if defined?(@cast_result)

        ensure_result
        @affected_rows ||= adapter.send(:affected_rows, @raw_result)
      end

      def finish_log(exception: nil) # :nodoc:
        return if @finalized

        settle_failure if exception
        @finalized = true
        adapter.finish_intent_log(self, exception: exception)
      end

      private
        # Retry a delivered failure while the adapter remains available. Scoped
        # connection owners call this before releasing their connection; otherwise
        # result observation takes responsibility for pumping retries.
        def pump_retries
          while !@finalized && @error && !cancel_requested? && adapter.attempt_retry(@error, @retry_budget)
            reset_for_retry
            run_query!
          end

          settle_failure
        end

        def settle_failure
          return unless @error && !@raw_result_available

          adapter.lock.synchronize do
            mark_transaction_dirty
            @raw_result_available = true
          end
        end

        def mark_transaction_dirty
          adapter.send(:dirty_current_transaction) if @materialize_transactions
        end

        # Claims the intent for execution on +connection+. Returns false when the
        # intent was canceled (or completed) before the worker got to run, in
        # which case the caller must not touch the database.
        def begin_executing?(connection)
          @state_mutex.synchronize do
            if @state == :pending
              @running_connection = connection
              true
            else
              false
            end
          end
        end

        # Releases the execution claim and finalizes the lifecycle once a worker
        # attempt ends. An explicit cancellation supersedes whatever the attempt
        # produced; otherwise a delivered result or error decides the state.
        def finish_executing
          @state_mutex.synchronize do
            @running_connection = nil

            if @cancel_requested
              transition_to_canceled_locked(force: true)
            elsif @raw_result_available || @error
              transition_to_completed_locked
            end

            @state_condition.broadcast
          end
        end

        def mark_completed!
          @state_mutex.synchronize do
            transition_to_completed_locked
          end
        end

        def transition_to_completed_locked
          return unless @state == :pending
          @state = :complete
          # Keep the session finalization listener registered: a completed
          # intent whose result is never observed must still surface Canceled
          # once the owning session is finalized. The listener is dropped when
          # the result is observed, the intent is canceled, or the session
          # finalizes.
          @state_condition.broadcast
        end

        def transition_to_canceled_locked(force: false)
          return unless force || @state == :pending
          @state = :canceled
          @error = FutureResult::Canceled.new
          unregister_cancel_listener
          @state_condition.broadcast
        end

        def mark_canceled!(reason:)
          @state_mutex.synchronize do
            if reason == :session
              # The owning unit of work is gone: a queued intent must not run,
              # even if a worker reaches it a moment later.
              @cancel_requested = true
              transition_to_canceled_locked(force: true)
            else
              transition_to_canceled_locked
            end
          end
        end

        # Invokes the adapter's native out-of-band cancellation. Failures are
        # swallowed (and instrumented): the worker's own error handling remains
        # the fallback for recycling a connection that could not be interrupted.
        def interrupt_running_query(connection)
          connection.cancel_running_query(self)
        rescue => error
          ActiveSupport::Notifications.instrument(
            "async_query_cancel_failed.active_record",
            { error: error, intent: self }
          )
        end

        def unregister_cancel_listener
          return unless @session && @cancel_listener
          @session.remove_cancel_listener(@cancel_listener)
          @cancel_listener = nil
        end

        def async_schedule!(session)
          if adapter.current_transaction.joinable?
            raise AsynchronousQueryInsideTransactionError, "Asynchronous queries are not allowed inside transactions"
          end

          # Upgrade to real mutex now that we'll have concurrent access
          @mutex = Mutex.new
          @session = session

          # If the session is already gone (e.g. an exception unwound the
          # executor before the query was scheduled), skip the queue entirely.
          unless session.active?
            mark_canceled!(reason: :session)
            return
          end

          # Session finalization must withdraw every intent it owns, including
          # ones still waiting for a worker thread.
          @cancel_listener = session.on_cancel { cancel_for_session_finalization! }

          # Force preprocessing on original thread before queuing
          processed_sql

          # Detach from original adapter while in queue
          @adapter = nil

          # Schedule on the pool's async queue
          @pool.schedule_query(self)
        end

        # Heuristically guesses whether this is a write query by examining the outermost
        # SQL operation. Subqueries, function calls, etc are not considered.
        def write_query?
          return @write_query unless @write_query.nil?

          @write_query =
            case arel
            when Arel::SelectManager
              false
            when Arel::InsertManager, Arel::UpdateManager, Arel::DeleteManager
              true
            else
              adapter.write_query?(raw_sql)
            end
        end

        def preprocess_query
          if adapter.preventing_writes? && write_query?
            raise ActiveRecord::ReadOnlyError, "Write query attempted while in readonly mode: #{raw_sql}"
          end

          sql = raw_sql

          # We call transformers after the write checks so we don't need to parse the
          # transformed result (which probably just adds comments we'd need to ignore).
          # This means we assume no transformer will change a read into a write.
          ActiveRecord.query_transformers&.each do |transformer|
            sql = transformer.call(sql, adapter)
          end

          sql
        end

        def compile_arel!
          return if @raw_sql || !@arel
          @raw_sql, @binds, @prepare, @allow_retry = adapter.to_sql_and_binds(@arel, @binds, @prepare, @allow_retry)
          nil
        end

        # Block until result is available, or execute as foreground fallback
        def execute_or_wait
          return (@lock_wait = 0.0) if @raw_result_available

          start = Process.clock_gettime(Process::CLOCK_MONOTONIC, :float_millisecond)
          @mutex.synchronize do
            if pending?
              @pool.with_connection do |connection|
                if begin_executing?(connection)
                  begin
                    @adapter = connection
                    @ran_async = false  # Foreground fallback, not actually async
                    run_query!
                    pump_retries
                  ensure
                    finish_executing
                  end
                end
              end
            else
              # Result was computed by background thread while we waited for mutex
              @lock_wait = Process.clock_gettime(Process::CLOCK_MONOTONIC, :float_millisecond) - start
            end
          rescue => error
            @error = error
            finish_executing
          end
        end

        def can_run_async?
          @allow_async && adapter.async_enabled?
        end

        def run_query!
          adapter.execute_intent(self)
        rescue ::RangeError
          raise if write_query?
          @cast_result = ActiveRecord::Result.empty
          @raw_result_available = true
        end
    end
  end
end
