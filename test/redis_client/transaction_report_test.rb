# frozen_string_literal: true

require "test_helper"

class RedisClient
  # These tests drive the client through controlled failure sequences with an
  # in-process RESP3 server, so they don't need a real Redis instance.
  class TransactionReportTest < RedisClientTestCase
    def setup
      @servers = []
      super
    end

    def teardown
      @servers.each(&:close)
      super
    end

    def test_successful_transaction
      client, = new_client_for { |session, command| default_handler(session, command) }

      executed = []
      report = client.multi(report: true) do |transaction|
        executed << transaction.call("SET", "foo", "bar")
        executed << transaction.call("INCR", "counter")
      end

      assert_predicate report, :executed?
      assert_predicate report, :success?
      refute_predicate report, :failure?
      assert_equal 2, report.size
      assert_equal ["OK", 1], report.results
      assert_equal [1, 2], report.entries.map(&:position)
      assert_equal [nil, nil], executed, "transaction.call must return nil, like the regular API"
      assert_equal ["SET", "foo", "bar"], report.entries[0].command
      assert_empty report.errors
    end

    def test_empty_transaction
      client, = new_client_for { |session, command| default_handler(session, command) }

      report = client.multi(report: true) { |_transaction| }

      assert_predicate report, :empty?
      assert_predicate report, :success?
      refute_predicate report, :started?
      assert_equal [], report.results
      assert_equal [], report.entries
      refute_predicate client, :connected?, "an empty transaction must not open a connection"
    end

    def test_queue_rejection_is_distinct_from_execution_failure
      client, = new_client_for do |session, command|
        case command.first.upcase
        when "MULTI"
          session << FakeRedisServer::Wire.ok
        when "EXEC"
          session << FakeRedisServer::Wire.error("EXECABORT Transaction discarded because of previous errors.")
        when "BAD"
          session << FakeRedisServer::Wire.error("ERR unknown command 'BAD'")
        else
          session << FakeRedisServer::Wire.queued
        end
      end

      report = client.multi(report: true) do |transaction|
        transaction.call("BAD")
        transaction.call("SET", "foo", "bar")
      end

      assert_predicate report, :rejected?
      refute_predicate report, :executed?
      refute_predicate report, :success?

      assert_predicate report.entries[0], :rejected?
      assert_predicate report.entries[1], :not_queued?
      assert_equal ["BAD"], report.entries[0].error.command
      assert_instance_of RedisClient::CommandError, report.exec_error
      assert_equal({ 0 => report.entries[0].error }, report.errors)
      assert_equal [0].map { |i| report.results[i] }, [report.entries[0].error]
    end

    def test_execution_failure_keeps_original_positions
      client, = new_client_for do |session, command|
        case command.first.upcase
        when "MULTI"
          session << FakeRedisServer::Wire.ok
        when "EXEC"
          session << FakeRedisServer::Wire.array([
            FakeRedisServer::Wire.ok,
            FakeRedisServer::Wire.error("WRONGTYPE Operation against a key holding the wrong kind of value"),
            FakeRedisServer::Wire.int(3),
          ])
        else
          session << FakeRedisServer::Wire.queued
        end
      end

      report = client.multi(report: true) do |transaction|
        transaction.call("SET", "foo", "1")
        transaction.call("INCR", "foo")
        transaction.call("INCR", "bar")
      end

      assert_predicate report, :executed?
      refute_predicate report, :success?

      assert_equal ["OK", report.entries[1].error, 3], report.results
      assert_equal [2], report.failed_entries.map(&:position)
      assert_instance_of RedisClient::WrongTypeError, report.entries[1].error
      # The nested error still refers to the original command.
      assert_equal ["INCR", "foo"], report.entries[1].error.command
      assert_equal 3, report.entries[2].value
      assert_equal({ 1 => report.entries[1].error }, report.errors)
    end

    def test_watch_abort_is_not_an_empty_success
      client, = new_client_for do |session, command|
        case command.first.upcase
        when "MULTI", "WATCH"
          session << FakeRedisServer::Wire.ok
        when "EXEC"
          session << FakeRedisServer::Wire.null
        else
          session << FakeRedisServer::Wire.queued
        end
      end

      report = client.multi(report: true, watch: ["foo"]) do |transaction|
        transaction.call("SET", "foo", "bar")
      end

      assert_predicate report, :aborted?
      refute_predicate report, :executed?
      refute_predicate report, :success?
      refute_predicate report, :empty?
      assert_same TransactionReport::UNKNOWN, report.results[0]
      assert_predicate report.entries[0], :aborted?
      refute TransactionReport.unknown?(nil)
    end

    def test_explicit_nil_is_an_executed_nil
      client, = new_client_for do |session, command|
        case command.first.upcase
        when "MULTI"
          session << FakeRedisServer::Wire.ok
        when "EXEC"
          # e.g. `SET key value NX` that didn't set anything: a real nil.
          session << FakeRedisServer::Wire.array([FakeRedisServer::Wire.null])
        else
          session << FakeRedisServer::Wire.queued
        end
      end

      report = client.multi(report: true) do |transaction|
        transaction.call("SET", "foo", "bar", "NX")
      end

      assert_predicate report, :executed?
      assert_predicate report, :success?
      assert_nil report.entries[0].value
      assert_predicate report.entries[0], :executed?
      assert_nil report.results[0]
      refute TransactionReport.unknown?(report.results[0])
    end

    def test_per_command_blocks_only_run_for_executed_commands
      client, = new_client_for do |session, command|
        case command.first.upcase
        when "MULTI"
          session << FakeRedisServer::Wire.ok
        when "EXEC"
          session << FakeRedisServer::Wire.array([
            FakeRedisServer::Wire.int(2),
            FakeRedisServer::Wire.error("WRONGTYPE nope"),
          ])
        else
          session << FakeRedisServer::Wire.queued
        end
      end

      applied = []
      report = client.multi(report: true) do |transaction|
        transaction.call("INCR", "counter") { |value| applied << value; value * 10 }
        transaction.call("INCR", "other") { |value| applied << value; value * 10 }
      end

      assert_equal [2], applied
      assert_equal 20, report.entries[0].value
      assert_predicate report.entries[1], :failed?
      assert_predicate report, :failure?
    end

    def test_refused_multi_is_reported_as_untransacted
      client, = new_client_for do |session, command|
        case command.first.upcase
        when "MULTI"
          session << FakeRedisServer::Wire.error("ERR MULTI calls can not be nested")
        when "EXEC"
          session << FakeRedisServer::Wire.error("ERR EXEC without MULTI")
        else
          # Executed on their own, outside of a transaction.
          session << FakeRedisServer::Wire.ok
        end
      end

      report = client.multi(report: true) do |transaction|
        transaction.call("SET", "foo", "bar")
        transaction.call("SET", "baz", "qux")
      end

      assert_predicate report, :untransacted?
      refute_predicate report, :success?
      assert_instance_of RedisClient::CommandError, report.multi_error
      # The commands did run, just not as a transaction.
      assert report.entries.all?(&:executed?)
      assert_equal ["OK", "OK"], report.results
    end

    def test_interrupted_transaction_keeps_unknown_outcomes
      client, = new_client_for(reconnect_attempts: 3) do |session, command|
        case command.first.upcase
        when "MULTI"
          session << FakeRedisServer::Wire.ok
        when "EXEC"
          # Drop the connection instead of sending the EXEC reply.
          session.close
        else
          session << FakeRedisServer::Wire.queued
        end
      end

      block_calls = 0
      report = client.multi(report: true) do |transaction|
        transaction.call("SET", "foo", "bar") { |value| block_calls += 1; value }
        transaction.call("SET", "baz", "qux")
      end

      assert_predicate report, :interrupted?
      assert_predicate report, :failure?
      refute_predicate report, :executed?
      assert_equal 2, report.size
      assert report.entries.all?(&:unknown?)
      assert_equal [TransactionReport::UNKNOWN] * 2, report.results
      assert_instance_of RedisClient::ConnectionError, report.interruption_error
      # The transaction must not be replayed, and the blocks must not run.
      assert_equal 0, block_calls
    end

    def test_callback_block_runs_once_when_the_connection_drops
      client, = new_client_for(reconnect_attempts: 3) do |session, command|
        case command.first.upcase
        when "MULTI"
          session << FakeRedisServer::Wire.ok
        when "EXEC"
          session.close
        else
          session << FakeRedisServer::Wire.queued
        end
      end

      calls = 0
      client.multi(report: true) do |transaction|
        calls += 1
        transaction.call("SET", "foo", "bar")
      end

      assert_equal 1, calls
    end

    def test_connection_failure_before_anything_is_sent
      port = TCPServer.new("127.0.0.1", 0).then { |server| server.addr[1].tap { server.close } }
      client = RedisClient.new(host: "127.0.0.1", port: port, timeout: 1.0)

      calls = 0
      report = client.multi(report: true) do |transaction|
        calls += 1
        transaction.call("SET", "foo", "bar")
      end

      assert_equal 1, calls
      assert_predicate report, :not_entered?
      assert_predicate report, :failure?
      refute_predicate report, :started?
      assert report.entries.all?(&:not_queued?)
      assert_instance_of RedisClient::CannotConnectError, report.error
    end

    def test_partial_replies_before_the_interruption_are_kept
      client, = new_client_for do |session, command|
        case command.first.upcase
        when "MULTI"
          # MULTI was accepted, then the connection drops while the commands are
          # still being queued: nothing is known about the commands themselves.
          session << FakeRedisServer::Wire.ok
        else
          session.close
        end
      end

      report = client.multi(report: true) do |transaction|
        transaction.call("SET", "foo", "bar")
        transaction.call("SET", "baz", "qux")
      end

      assert_predicate report, :interrupted?
      assert report.entries.all?(&:unknown?)
      assert_equal [RedisClient::TransactionReport::UNKNOWN] * 2, report.results
    end

    def test_exec_reply_received_before_a_later_failure_is_fully_known
      # The EXEC reply is read, then the connection drops while the client is
      # idle: the transaction outcome is known and must be reported as executed.
      client, = new_client_for do |session, command|
        case command.first.upcase
        when "MULTI"
          session << FakeRedisServer::Wire.ok
        when "EXEC"
          session << FakeRedisServer::Wire.array([FakeRedisServer::Wire.ok])
          session.close
        else
          session << FakeRedisServer::Wire.queued
        end
      end

      report = client.multi(report: true) { |transaction| transaction.call("SET", "foo", "bar") }

      assert_predicate report, :executed?
      assert_predicate report, :success?
      assert_equal ["OK"], report.results
    end

    def test_truncated_exec_reply_leaves_the_missing_command_unknown
      client, = new_client_for do |session, command|
        case command.first.upcase
        when "MULTI"
          session << FakeRedisServer::Wire.ok
        when "EXEC"
          # Two commands were queued but only one reply is provided.
          session << FakeRedisServer::Wire.array([FakeRedisServer::Wire.ok])
        else
          session << FakeRedisServer::Wire.queued
        end
      end

      report = client.multi(report: true) do |transaction|
        transaction.call("SET", "foo", "bar")
        transaction.call("SET", "baz", "qux")
      end

      assert_predicate report, :interrupted?
      refute_predicate report, :success?
      assert_predicate report.entries[0], :executed?
      assert_predicate report.entries[1], :unknown?
      assert_equal ["OK", RedisClient::TransactionReport::UNKNOWN], report.results
      assert_instance_of RedisClient::ProtocolError, report.error
    end

    def test_interruption_after_observed_queue_rejection
      client, = new_client_for do |session, command|
        case command.first.upcase
        when "MULTI"
          session << FakeRedisServer::Wire.ok
        when "BAD"
          session << FakeRedisServer::Wire.error("ERR unknown command 'BAD'")
        else
          session.close
        end
      end

      report = client.multi(report: true) do |transaction|
        transaction.call("BAD")
        transaction.call("SET", "foo", "bar")
        transaction.call("SET", "baz", "qux")
      end

      # The rejection is a fact that was observed, so it is still reported as
      # such, even though the connection dropped afterwards.
      assert_predicate report, :rejected?
      assert_predicate report.entries[0], :rejected?
      assert_predicate report.entries[1], :not_queued?
      assert_predicate report.entries[2], :not_queued?
    end

    def test_watch_mode_executes_the_block_on_the_watched_connection
      seen = []
      client, = new_client_for do |session, command|
        case command.first.upcase
        when "WATCH", "MULTI"
          session << FakeRedisServer::Wire.ok
        when "GET"
          session << "$3\r\nfoo\r\n"
        when "EXEC"
          session << FakeRedisServer::Wire.array([FakeRedisServer::Wire.ok])
        else
          session << FakeRedisServer::Wire.queued
        end
      end

      report = client.multi(report: true, watch: ["foo"]) do |transaction|
        # Reading from the client inside the block must use the same watched
        # connection, like the regular `#multi(watch:)`.
        seen << client.call("GET", "foo")
        transaction.call("SET", "foo", "bar")
      end

      assert_equal ["foo"], seen
      assert_predicate report, :executed?
      assert_predicate report, :success?
      assert_equal ["foo"], report.watch
      assert_equal ["OK"], report.results
    end

    def test_watch_abort_releases_the_watch
      exec_count = 0
      client, = new_client_for do |session, command|
        case command.first.upcase
        when "MULTI", "WATCH", "UNWATCH"
          session << FakeRedisServer::Wire.ok
        when "EXEC"
          exec_count += 1
          # First EXEC is aborted, the next one succeeds.
          session << if exec_count == 1
            FakeRedisServer::Wire.null
          else
            FakeRedisServer::Wire.array([FakeRedisServer::Wire.ok])
          end
        else
          session << FakeRedisServer::Wire.queued
        end
      end

      report = client.multi(report: true, watch: ["foo"]) { |t| t.call("SET", "foo", "bar") }

      assert_predicate report, :aborted?
      # A subsequent transaction must not inherit the WATCH state.
      assert_equal ["OK"], client.multi { |t| t.call("SET", "foo", "bar") }
    end

    def test_callback_error_propagates_without_being_reported
      client, = new_client_for { |session, command| default_handler(session, command) }

      error = assert_raises(ArgumentError) do
        client.multi(report: true) do |_transaction|
          raise ArgumentError, "boom"
        end
      end

      assert_equal "boom", error.message
    end

    def test_callback_error_inside_watch_keeps_the_client_usable
      client, = new_client_for { |session, command| default_handler(session, command) }

      assert_raises(RuntimeError) do
        client.multi(report: true, watch: ["foo"]) do |_transaction|
          raise "boom"
        end
      end

      # The WATCH state was released, so plain calls work again.
      assert_equal ["OK", 1], client.multi { |t| t.call("SET", "foo", "bar") }
    end

    def test_regular_multi_behavior_is_unchanged
      client, = new_client_for do |session, command|
        case command.first.upcase
        when "MULTI"
          session << FakeRedisServer::Wire.ok
        when "EXEC"
          session << FakeRedisServer::Wire.array([FakeRedisServer::Wire.int(1)])
        else
          session << FakeRedisServer::Wire.queued
        end
      end

      assert_equal [1], client.multi { |transaction| transaction.call("INCR", "counter") }
    end

    def test_report_to_h
      client, = new_client_for { |session, command| default_handler(session, command) }

      report = client.multi(report: true) { |transaction| transaction.call("SET", "foo", "bar") }
      hash = report.to_h

      assert_equal :executed, hash[:status]
      assert_nil hash[:watch]
      assert_equal 1, hash[:entries].size
      assert_equal 1, hash[:entries][0][:position]
      assert_equal ["SET", "foo", "bar"], hash[:entries][0][:command]
      assert_equal :executed, hash[:entries][0][:status]
    end

    private

    def new_client_for(**overrides, &block)
      server = FakeRedisServer.new(block)
      @servers << server
      [server.client(**overrides), server]
    end

    def default_handler(session, command)
      case command.first.upcase
      when "MULTI"
        session << FakeRedisServer::Wire.ok
      when "EXEC"
        session << FakeRedisServer::Wire.array([FakeRedisServer::Wire.ok, FakeRedisServer::Wire.int(1)])
      else
        session << FakeRedisServer::Wire.queued
      end
    end
  end
end
