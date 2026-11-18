# frozen_string_literal: true

# Self-contained, zero-network tests for the layered observability feature.
# They run against an in-process fake connection (no Redis server required),
# so they also work in sandboxes without the full megatest/redis toolchain.
# Run with: ruby -Ilib test/redis_client/observability_test.rb
require "minitest/autorun"
require "redis_client"
require "redis_client/observability"

class RedisClient
  class ObservabilityTest < Minitest::Test
    class FakeConnection
      include RedisClient::ConnectionMixin

      class << self
        attr_accessor :fail_commands, :connect_count

        def reset!
          @fail_commands = []
          @connect_count = 0
        end
      end

      def initialize(config, **)
        super(config)
        self.class.connect_count = (self.class.connect_count || 0) + 1
      end

      def connected?
        !@closed
      end

      def close
        @closed = true
      end

      def write(command)
        @closed = false
        (@queue ||= []).push(command)
      end

      def write_multi(commands)
        commands.each { |c| write(c) }
      end

      def read(_timeout = nil)
        command = (@queue ||= []).shift
        if command && (idx = self.class.fail_commands.index(command.first))
          self.class.fail_commands.delete_at(idx)
          raise connection_error("simulated failure on #{command.first}")
        end

        case command&.first
        when "SET" then "OK"
        when "GET" then "v"
        when "MULTI" then "OK"
        when "EXEC" then ["OK"]
        else "PONG"
        end
      end
    end

    module FakeDriver
      def self.new(config, **options)
        FakeConnection.new(config, **options)
      end

      def self.ssl_context(_params)
        nil
      end
    end

    RedisClient.register_driver(:observability_fake) { FakeDriver }

    def setup
      FakeConnection.reset!
      @recorder = RedisClient::Observability::RecordingObserver.new
    end

    def client(**overrides)
      RedisClient.new({ driver: :observability_fake, observer: @recorder }.merge(overrides))
    end

    def finished_command
      event = @recorder.events.find { |callback, ev| callback == :command_finished && ev.command? }
      event&.last
    end

    def test_stable_call_id_with_distinct_attempt_ids
      result = client.call("PING")
      assert_equal "PONG", result

      command_events = @recorder.events.select { |_cb, ev| ev.command? }
      assert_equal 2, command_events.size # started + finished
      call_ids = command_events.map(&:last).map(&:call_id).uniq
      assert_equal 1, call_ids.size

      attempts = @recorder.events.select { |cb, ev| cb == :attempt_finished && ev.name == :call }
      assert_equal 1, attempts.size
      refute_equal attempts.first.last.attempt_id, call_ids.first
    end

    def test_retry_one_logical_command_many_attempts
      c = client(reconnect_attempts: [0, 0])
      FakeConnection.fail_commands = ["PING"]
      assert_equal "PONG", c.call("PING")

      logical = @recorder.events.select { |_cb, ev| ev.command? }.map(&:last).map(&:call_id).uniq
      assert_equal 1, logical.size

      attempts = @recorder.events.select { |cb, ev| cb == :attempt_finished && ev.name == :call }
      assert_equal 2, attempts.size
      assert_equal [0, 1], attempts.map { |_cb, ev| ev.attempt }
      assert_equal :retry, attempts[0].last.status
      assert_equal :success, attempts[1].last.status
      assert_equal 2, attempts.map { |_cb, ev| ev.attempt_id }.uniq.size
      assert(attempts.all? { |_cb, ev| ev.call_id == logical.first })
    end

    def test_exhausted_retries_report_final_error
      c = client(reconnect_attempts: [0])
      FakeConnection.fail_commands = ["PING", "PING"]
      assert_raises(ConnectionError) { c.call("PING") }

      assert_equal :error, finished_command.status
      attempts = @recorder.events.select { |cb, ev| cb == :attempt_finished && ev.name == :call }
      assert_equal :retry, attempts[0].last.status
      assert_equal :error, attempts[1].last.status
      assert attempts[1].last.error.final?
    end

    def test_unknown_outcome_for_sent_write
      c = client(reconnect_attempts: [0])
      FakeConnection.fail_commands = ["SET", "SET"]
      assert_raises(ConnectionError) { c.call("SET", "k", "secret") }

      assert_equal :unknown, finished_command.status
      assert_equal({ command: "SET", arguments: 2 }, finished_command.command)
    end

    def test_read_failure_is_error_not_unknown
      c = client(reconnect_attempts: [0])
      FakeConnection.fail_commands = ["GET", "GET"]
      assert_raises(ConnectionError) { c.call("GET", "k") }

      assert_equal :error, finished_command.status
    end

    def test_pipeline_identity_and_positions
      result = client.pipelined do |p|
        p.call("PING")
        p.call("SET", "k", "v")
        p.call("GET", "k")
      end
      assert_equal ["PONG", "OK", "v"], result

      started = @recorder.events.find { |cb, ev| cb == :command_started && ev.name == :pipelined }.last
      refute_nil started.pipeline_id
      assert_equal 3, started.size
      assert_equal [
        { command: "PING", arguments: 0 },
        { command: "SET", arguments: 2 },
        { command: "GET", arguments: 1 },
      ], started.commands

      attempt = @recorder.events.find { |cb, ev| cb == :attempt_started && ev.name == :pipelined }.last
      assert_equal started.pipeline_id, attempt.pipeline_id
      assert_equal started.call_id, attempt.call_id
    end

    def test_pipeline_unknown_outcome
      c = client(reconnect_attempts: [0])
      FakeConnection.fail_commands = ["SET", "SET"]
      assert_raises(ConnectionError) do
        c.pipelined do |p|
          p.call("SET", "a", "1")
          p.call("SET", "b", "2")
        end
      end
      assert_equal :unknown, finished_command.status
    end

    def test_prelude_is_marked_and_redacted
      c = client(username: "default", password: "topsecret", db: 3, driver_info: "rb")
      c.call("PING")

      prelude = @recorder.events.select do |callback, event|
        callback == :attempt_finished && event.prelude?
      end
      assert prelude.any?
      assert(prelude.all? { |_cb, ev| ev.phase == :connect })

      hello = prelude.find { |_cb, ev| ev.command[:command] == "HELLO" }.last
      assert_equal 4, hello.command[:arguments]
      refute_match(/topsecret/, hello.inspect)
      assert_equal [0, 1], prelude.map { |_cb, ev| ev.index }.first(2)
    end

    def test_observer_failure_does_not_change_reply
      exploding = Class.new(RedisClient::Observability::Observer) do
        def command_started(_event)
          raise "boom"
        end
      end
      c = client(observer: exploding.new)
      assert_equal "PONG", c.call("PING")
    end

    def test_observer_failure_never_masks_real_error
      exploding = Class.new(RedisClient::Observability::Observer) do
        def command_finished(_event)
          raise "observer boom"
        end

        def attempt_finished(_event)
          raise "observer boom"
        end

        def connect_finished(_event)
          raise "observer boom"
        end
      end
      c = client(observer: exploding.new, reconnect_attempts: [0])
      FakeConnection.fail_commands = ["PING", "PING"]
      error = assert_raises(ConnectionError) { c.call("PING") }
      assert_match(/simulated failure/, error.message)
    end

    def test_safe_observer_routes_errors_to_handler
      handled = []
      exploding = Class.new(RedisClient::Observability::Observer) do
        def attempt_finished(_event)
          raise "kaboom"
        end
      end.new
      safe = RedisClient::Observability::SafeObserver.new(exploding, on_error: ->(err, _cb, _ev) { handled << err.message })
      c = client(observer: safe)
      assert_equal "PONG", c.call("PING")
      assert_includes handled, "kaboom"
    end

    def test_arguments_only_logged_when_opted_in
      c = client(log_arguments: true)
      c.call("SET", "k", "raw")
      assert_equal ["SET", "k", "raw"], finished_command.command
    end

    def test_arguments_redacted_by_default
      client.call("SET", "k", "raw")
      assert_equal({ command: "SET", arguments: 2 }, finished_command.command)
    end

    def test_no_instrumenter_without_observer
      c = RedisClient.new(driver: :observability_fake)
      assert_nil c.instance_variable_get(:@instrumenter)
      assert_equal "PONG", c.call("PING")
    end

    def test_invalid_observer_rejected
      assert_raises(ArgumentError) do
        RedisClient.new(driver: :observability_fake, observer: Object.new)
      end
    end

    def test_read_only_classification
      assert Observability.read_only_command?(["GET", "k"])
      assert Observability.read_only_command?(["CONFIG", "GET", "x"])
      refute Observability.read_only_command?(["CONFIG", "SET", "x", "y"])
      refute Observability.read_only_command?(["SET", "k", "v"])
      refute Observability.read_only_command?(["UNKNOWNCMD"])
    end

    def test_call_once_failure_is_a_single_final_attempt
      c = client(reconnect_attempts: [0])
      FakeConnection.fail_commands = ["PING"]
      assert_raises(ConnectionError) { c.call_once("PING") }

      attempts = @recorder.events.select { |cb, ev| cb == :attempt_finished && ev.name == :call }
      assert_equal 1, attempts.size
      event = attempts.first.last
      assert_equal :error, event.status
      refute event.retryable
      assert event.error.final?
      assert_equal finished_command.call_id, event.call_id
    end

    def test_empty_pipeline_emits_no_events
      result = client.pipelined { |pipeline| pipeline }
      assert_equal [], result
      assert_empty @recorder.events
    end

    def test_multi_uses_batch_identity
      client.multi do |transaction|
        transaction.call("SET", "k", "v")
      end
      event = @recorder.events.find { |cb, ev| cb == :command_started && ev.name == :multi }.last
      refute_nil event.pipeline_id
      assert_equal 3, event.size # MULTI, SET, EXEC
    end

    def test_event_is_immutable
      event = RedisClient::Observability::Event.new(kind: :command, name: :call, call_id: "x")
      assert event.frozen?
      merged = event.merge(status: :success)
      refute_same event, merged
      assert_nil event.status
      assert_equal :success, merged.status
    end
  end
end
