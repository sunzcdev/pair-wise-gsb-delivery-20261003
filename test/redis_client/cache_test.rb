# frozen_string_literal: true

require "test_helper"

class RedisClient
  class CacheTest < RedisClientTestCase
    # A scripted driver: tests queue "events" (replies, push messages, errors)
    # and the driver replays them from `read`, which lets tests control the
    # exact ordering of replies versus invalidation pushes.
    class FakeDriver
      include ConnectionMixin

      class << self
        def connections
          @connections ||= []
        end

        def events
          @events ||= []
        end

        def written
          @written ||= []
        end

        def reset!
          @connections = []
          @events = []
          @written = []
        end
      end

      def initialize(config, **)
        super(config)
        @connected = true
        @server_key = config.server_key
        self.class.connections << self
      end

      def connected?
        @connected
      end

      def close
        @connected = false
        super
      end

      def reconnect
        close
        @connected = true
        @server_key = @config.server_key
        true
      end

      def write(command)
        self.class.written << command
      end

      def write_multi(commands)
        commands.each { |command| write(command) }
      end

      def read(_timeout = nil)
        if self.class.events.empty?
          raise "FakeDriver: no scripted event (written: #{self.class.written.inspect})"
        end

        event = self.class.events.shift
        raise event if event.is_a?(Exception)

        event
      end
    end

    RedisClient.register_driver(:fake) { FakeDriver }

    Push = RESP3::Push

    def setup
      FakeDriver.reset!
      @cache = Cache.new
      @client = RedisClient.config(driver: :fake, cache: @cache).new_client
    end

    def teardown
      @client.close
    end

    def test_enables_tracking_in_connection_prelude
      stub_prelude
      stub_reply "value"
      assert_equal "value", @client.call("GET", "key")
      assert_includes written.map { |c| c[0..1] }, ["CLIENT", "TRACKING"]
      assert_equal(["CLIENT", "TRACKING", "ON"], written.find { |c| c[0..1] == ["CLIENT", "TRACKING"] })
    end

    def test_cacheable_command_is_cached
      stub_prelude
      stub_reply "value"
      assert_equal "value", @client.call("GET", "key")
      assert_equal "value", @client.call("GET", "key")
      assert_equal(1, written.count { |c| c[0] == "GET" })
    end

    def test_unknown_commands_are_never_cached
      stub_prelude
      2.times do
        stub_reply "OK"
        @client.call("SET", "key", "value")
      end
      2.times do
        stub_reply "PONG"
        @client.call("PING")
      end
      assert_equal(2, written.count { |c| c[0] == "SET" })
      assert_equal(2, written.count { |c| c[0] == "PING" })
    end

    def test_commands_not_matching_the_declared_shape_use_the_regular_path
      stub_prelude
      2.times do
        stub_reply 2
        @client.call("EXISTS", "key-1", "key-2")
      end
      assert_equal(2, written.count { |c| c[0] == "EXISTS" })

      stub_reply 1
      @client.call("EXISTS", "key-1")
      assert_equal 1, @client.call("EXISTS", "key-1")
      assert_equal(3, written.count { |c| c[0] == "EXISTS" })
    end

    def test_invalidation_push_evicts_the_key
      stub_prelude
      stub_reply "value-1"
      @client.call("GET", "key")

      stub_event Push.new(["invalidate", ["key"]])
      stub_reply "PONG"
      @client.call("PING") # drains the pending push

      stub_reply "value-2"
      assert_equal "value-2", @client.call("GET", "key")
      assert_equal "value-2", @client.call("GET", "key")
      assert_equal(2, written.count { |c| c == ["GET", "key"] })
    end

    def test_invalidation_push_only_evicts_listed_keys
      stub_prelude
      stub_reply "a"
      @client.call("GET", "key-a")
      stub_reply "b"
      @client.call("GET", "key-b")

      stub_event Push.new(["invalidate", ["key-a"]])
      stub_reply "PONG"
      @client.call("PING")

      assert_equal "b", @client.call("GET", "key-b")
      stub_reply "a-2"
      assert_equal "a-2", @client.call("GET", "key-a")
    end

    def test_late_reply_of_an_in_flight_read_is_not_cached
      stub_prelude
      # The invalidation for `key` is delivered while the GET is in flight,
      # before its reply: the reply is stale and must not be cached.
      stub_event Push.new(["invalidate", ["key"]])
      stub_reply "stale-value"
      assert_equal "stale-value", @client.call("GET", "key")

      stub_reply "fresh-value"
      assert_equal "fresh-value", @client.call("GET", "key")
      assert_equal "fresh-value", @client.call("GET", "key")
      assert_equal(2, written.count { |c| c == ["GET", "key"] })
    end

    def test_full_invalidation_advances_generation_and_clears
      stub_prelude
      stub_reply "value-1"
      @client.call("GET", "key")

      generation = @cache.generation
      stub_event Push.new(["invalidate", nil])
      stub_reply "PONG"
      @client.call("PING")

      assert_equal generation + 1, @cache.generation
      stub_reply "value-2"
      assert_equal "value-2", @client.call("GET", "key")
    end

    def test_close_advances_generation
      stub_prelude
      stub_reply "value"
      @client.call("GET", "key")

      generation = @cache.generation
      @client.close
      assert_equal generation + 1, @cache.generation
      assert_equal 0, @cache.size
    end

    def test_reconnect_reestablishes_tracking_and_advances_generation
      stub_prelude
      stub_reply "value-1"
      @client.call("GET", "key")
      @client.close

      generation = @cache.generation
      stub_prelude
      stub_reply "value-2"
      assert_equal "value-2", @client.call("GET", "key")

      assert_equal generation + 1, @cache.generation
      assert_equal(2, written.count { |c| c[0..1] == ["CLIENT", "TRACKING"] })
    end

    def test_missing_keys_are_cached
      stub_prelude
      stub_reply nil
      assert_nil @client.call("GET", "missing")
      assert_nil @client.call("GET", "missing")
      assert_equal(1, written.count { |c| c == ["GET", "missing"] })
    end

    def test_cached_values_are_deeply_frozen_and_shared
      stub_prelude
      stub_reply ["a", "b"]
      list = @client.call("LRANGE", "list", 0, -1)
      assert_predicate list, :frozen?
      assert list.all?(&:frozen?)
      assert_same list, @client.call("LRANGE", "list", 0, -1)

      stub_reply({ "field" => "value" })
      hash = @client.call("HGETALL", "hash")
      assert_predicate hash, :frozen?
      assert hash.keys.all?(&:frozen?)
      assert hash.values.all?(&:frozen?)
      assert_same hash, @client.call("HGETALL", "hash")
    end

    def test_pipelined_calls_process_interleaved_pushes
      stub_prelude
      stub_reply "value"
      @client.call("GET", "key")

      stub_event Push.new(["invalidate", ["key"]])
      stub_reply "p1"
      stub_reply "p2"
      results = @client.pipelined do |pipeline|
        pipeline.call("GET", "key")
        pipeline.call("GET", "other")
      end
      assert_equal ["p1", "p2"], results

      # The push interleaved in the pipeline evicted the key.
      stub_reply "fresh"
      assert_equal "fresh", @client.call("GET", "key")
    end

    def test_call_once_and_call_v_are_cached
      stub_prelude
      stub_reply "value"
      @client.call_once("GET", "key")
      assert_equal "value", @client.call_once_v(["GET", "key"])
      assert_equal "value", @client.call_v(["GET", "key"])
      assert_equal(1, written.count { |c| c[0] == "GET" })
    end

    def test_block_is_applied_to_cached_values
      stub_prelude
      stub_reply "value"
      assert_equal "VALUE", @client.call("GET", "key") { |v| v.upcase }
      assert_equal "VALUE", @client.call("GET", "key") { |v| v.upcase }
      assert_equal(1, written.count { |c| c[0] == "GET" })
    end

    def test_failed_read_does_not_leak_in_flight_reads
      client = RedisClient.config(driver: :fake, cache: @cache, reconnect_attempts: 1).new_client
      stub_prelude
      stub_event ConnectionError.new("boom")
      stub_prelude
      stub_reply "recovered"
      assert_equal "recovered", client.call("GET", "key")
      assert_equal "recovered", client.call("GET", "key")
      # One GET per attempt; the retried read was cached, so the second
      # call didn't reach the connection.
      assert_equal(2, written.count { |c| c == ["GET", "key"] })
    end

    def test_max_size_evicts_oldest_entries
      cache = Cache.new(max_size: 2)
      client = RedisClient.config(driver: :fake, cache: cache).new_client
      stub_prelude
      stub_reply "1"
      client.call("GET", "a")
      stub_reply "2"
      client.call("GET", "b")
      stub_reply "3"
      client.call("GET", "c")
      assert_equal 2, cache.size

      stub_reply "1b"
      assert_equal "1b", client.call("GET", "a") # "a" was evicted
      assert_equal "3", client.call("GET", "c") # still cached
    end

    def test_max_size_with_multiple_commands_on_the_same_key
      cache = Cache.new(max_size: 2)
      client = RedisClient.config(driver: :fake, cache: cache).new_client
      stub_prelude
      stub_reply "1"
      client.call("GET", "key")
      stub_reply({ "f" => "v" })
      client.call("HGETALL", "key")
      assert_equal 2, cache.size

      # Caching another key evicts the oldest key wholesale, not itself.
      stub_reply "x"
      client.call("GET", "other")
      assert_equal 1, cache.size
      assert_equal "x", client.call("GET", "other")

      stub_reply "2"
      assert_equal "2", client.call("GET", "key")
    end

    def test_custom_cacheable_commands
      cache = Cache.new(commands: { "MGET" => 1 })
      client = RedisClient.config(driver: :fake, cache: cache).new_client
      stub_prelude
      stub_reply %w(a b)
      client.call("MGET", "key-1", "key-2")
      assert_equal %w(a b), client.call("MGET", "key-1", "key-2")
      assert_equal(1, written.count { |c| c[0] == "MGET" })

      stub_reply "x"
      client.call("GET", "key-1")
      stub_reply "x"
      client.call("GET", "key-1")
      assert_equal(2, written.count { |c| c[0] == "GET" })
    end

    def test_cache_option_forms
      assert_instance_of Cache, RedisClient.config(cache: true).cache
      assert_nil RedisClient.config.cache
      assert_nil RedisClient.config(cache: false).cache

      cache = Cache.new
      assert_same cache, RedisClient.config(cache: cache).cache

      config = RedisClient.config(cache: { max_size: 42 })
      assert_instance_of Cache, config.cache

      error = assert_raises ArgumentError do
        RedisClient.config(cache: "yes")
      end
      assert_includes error.message, "cache:"
    end

    def test_cache_requires_resp3
      error = assert_raises ArgumentError do
        RedisClient.config(protocol: 2, cache: true)
      end
      assert_includes error.message, "RESP3"
    end

    def test_no_tracking_without_cache
      client = RedisClient.config(driver: :fake).new_client
      stub_reply "OK" # HELLO
      stub_reply "a"
      client.call("GET", "key")
      stub_reply "b"
      assert_equal "b", client.call("GET", "key")
      assert_empty(written.select { |c| c[0..1] == ["CLIENT", "TRACKING"] })
    end

    private

    def written
      FakeDriver.written
    end

    def stub_event(event)
      FakeDriver.events << event
    end

    def stub_reply(reply)
      stub_event(reply)
    end

    def stub_prelude
      stub_reply "OK" # HELLO 3
      stub_reply "OK" # CLIENT TRACKING ON
    end
  end
end
