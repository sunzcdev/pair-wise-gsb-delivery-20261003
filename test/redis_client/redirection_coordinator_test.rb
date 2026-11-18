# frozen_string_literal: true

require "test_helper"

require "redis_client/redirection_coordinator"

class RedisClient
  class RedirectionCoordinatorTest < RedisClientTestCase
    # In-memory client, no redis-server required. Each #call_v pops the next
    # scripted response; a CommandError is raised instead of returned.
    class ScriptedClient
      attr_reader :commands, :name

      def initialize(name, responses = [])
        @name = name
        @responses = responses
        @commands = []
      end

      def call_v(command)
        @commands << command
        response = @responses.shift
        raise response if response.is_a?(CommandError)

        response
      end

      # Mimics RedisClient#with / Pooled#with: yields the underlying connection
      # and returns the block's result.
      def with(_options = nil)
        yield self
      end
    end

    # Minimal connection pool wrapper: every #with checkout yields the very same
    # underlying connection, which is how a real pool behaves for a single
    # thread. Used to assert ASKING never leaks across checkouts.
    class FakePool
      def initialize(client)
        @client = client
      end

      def with(_options = nil)
        yield @client
      end
    end

    def setup
      super
      @targets = {}
      @factory_calls = []
      @factory = ->(redirection) do
        @factory_calls << redirection
        @targets.fetch(redirection.address)
      end
      @coordinator = RedirectionCoordinator.new(@factory)
    end

    # Redirection parsing

    def test_parse_moved
      redirection = parse("MOVED 3999 127.0.0.1:6381")

      assert_equal 3999, redirection.slot
      assert_equal "127.0.0.1", redirection.host
      assert_equal 6381, redirection.port
      assert_equal :moved, redirection.kind
      assert_predicate redirection, :moved?
      refute_predicate redirection, :ask?
      assert_predicate redirection, :cacheable?
      refute_predicate redirection, :ipv6?
      assert_equal "127.0.0.1:6381", redirection.address
    end

    def test_parse_ask
      redirection = parse("ASK 12182 127.0.0.1:6381")

      assert_equal 12182, redirection.slot
      assert_equal :ask, redirection.kind
      assert_predicate redirection, :ask?
      refute_predicate redirection, :moved?
      refute_predicate redirection, :cacheable?
    end

    def test_parse_bracketed_ipv6
      redirection = parse("ASK 12182 [2001:db8::1]:6381")

      assert_equal "2001:db8::1", redirection.host
      assert_equal 6381, redirection.port
      assert_predicate redirection, :ipv6?
      assert_equal "[2001:db8::1]:6381", redirection.address
    end

    def test_parse_bare_ipv6
      redirection = parse("MOVED 7 ::1:6379")

      assert_equal "::1", redirection.host
      assert_equal 6379, redirection.port
      assert_predicate redirection, :ipv6?
      assert_equal "[::1]:6379", redirection.address
    end

    def test_parse_full_ipv6_without_brackets
      redirection = parse("MOVED 3999 2001:db8:0:0:0:0:0:1:6381")

      assert_equal "2001:db8:0:0:0:0:0:1", redirection.host
      assert_equal 6381, redirection.port
      assert_equal "[2001:db8:0:0:0:0:0:1]:6381", redirection.address
    end

    def test_parse_tolerates_surrounding_whitespace
      redirection = parse("  MOVED 1 10.0.0.1:6379  ")

      assert_equal :moved, redirection.kind
      assert_equal 1, redirection.slot
    end

    def test_parse_returns_nil_for_unrelated_errors
      assert_nil RedirectionCoordinator::Redirection.parse(command_error("WRONGTYPE nope"))
      assert_nil RedirectionCoordinator::Redirection.parse(command_error("ERR something else"))
    end

    def test_redirection_equality_and_identity
      a = parse("MOVED 1 10.0.0.1:6379")
      b = parse("MOVED 1 10.0.0.1:6379")
      c = parse("ASK 1 10.0.0.1:6379")

      assert_equal a, b
      assert_equal a.hash, b.hash
      refute_equal a, c
      assert_equal [:moved, 1, "10.0.0.1", 6379], a.loop_key
    end

    # Construction

    def test_requires_a_callable_factory
      assert_raises ArgumentError do
        RedirectionCoordinator.new(nil)
      end
    end

    def test_requires_positive_max_hops
      assert_raises ArgumentError do
        RedirectionCoordinator.new(@factory, max_hops: 0)
      end
    end

    def test_exposes_max_hops_with_a_default
      assert_equal RedirectionCoordinator::DEFAULT_MAX_HOPS, @coordinator.max_hops
      assert_equal 3, RedirectionCoordinator.new(@factory, max_hops: 3).max_hops
    end

    # Coordination

    def test_returns_immediately_when_no_redirection
      origin = ScriptedClient.new("origin", ["PONG"])

      assert_equal "PONG", @coordinator.call(origin, ["PING"])
      assert_equal [["PING"]], origin.commands
      assert_empty @factory_calls
    end

    def test_follows_moved_and_uses_factory_target
      target = ScriptedClient.new("target", ["OK"])
      origin = ScriptedClient.new("origin", [command_error("MOVED 3999 10.0.0.2:6380")])
      @targets["10.0.0.2:6380"] = target

      assert_equal "OK", @coordinator.call(origin, ["GET", "key"])

      assert_equal [["GET", "key"]], origin.commands
      assert_equal [["GET", "key"]], target.commands
      assert_equal 1, @factory_calls.size
      redirection = @factory_calls.first
      assert_predicate redirection, :moved?
      assert_equal "10.0.0.2:6380", redirection.address
    end

    def test_follows_ask_with_asking_on_the_same_session
      target = ScriptedClient.new("target", ["OK", "OK"])
      origin = ScriptedClient.new("origin", [command_error("ASK 12182 10.0.0.3:6381")])
      @targets["10.0.0.3:6381"] = FakePool.new(target)

      assert_equal "OK", @coordinator.call(origin, ["SET", "key", "v"])

      assert_equal [["ASKING"], ["SET", "key", "v"]], target.commands
      redirection = @factory_calls.first
      assert_predicate redirection, :ask?
      refute_predicate redirection, :cacheable?
    end

    def test_asking_does_not_leak_state_to_other_pool_users
      target = ScriptedClient.new("target", ["OK", "OK", "PONG"])
      pool = FakePool.new(target)
      origin = ScriptedClient.new("origin", [command_error("ASK 8 10.0.0.3:6381")])
      @targets["10.0.0.3:6381"] = pool

      @coordinator.call(origin, ["GET", "key"])

      # A subsequent, unrelated checkout must not see a leftover ASKING command.
      pool.with { |connection| connection.call_v(["PING"]) }
      assert_equal [["ASKING"], ["GET", "key"], ["PING"]], target.commands
    end

    def test_follows_chained_moved_and_ask_redirections
      moved_target = ScriptedClient.new("moved", [command_error("ASK 9 10.0.0.4:6382")])
      ask_target = ScriptedClient.new("ask", ["OK", "DONE"])
      origin = ScriptedClient.new("origin", [command_error("MOVED 9 10.0.0.3:6381")])
      @targets["10.0.0.3:6381"] = moved_target
      @targets["10.0.0.4:6382"] = ask_target

      assert_equal "DONE", @coordinator.call(origin, ["GET", "key"])

      assert_equal [["ASKING"], ["GET", "key"]], ask_target.commands
      assert_equal [:moved, :ask], @factory_calls.map(&:kind)
    end

    def test_raises_loop_error_when_redirected_back_to_the_same_target
      target = ScriptedClient.new("target", [command_error("MOVED 5 10.0.0.9:6379")])
      origin = ScriptedClient.new("origin", [command_error("MOVED 5 10.0.0.9:6379")])
      @targets["10.0.0.9:6379"] = target

      error = assert_raises(RedirectionCoordinator::RedirectionLoopError) do
        @coordinator.call(origin, ["GET", "key"])
      end
      assert_includes error.message, "10.0.0.9:6379"
    end

    def test_raises_limit_error_when_hop_cap_is_reached
      # Two distinct targets pointing at each other would loop; instead build a
      # strictly increasing chain that exceeds the cap without repeating.
      clients = {}
      3.times do |i|
        port = 7000 + i + 1
        clients["10.0.0.#{i}:700#{i}"] =
          ScriptedClient.new("node-#{i}", [command_error("MOVED #{i} 10.0.0.#{i + 1}:#{port}")])
      end
      origin = ScriptedClient.new("origin", [command_error("MOVED 0 10.0.0.1:7001")])
      @targets.merge!(clients)
      coordinator = RedirectionCoordinator.new(@factory, max_hops: 2)

      error = assert_raises(RedirectionCoordinator::RedirectionLimitError) do
        coordinator.call(origin, ["GET", "key"])
      end
      assert_includes error.message, "2 redirection hops"
      assert_equal 2, @factory_calls.size
    end

    def test_max_hops_one_allows_a_single_redirect_but_not_two
      first = ScriptedClient.new("first", [command_error("MOVED 2 10.0.0.3:6380")])
      second = ScriptedClient.new("second", ["DONE"])
      @targets["10.0.0.2:6380"] = first
      @targets["10.0.0.3:6380"] = second
      origin = ScriptedClient.new("origin", [command_error("MOVED 2 10.0.0.2:6380")])

      # One redirect is followed, the second hits the cap of one hop.
      assert_raises(RedirectionCoordinator::RedirectionLimitError) do
        RedirectionCoordinator.new(@factory, max_hops: 1).call(origin, ["GET", "key"])
      end

      # With enough hops the same chain converges.
      first.instance_variable_get(:@responses) << command_error("MOVED 2 10.0.0.3:6380")
      converging_origin = ScriptedClient.new("origin-2", [command_error("MOVED 2 10.0.0.2:6380")])
      assert_equal "DONE",
        RedirectionCoordinator.new(@factory, max_hops: 2).call(converging_origin, ["GET", "key"])
    end

    def test_non_redirection_command_error_is_re_raised
      origin = ScriptedClient.new("origin", [command_error("WRONGTYPE Operation against a key")])

      error = assert_raises(CommandError) do
        @coordinator.call(origin, ["GET", "key"])
      end
      assert_equal "WRONGTYPE", error.code
      assert_empty @factory_calls
    end

    def test_factory_error_propagates
      origin = ScriptedClient.new("origin", [command_error("MOVED 1 10.0.0.2:6380")])
      factory = ->(_redirection) { raise "node not provisioned" }
      coordinator = RedirectionCoordinator.new(factory)

      assert_raises(RuntimeError, "node not provisioned") do
        coordinator.call(origin, ["GET", "key"])
      end
    end

    def test_follows_ipv6_moved_target
      target = ScriptedClient.new("v6-target", ["OK"])
      origin = ScriptedClient.new("origin", [command_error("MOVED 3 [2001:db8::1]:6381")])
      @targets["[2001:db8::1]:6381"] = target

      assert_equal "OK", @coordinator.call(origin, ["GET", "key"])
      assert_equal "[2001:db8::1]:6381", @factory_calls.first.address
    end

    def test_uses_call_v_for_moved_without_asking
      target = ScriptedClient.new("target", ["OK"])
      origin = ScriptedClient.new("origin", [command_error("MOVED 2 10.0.0.5:6383")])
      @targets["10.0.0.5:6383"] = target

      @coordinator.call(origin, ["GET", "key"])

      assert_equal [["GET", "key"]], target.commands
    end

    def test_is_thread_safe_with_independent_command_chains
      # Coordinators hold no per-command state: hops and seen keys are local to
      # #call, so concurrent commands must not influence one another.
      target_a = ScriptedClient.new("a", ["A"])
      target_b = ScriptedClient.new("b", ["B"])
      @targets["10.0.0.1:6379"] = target_a
      @targets["10.0.0.2:6379"] = target_b
      origin_a = ScriptedClient.new("origin-a", [command_error("MOVED 1 10.0.0.1:6379")])
      origin_b = ScriptedClient.new("origin-b", [command_error("MOVED 2 10.0.0.2:6379")])

      results = [origin_a, origin_b].map do |origin|
        Thread.new { @coordinator.call(origin, ["GET", "key"]) }.value
      end

      assert_equal ["A", "B"], results
    end

    # Pipelines and transactions are refused up front.

    def test_pipelined_is_refused
      assert_raises(RedirectionCoordinator::BatchNotSupportedError) do
        @coordinator.pipelined { }
      end
    end

    def test_multi_is_refused
      assert_raises(RedirectionCoordinator::BatchNotSupportedError) do
        @coordinator.multi { }
      end
    end

    private

    def parse(message)
      RedirectionCoordinator::Redirection.parse(command_error(message))
    end

    def command_error(message)
      CommandError.parse(message)
    end
  end
end
