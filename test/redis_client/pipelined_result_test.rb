# frozen_string_literal: true

require "test_helper"

class RedisClient
  class PipelinedResultTest < RedisClientTestCase
    # A scripted, server-less connection used to simulate pipelines
    # interrupted at exact points.
    class Script
      attr_reader :written, :write_multi_calls, :read_count

      def initialize
        @replies = []
        @written = []
        @write_multi_calls = 0
        @read_count = 0
        @write_error = nil
        @read_error_at = nil
        @read_error = nil
      end

      def push_replies(*replies)
        @replies.concat(replies)
      end

      def fail_write(error)
        @write_error = error
      end

      def fail_read_at(count, error)
        @read_error_at = count
        @read_error = error
      end

      def write(command)
        raise @write_error if @write_error

        @written << command
      end

      def write_multi(commands)
        @write_multi_calls += 1
        commands.each { |command| write(command) }
      end

      def read(_timeout = nil)
        if @read_error_at && @read_count >= @read_error_at
          raise @read_error
        end

        @read_count += 1
        @replies.shift
      end
    end

    class FakeDriver
      include ConnectionMixin

      class << self
        attr_accessor :script
      end

      def initialize(config, **)
        super(config)
        @server_key = config.server_key
      end

      def connect
        @server_key = @config.server_key
        true
      end

      def connected?
        true
      end

      def write(command)
        self.class.script.write(command)
      end

      def write_multi(commands)
        self.class.script.write_multi(commands)
      end

      def read(timeout = nil)
        self.class.script.read(timeout)
      end
    end

    COMMANDS = [["SET", "foo", "bar"], ["INCR", "baz"], ["GET", "foo"]].freeze

    def setup
      @script = Script.new
      FakeDriver.script = @script
      @connection = FakeDriver.new(RedisClient.config)
    end

    def test_detailed_complete_pipeline
      @script.push_replies("OK", 1, "bar")

      outcome = @connection.call_pipelined_detailed(COMMANDS, nil)

      assert_predicate outcome, :complete?
      refute_predicate outcome, :interrupted?
      assert_nil outcome.failure
      assert_nil outcome.failure_stage
      assert_equal ["OK", 1, "bar"], outcome.results
      assert_equal COMMANDS, outcome.commands
      assert_empty outcome.errors
      assert_empty outcome.pushes
      assert_equal 3, outcome.confirmed_count
      assert outcome.confirmed?(0)
      assert outcome.confirmed?(2)
      assert_equal (3...3), outcome.unconfirmed_range
      assert_empty outcome.unconfirmed_commands
    end

    def test_detailed_server_errors
      @script.push_replies(CommandError.parse("ERR unknown command"), 1, "bar")

      outcome = @connection.call_pipelined_detailed(COMMANDS, nil)

      assert_predicate outcome, :complete?
      assert_equal 1, outcome.errors.size
      assert_instance_of CommandError, outcome.results[0]
      assert_same outcome.errors.first, outcome.results[0]
      assert_equal COMMANDS[0], outcome.errors.first.command
      assert_equal 1, outcome[1]
      assert_equal "bar", outcome[2]
    end

    def test_detailed_server_errors_are_chained
      @script.push_replies(
        CommandError.parse("ERR first"),
        CommandError.parse("ERR second"),
        "bar",
      )

      outcome = @connection.call_pipelined_detailed(COMMANDS, nil)

      assert_equal 2, outcome.errors.size
      assert_same outcome.errors[1], outcome.errors[0].next_error
    end

    def test_detailed_write_interruption
      error = ConnectionError.with_config("write boom", nil)
      @script.fail_write(error)

      outcome = @connection.call_pipelined_detailed(COMMANDS, nil)

      refute_predicate outcome, :complete?
      assert_predicate outcome, :interrupted?
      assert_predicate outcome, :write_failed?
      refute_predicate outcome, :read_failed?
      assert_equal :write, outcome.failure_stage
      assert_same error, outcome.failure
      assert_equal 0, outcome.confirmed_count
      assert_equal (0...3), outcome.unconfirmed_range
      assert_equal COMMANDS, outcome.unconfirmed_commands
      assert_equal [PipelinedResult::UNKNOWN] * 3, outcome.results
      refute outcome.confirmed?(0)
    end

    def test_detailed_read_interruption
      error = ConnectionError.with_config("read boom", nil)
      @script.push_replies("OK")
      @script.fail_read_at(1, error)

      outcome = @connection.call_pipelined_detailed(COMMANDS, nil)

      refute_predicate outcome, :complete?
      assert_predicate outcome, :read_failed?
      refute_predicate outcome, :write_failed?
      assert_equal :read, outcome.failure_stage
      assert_same error, outcome.failure
      assert_equal 1, outcome.confirmed_count
      assert_equal "OK", outcome[0]
      assert_same PipelinedResult::UNKNOWN, outcome[1]
      assert_same PipelinedResult::UNKNOWN, outcome[2]
      assert_equal (1...3), outcome.unconfirmed_range
      assert_equal COMMANDS[1..], outcome.unconfirmed_commands
    end

    def test_detailed_read_interruption_preserves_server_errors
      @script.push_replies(CommandError.parse("ERR unknown command"))
      @script.fail_read_at(1, ConnectionError.with_config("read boom", nil))

      outcome = @connection.call_pipelined_detailed(COMMANDS, nil)

      assert_predicate outcome, :read_failed?
      assert_equal 1, outcome.errors.size
      assert_equal COMMANDS[0], outcome.errors.first.command
    end

    def test_detailed_push_messages_do_not_shift_positions
      @script.push_replies(
        RESP3::Push["message", "mychannel", "event-0"],
        "OK",
        RESP3::Push["message", "mychannel", "event-1"],
        1,
        "bar",
      )

      outcome = @connection.call_pipelined_detailed(COMMANDS, nil)

      assert_predicate outcome, :complete?
      assert_equal ["OK", 1, "bar"], outcome.results
      assert_equal 2, outcome.pushes.size
      assert_equal ["message", "mychannel", "event-0"], outcome.pushes[0]
    end

    def test_classic_pipeline_returns_results
      @script.push_replies("OK", 1, "bar")

      assert_equal ["OK", 1, "bar"], @connection.call_pipelined(COMMANDS, nil)
    end

    def test_classic_pipeline_raises_first_server_error
      @script.push_replies(
        CommandError.parse("ERR first"),
        CommandError.parse("ERR second"),
        "bar",
      )

      error = assert_raises CommandError do
        @connection.call_pipelined(COMMANDS, nil)
      end
      assert_includes error.message, "ERR first"
      assert_instance_of CommandError, error.next_error
    end

    def test_classic_pipeline_with_exception_false_returns_errors
      @script.push_replies(CommandError.parse("ERR unknown command"), 1, "bar")

      results = @connection.call_pipelined(COMMANDS, nil, exception: false)

      assert_instance_of CommandError, results[0]
      assert_equal [1, "bar"], results[1..]
    end

    def test_classic_pipeline_connection_error_carries_partial_results
      @script.push_replies("OK")
      @script.fail_read_at(1, ConnectionError.with_config("read boom", nil))

      error = assert_raises ConnectionError do
        @connection.call_pipelined(COMMANDS, nil)
      end

      outcome = error.pipeline_result
      assert_instance_of PipelinedResult, outcome
      assert_predicate outcome, :read_failed?
      assert_equal 1, outcome.confirmed_count
      assert_equal "OK", outcome[0]
      assert_same PipelinedResult::UNKNOWN, outcome[1]
      assert_equal (1...3), outcome.unconfirmed_range
    end

    def test_classic_pipeline_skips_push_messages
      @script.push_replies(
        RESP3::Push["invalidate", ["foo"]],
        "OK",
        1,
        "bar",
      )

      assert_equal ["OK", 1, "bar"], @connection.call_pipelined(COMMANDS, nil)
    end

    def test_client_pipelined_detailed_complete
      @script.push_replies("OK", 1, "bar")
      client = new_fake_client

      outcome = client.pipelined_detailed do |pipeline|
        COMMANDS.each { |command| pipeline.call_v(command) }
      end

      assert_predicate outcome, :complete?
      assert_equal ["OK", 1, "bar"], outcome.results
      assert_equal 1, @script.write_multi_calls
    end

    def test_client_pipelined_detailed_empty
      client = new_fake_client

      outcome = client.pipelined_detailed { |_pipeline| }

      assert_predicate outcome, :complete?
      assert_equal 0, outcome.size
      assert_empty outcome.results
    end

    def test_client_pipelined_detailed_does_not_resend_unconfirmed_commands
      @script.push_replies("OK")
      @script.fail_read_at(1, ConnectionError.with_config("read boom", nil))
      client = new_fake_client(reconnect_attempts: 3)

      outcome = client.pipelined_detailed do |pipeline|
        COMMANDS.each { |command| pipeline.call_v(command) }
      end

      assert_predicate outcome, :read_failed?
      assert_equal 1, outcome.confirmed_count
      assert_equal COMMANDS[1..], outcome.unconfirmed_commands
      # The commands may already have been executed by the server, so the
      # client must not have resent them, even with reconnect_attempts.
      assert_equal 1, @script.write_multi_calls
    end

    def test_client_pipelined_detailed_write_interruption
      @script.fail_write(ConnectionError.with_config("write boom", nil))
      client = new_fake_client

      outcome = client.pipelined_detailed do |pipeline|
        COMMANDS.each { |command| pipeline.call_v(command) }
      end

      assert_predicate outcome, :write_failed?
      assert_equal 0, outcome.confirmed_count
      assert_equal COMMANDS, outcome.unconfirmed_commands
    end

    private

    def new_fake_client(**overrides)
      RedisClient.new(driver: FakeDriver, protocol: 2, **overrides)
    end
  end
end
