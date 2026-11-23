# frozen_string_literal: true

require "cases/helper"
require "models/post"
require "support/manual_serial_executor"
require "concurrent/atomic/count_down_latch"

module ActiveRecord
  class FutureResultCancelTest < ActiveRecord::TestCase
    include WaitForTestHelper

    fixtures :posts

    setup do
      @connection = ActiveRecord::Base.lease_connection
      skip "adapter does not run asynchronous queries" unless @connection.async_enabled?
      @pool = @connection.pool
      @executor = nil
      @gates = []
      @checkout_wrapped = false
      @hook_connections = nil
      @original_run_query = nil
    end

    teardown do
      @gates.each(&:release)
      @executor&.drain
    ensure
      @executor&.shutdown
      if defined?(@original_async_executor)
        @pool.instance_variable_set(:@async_executor, @original_async_executor)
      end
      uninstall_cancel_hook
      uninstall_run_query_stub
    end

    def test_cancel_returns_true_for_a_pending_future_result
      future_result = blocked_future("Cancelable Blocking Load")
      assert_predicate future_result, :pending?

      assert_equal true, future_result.cancel
      assert_predicate future_result, :canceled?
      assert_not_predicate future_result, :pending?
    end

    def test_cancel_returns_false_when_already_finished
      future_result = @connection.select_all("SELECT * FROM posts", async: true)
      wait_for_async_query(@connection)

      assert_equal false, future_result.cancel
      assert_not_predicate future_result, :canceled?
      assert_kind_of ActiveRecord::Result, future_result.result
    end

    def test_consuming_a_canceled_future_result_raises_canceled_not_a_database_error
      future_result = blocked_future("Cancelable Blocking Load")
      future_result.cancel

      error = assert_raises(ActiveRecord::FutureResult::Canceled) do
        future_result.result
      end
      assert_kind_of ActiveRecord::FutureResult::Canceled, error
    end

    def test_a_real_database_error_is_still_surfaced_after_completion
      future_result = @connection.select_all("SELECT * FROM does_not_exists", async: true)
      wait_for_async_query(@connection)

      # The query genuinely failed in the background: cancel cannot rewrite that
      # into an explicit withdrawal once it has finished.
      assert_equal false, future_result.cancel
      assert_raises(ActiveRecord::StatementInvalid) do
        future_result.result
      end
    end

    def test_canceled_future_result_to_a_raises_canceled
      future_result = blocked_future("Cancelable Blocking Load")
      future_result.cancel

      assert_raises(ActiveRecord::FutureResult::Canceled) do
        future_result.to_a
      end
    end

    def test_relation_cancel_withdraws_a_scheduled_query
      gate = install_run_query_stub("Post Load")
      use_manual_executor
      relation = Post.where(author_id: 1).load_async
      assert gate.entered.wait(10)
      assert_predicate relation, :scheduled?

      assert_equal true, relation.cancel
      assert_predicate relation, :canceled?
      assert_not_predicate relation, :scheduled?

      assert_raises(ActiveRecord::FutureResult::Canceled) do
        relation.to_a
      end
    end

    def test_relation_cancel_returns_false_without_a_scheduled_query
      relation = Post.where(author_id: 1)
      assert_equal false, relation.cancel
      assert_not_predicate relation, :canceled?
    end

    def test_relation_reset_clears_the_canceled_state
      gate = install_run_query_stub("Post Load")
      use_manual_executor
      relation = Post.where(author_id: 1).load_async
      assert gate.entered.wait(10)
      relation.cancel
      assert_predicate relation, :canceled?

      relation.reset
      assert_not_predicate relation, :canceled?
      # The relation is usable again and runs a fresh query.
      assert_equal Post.where(author_id: 1).to_a, relation.to_a
    end

    # The headline scenario: a bounded (single-thread) executor is occupied by
    # a first query, the second query is queued and canceled before it starts,
    # and a third query submitted afterwards still gets to borrow a connection.
    def test_queued_cancel_skips_the_connection_and_a_later_query_still_runs
      use_manual_executor
      before = @executor.scheduled_task_count
      gate = install_run_query_stub("Blocking First Load")

      # Occupy the single worker thread.
      @connection.select_all("SELECT * FROM posts", "Blocking First Load", async: true)
      assert gate.entered.wait(10)

      # The second query is queued behind the first and never starts.
      queued = @connection.select_all("SELECT * FROM posts", "Queued Second Load", async: true)
      queued_intent = queued.instance_variable_get(:@intent)
      wait_for(message: "second query was not scheduled") do
        queued_intent.instance_variable_get(:@session)
      end
      wait_for(message: "second query was not queued") do
        @executor.scheduled_task_count >= before + 2
      end
      assert_nil queued_intent.adapter, "queued query must not have borrowed a connection"

      assert_equal true, queued.cancel
      assert_predicate queued, :canceled?
      assert_nil queued_intent.adapter, "canceled queued query must never borrow a connection"

      # Free the worker and let it drain. The canceled queued job must be
      # skipped without touching the database or borrowing a connection.
      gate.release.count_down
      @executor.drain

      # A fresh submission proves connections can still be borrowed normally.
      follow_up = @connection.select_all("SELECT * FROM posts", "Follow Up Load", async: true)
      wait_for_async_query(@connection)
      assert_kind_of ActiveRecord::Result, follow_up.result
    end

    def test_running_query_cancel_waits_for_the_worker_and_recycles_its_connection
      future_result = blocked_future("Cancelable Blocking Load")
      intent = future_result.instance_variable_get(:@intent)

      # The worker has borrowed a connection and is inside the query.
      assert_not_nil intent.instance_variable_get(:@running_connection)

      assert_equal true, future_result.cancel
      assert_predicate future_result, :canceled?
      assert_nil intent.instance_variable_get(:@running_connection)
      assert_raises(ActiveRecord::FutureResult::Canceled) { future_result.result }

      # The interrupted worker checked its connection back in: another query on
      # the same serial executor can borrow it and succeed.
      follow_up = @connection.select_all("SELECT * FROM posts", "Follow Up Load", async: true)
      wait_for_async_query(@connection)
      assert_kind_of ActiveRecord::Result, follow_up.result
    end

    def test_cancel_invokes_the_adapter_native_cancel_hook_for_a_running_query
      gate = install_run_query_stub("Cancelable Blocking Load", auto_release_on_cancel: false)
      use_manual_executor

      hook_calls = []
      install_cancel_hook_on_pool do |intent|
        hook_calls << intent
        gate.release.count_down
      end

      future_result = @connection.select_all("SELECT * FROM posts", "Cancelable Blocking Load", async: true)
      assert gate.entered.wait(10)

      future_result.cancel

      assert_equal 1, hook_calls.size
      assert_same intent_of(future_result), hook_calls.first
    end

    def test_a_failed_native_cancel_hook_does_not_swallow_the_cancellation
      gate = install_run_query_stub("Cancelable Blocking Load", auto_release_on_cancel: false)
      use_manual_executor
      install_cancel_hook_on_pool { |_intent| raise "native cancel unavailable" }

      future_result = @connection.select_all("SELECT * FROM posts", "Cancelable Blocking Load", async: true)
      assert gate.entered.wait(10)

      # Let the worker finish independently, as it would once a non-cancelable
      # query reaches the end of its natural execution.
      Thread.new { gate.release.count_down }

      # The orchestration still waits for the worker and reports cancellation;
      # the hook failure is instrumented, not raised.
      assert_equal true, future_result.cancel
      assert_predicate future_result, :canceled?
    end

    def test_adapter_advertises_whether_it_can_cancel_running_queries
      assert_includes [true, false], @connection.supports_canceling_running_queries?
    end

    if ActiveRecord::Base.lease_connection.adapter_name == "SQLite"
      def test_sqlite_interrupt_is_translated_to_query_canceled
        translated = @connection.send(
          :translate_exception,
          SQLite3::InterruptException.new("interrupted"),
          message: "interrupted",
          sql: "SELECT 1",
          binds: []
        )

        assert_kind_of ActiveRecord::QueryCanceled, translated
      end
    end

    def test_promise_reflects_cancellation
      future_result = blocked_future("Cancelable Blocking Load")
      promise = future_result.then { |result| result.rows }
      future_result.cancel

      assert_raises(ActiveRecord::FutureResult::Canceled) do
        promise.value
      end
    end

    private
      QueryGate = Struct.new(:entered, :release)

      def intent_of(future_result)
        future_result.instance_variable_get(:@intent)
      end

      def use_manual_executor
        @executor = ManualSerialExecutor.new
        # schedule_query reads the pool's @async_executor instance variable
        # directly, so the double must be assigned there rather than hidden
        # behind an overridden reader.
        @original_async_executor = @pool.instance_variable_get(:@async_executor)
        @pool.instance_variable_set(:@async_executor, @executor)
      end

      # Stubs QueryIntent#run_query! for a named query so that, once a worker
      # has checked out a connection and reached the query, it blocks on a gate
      # until the test releases it. The block happens inside the real
      # execute_or_skip, so @running_connection is set and the connection is
      # genuinely borrowed.
      def install_run_query_stub(name, auto_release_on_cancel: true)
        entered = Concurrent::CountDownLatch.new
        release = Concurrent::CountDownLatch.new
        gate = QueryGate.new(entered, release)
        @gates << gate

        intent_class = ActiveRecord::ConnectionAdapters::QueryIntent
        @original_run_query = intent_class.instance_method(:run_query!)
        original = @original_run_query

        intent_class.silence_redefinition_of_method(:run_query!)
        intent_class.define_method(:run_query!) do
          if name == self.name
            gate.entered.count_down
            gate.release.wait(30)
          end
          # Delegate to the real implementation so a non-matching query (e.g.
          # the follow-up) runs normally against the database.
          original.bind_call(self)
        end
        intent_class.send(:private, :run_query!)

        if auto_release_on_cancel
          # Emulate the adapter's native out-of-band interrupt: when the intent
          # is canceled, release the gate the worker is blocked on.
          install_cancel_hook_on_pool { gate.release.count_down }
        end

        gate
      end

      def install_cancel_hook_on_pool(&block)
        # Apply the hook to connections already in the pool...
        @pool.connections.each do |connection|
          unless connection.respond_to?(:test_cancel_hook_installed)
            connection.define_singleton_method(:test_cancel_hook_installed) { true }
            connection.define_singleton_method(:cancel_running_query) do |intent|
              block.call(intent)
            end
            (@hook_connections ||= []) << connection
          end
        end

        # ...and to any connection checked out later.
        return if @checkout_wrapped

        @checkout_wrapped = true
        pool = @pool
        original_checkout = pool.method(:checkout)

        pool.define_singleton_method(:checkout) do |*args|
          connection = original_checkout.call(*args)
          unless connection.respond_to?(:test_cancel_hook_installed)
            connection.define_singleton_method(:test_cancel_hook_installed) { true }
            connection.define_singleton_method(:cancel_running_query) do |intent|
              block.call(intent)
            end
            (@hook_connections ||= []) << connection
          end
          connection
        end
      end

      def uninstall_cancel_hook
        (@hook_connections || []).each do |connection|
          next unless connection.respond_to?(:test_cancel_hook_installed)
          connection.singleton_class.remove_method(:cancel_running_query)
          connection.singleton_class.remove_method(:test_cancel_hook_installed)
        end
        @hook_connections = nil

        return unless @checkout_wrapped
        @pool.singleton_class.remove_method(:checkout)
        @checkout_wrapped = false
      end

      def uninstall_run_query_stub
        return unless defined?(@original_run_query) && @original_run_query
        intent_class = ActiveRecord::ConnectionAdapters::QueryIntent
        intent_class.silence_redefinition_of_method(:run_query!)
        intent_class.send(:define_method, :run_query!, @original_run_query)
        intent_class.send(:private, :run_query!)
        @original_run_query = nil
      end

      def blocked_future(name)
        gate = install_run_query_stub(name)
        use_manual_executor
        future_result = @connection.select_all("SELECT * FROM posts", name, async: true)
        assert gate.entered.wait(10)
        future_result
      end
  end
end
