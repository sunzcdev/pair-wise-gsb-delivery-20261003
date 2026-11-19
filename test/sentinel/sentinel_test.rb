# frozen_string_literal: true

require "test_helper"

class RedisClient
  class SentinelTest < RedisClientTestCase
    include ClientTestHelper

    def setup
      @config = new_config
      super
      @config = new_config
    end

    def check_server
      retried = false
      begin
        @redis = @config.new_client
        @redis.call("FLUSHDB")
      rescue
        if retried
          raise
        else
          retried = true
          Servers::SENTINEL_TESTS.reset
          retry
        end
      end
    end

    def teardown
      Servers::SENTINEL_TESTS.reset
      super
    end

    def test_new_client
      assert_equal "PONG", @config.new_client.call("PING")
    end

    def test_url_list
      sentinel_config = new_config(sentinels: Servers::SENTINELS.map { |s| "redis://#{s.host}:#{s.port}" })
      assert_equal "PONG", sentinel_config.new_client.call("PING")
    end

    def test_sentinel_config
      assert_equal [Servers::REDIS.host, Servers::REDIS.port], [@config.host, @config.port]
    end

    def test_sentinel_down
      assert_equal [Servers::REDIS.host, Servers::REDIS.port], [@config.host, @config.port]

      original_order = Servers::SENTINELS.map(&:port)
      assert_equal original_order, @config.sentinels.map(&:port)
      Toxiproxy[/sentinel_(1|3)/].down do
        @config.reset
        assert_equal [Servers::REDIS.host, Servers::REDIS.port], [@config.host, @config.port]
        expected_order = [Servers::SENTINELS[1].port, Servers::SENTINELS[0].port, Servers::SENTINELS[2].port]
        assert_equal expected_order, @config.sentinels.map(&:port)
      end
    end

    def test_sentinel_all_down
      Toxiproxy[/sentinel_.*/].down do
        assert_raises RedisClient::ConnectionError do
          @config.new_client.call("PING")
        end
      end
    end

    class SentinelClientMock
      attr_reader :close_count

      def initialize(responses)
        @responses = responses
        @close_count = 0
      end

      def call(*args, &block)
        call_v(args, &block)
      end

      def call_v(command)
        expected_command, response = @responses.shift
        if expected_command == command
          if block_given?
            yield response
          else
            response
          end
        else
          raise "Expected #{expected_command.inspect}, got: #{command.inspect}"
        end
      end

      def close
        @close_count += 1
      end
    end

    # Mock that only has call_v, simulating Redis::Client which undefs call
    class CallVOnlySentinelClientMock < SentinelClientMock
      undef_method :call
    end

    def test_unknown_master
      @config = new_config(name: "does-not-exist")
      client = @config.new_client
      assert_raises ConnectionError do
        client.call("PING")
      end
    end

    def test_unresolved_config
      client = @config.new_client

      stub(@config, :server_url, -> { raise ConnectionError.with_config('this should not be called', @config) }) do
        stub(@config, :resolved?, false) do
          stub(client, :call, ->(_) { raise ConnectionError.with_config('call error', @config) }) do
            error = assert_raises ConnectionError do
              client.call('PING')
            end

            assert_equal 'call error', error.message
          end
        end
      end
    end

    def test_master_failover_not_ready
      sentinel_client_mock = SentinelClientMock.new([
        [["SENTINEL", "get-master-addr-by-name", "cache"], [Servers::REDIS_REPLICA.host, Servers::REDIS_REPLICA.port.to_s]],
        sentinel_refresh_command_mock,
        [["SENTINEL", "get-master-addr-by-name", "cache"], [Servers::REDIS_REPLICA.host, Servers::REDIS_REPLICA.port.to_s]],
        sentinel_refresh_command_mock,
        [["SENTINEL", "get-master-addr-by-name", "cache"], [Servers::REDIS_REPLICA.host, Servers::REDIS_REPLICA.port.to_s]],
        sentinel_refresh_command_mock,
      ])
      stub(@config, :sentinel_client, ->(_config) { sentinel_client_mock }) do
        client = @config.new_client
        assert_raises FailoverError do
          client.call("PING")
        end
      end
    end

    def test_master_failover_ready
      sentinel_client_mock = SentinelClientMock.new([
        [["SENTINEL", "get-master-addr-by-name", "cache"], [Servers::REDIS.host, Servers::REDIS.port.to_s]],
        sentinel_refresh_command_mock,
        [["SENTINEL", "get-master-addr-by-name", "cache"], [Servers::REDIS_REPLICA.host, Servers::REDIS_REPLICA.port.to_s]],
        sentinel_refresh_command_mock,
      ])
      replica = RedisClient.new(host: Servers::REDIS_REPLICA.host, port: Servers::REDIS_REPLICA.port)
      assert_equal "OK", replica.call("REPLICAOF", "NO", "ONE")

      stub(@config, :sentinel_client, ->(_config) { sentinel_client_mock }) do
        client = @config.new_client
        assert_equal "PONG", client.call("PING")
        initial_server_key = @config.server_key

        Toxiproxy[Servers::REDIS.name].down do
          assert_equal "PONG", client.call("PING")
          refute_equal initial_server_key, @config.server_key
        end
      end
    ensure
      replica = RedisClient.new(host: Servers::REDIS_REPLICA.host, port: Servers::REDIS_REPLICA.port)
      assert_equal "OK", replica.call("REPLICAOF", Servers::REDIS.host, Servers::REDIS.port)
    end

    def test_no_replicas
      @config = new_config(role: :replica)
      tries = Servers::SENTINELS.size * (SentinelConfig::DEFAULT_RECONNECT_ATTEMPTS + 1)
      sentinel_client_mock = SentinelClientMock.new([
        [["SENTINEL", "replicas", "cache"], []],
      ] * tries)
      stub(@config, :sentinel_client, ->(_config) { sentinel_client_mock }) do
        assert_raises ConnectionError do
          @config.new_client.call("PING")
        end
      end
    end

    def test_replica_failover_not_ready
      @config = new_config(role: :replica)
      sentinel_client_mock = SentinelClientMock.new([
        [["SENTINEL", "replicas", "cache"], [response_hash("ip" => Servers::REDIS_REPLICA.host, "port" => Servers::REDIS_REPLICA.port.to_s, "flags" => "slave")]],
        [["SENTINEL", "replicas", "cache"], [response_hash("ip" => Servers::REDIS.host, "port" => Servers::REDIS.port.to_s, "flags" => "slave")]],
        [["SENTINEL", "replicas", "cache"], [response_hash("ip" => Servers::REDIS.host, "port" => Servers::REDIS.port.to_s, "flags" => "slave")]],
      ])
      stub(@config, :sentinel_client, ->(_config) { sentinel_client_mock }) do
        client = @config.new_client
        assert_equal "PONG", client.call("PING")

        Toxiproxy[Servers::REDIS_REPLICA.name].down do
          assert_raises FailoverError do
            client.call("PING")
          end
        end
      end
    end

    def test_replica_failover_ready
      @config = new_config(role: :replica)
      sentinel_client_mock = SentinelClientMock.new([
        [["SENTINEL", "replicas", "cache"], [response_hash("ip" => Servers::REDIS.host, "port" => Servers::REDIS.port.to_s, "flags" => "slave")]],
        [["SENTINEL", "replicas", "cache"], [response_hash("ip" => Servers::REDIS_REPLICA.host, "port" => Servers::REDIS_REPLICA.port.to_s, "flags" => "slave")]],
        [["SENTINEL", "replicas", "cache"], [response_hash("ip" => Servers::REDIS_REPLICA.host, "port" => Servers::REDIS_REPLICA.port.to_s, "flags" => "slave")]],
        [["SENTINEL", "replicas", "cache"], [response_hash("ip" => Servers::REDIS_REPLICA.host, "port" => Servers::REDIS_REPLICA.port.to_s, "flags" => "slave")]],
      ])

      stub(@config, :sentinel_client, ->(_config) { sentinel_client_mock }) do
        client = @config.new_client
        assert_equal "PONG", client.call("PING")
      end
    end

    def test_successful_connection_refreshes_sentinels_list
      assert_equal Servers::SENTINELS.length, @config.sentinels.length

      new_sentinel_ip = "10.0.0.1"
      new_sentinel_port = 1234

      # Trigger sentinel refresh to make the client aware of a new sentinel
      sentinel_client_mock = SentinelClientMock.new([
        [["SENTINEL", "get-master-addr-by-name", "cache"], [Servers::REDIS.host, Servers::REDIS.port.to_s]],
        sentinel_refresh_command_mock(
          additional_sentinels: [response_hash("ip" => new_sentinel_ip, "port" => new_sentinel_port.to_s)],
        ),
      ])
      stub(@config, :sentinel_client, ->(_config) { sentinel_client_mock }) do
        client = @config.new_client
        assert_equal "PONG", client.call("PING")
      end

      assert_equal Servers::SENTINELS.length + 1, @config.sentinels.length
      assert_equal new_sentinel_ip, @config.sentinels.last.host
      assert_equal new_sentinel_port, @config.sentinels.last.port
      assert_equal 1, sentinel_client_mock.close_count
    end

    def test_sentinel_refresh_password
      @config = new_config(sentinel_password: "hunter2")

      assert_equal Servers::SENTINELS.length, @config.sentinels.length

      new_sentinel_ip = "10.0.0.1"
      new_sentinel_port = 1234

      # Trigger sentinel refresh to make the client aware of a new sentinel
      sentinel_client_mock = SentinelClientMock.new([
        [["SENTINEL", "get-master-addr-by-name", "cache"], [Servers::REDIS.host, Servers::REDIS.port.to_s]],
        sentinel_refresh_command_mock(
          additional_sentinels: [response_hash("ip" => new_sentinel_ip, "port" => new_sentinel_port.to_s)],
        ),
      ])

      stub(@config, :sentinel_client, ->(_config) { sentinel_client_mock }) do
        client = @config.new_client
        assert_equal "PONG", client.call("PING")
      end

      assert_equal Servers::SENTINELS.length + 1, @config.sentinels.length
      @config.sentinels.each do |sentinel|
        refute_nil sentinel.password
      end

      assert_equal 1, sentinel_client_mock.close_count
    end

    def test_hide_sentinel_password
      config = new_config(sentinel_password: "PASSWORD")
      refute_match "PASSWORD", config.inspect
      refute_match "PASSWORD", config.to_s
    end

    def test_sentinel_client_without_call_method
      # Simulates Redis::Client which undefs call method
      sentinel_client_mock = CallVOnlySentinelClientMock.new([
        [["SENTINEL", "get-master-addr-by-name", "cache"], [Servers::REDIS.host, Servers::REDIS.port.to_s]],
        sentinel_refresh_command_mock,
      ])
      stub(@config, :sentinel_client, ->(_config) { sentinel_client_mock }) do
        assert_equal [Servers::REDIS.host, Servers::REDIS.port], [@config.host, @config.port]
      end
    end

    def test_config_user_password_from_url_for_redis_master_replica_only
      config = new_config(url: "redis://george:hunter2@cache/10", name: nil)
      assert_equal "hunter2", config.password
      assert_equal "george", config.username
      assert_equal 10, config.db
      assert_equal [Servers::REDIS.host, Servers::REDIS.port], [config.host, config.port]

      config.sentinels.each do |sentinel|
        assert_nil sentinel.password
      end
    end

    def test_config_ssl_from_rediss_url
      sentinel_client_mock = SentinelClientMock.new([
        [["SENTINEL", "get-master-addr-by-name", "cache"], [Servers::REDIS.host, Servers::REDIS.port.to_s]],
        sentinel_refresh_command_mock,
      ])

      config = new_config(url: "rediss://george:hunter2@cache/10", name: nil)

      stub(config, :sentinel_client, ->(_config) { sentinel_client_mock }) do
        assert_equal "hunter2", config.password
        assert_equal "george", config.username
        assert_equal 10, config.db
        assert_predicate config, :ssl?
        assert_equal [Servers::REDIS.host, Servers::REDIS.port], [config.host, config.port]

        config.sentinels.each do |sentinel|
          assert_nil sentinel.password
          refute_predicate sentinel, :ssl?
        end
      end
    end

    def test_explicit_ssl_option_overrides_url
      sentinel_client_mock = SentinelClientMock.new([
        [["SENTINEL", "get-master-addr-by-name", "cache"], [Servers::REDIS.host, Servers::REDIS.port.to_s]],
        sentinel_refresh_command_mock,
      ])

      # Passing ssl: true should enable SSL for both Redis and Sentinel configs
      config = new_config(ssl: true)

      stub(config, :sentinel_client, ->(_config) { sentinel_client_mock }) do
        assert_predicate config, :ssl?, "Expected Redis config to use SSL when explicitly passed"

        config.sentinels.each do |sentinel|
          assert_predicate sentinel, :ssl?, "Expected Sentinel config to use SSL when explicitly passed"
        end
      end
    end

    def test_sentinel_shared_username_password
      config = new_config(sentinel_username: "alice", sentinel_password: "superpassword")

      config.sentinels.each do |sentinel|
        assert_equal "alice", sentinel.username
        assert_equal "superpassword", sentinel.password
      end
    end

    def test_sentinel_explicit_username_password
      sentinels = Servers::SENTINELS.map do |sentinel|
        { host: sentinel.host, port: sentinel.port, username: "alice", password: "superpassword" }
      end

      config = new_config(sentinels: sentinels)
      config.sentinels.each do |sentinel|
        assert_equal "alice", sentinel.username
        assert_equal "superpassword", sentinel.password
      end
    end

    def test_sentinel_config_from_url
      sentinels = Servers::SENTINELS.map do |sentinel|
        "redis://alice:superpassword@#{sentinel.host}:#{sentinel.port}"
      end

      config = new_config(sentinels: sentinels)
      assert_equal Servers::SENTINELS.length, config.sentinels.length
      config.sentinels.each do |sentinel|
        assert_equal "alice", sentinel.username
        assert_equal "superpassword", sentinel.password
      end
    end

    private

    def response_hash(hash)
      hash
    end

    def new_config(**kwargs)
      RedisClient.sentinel(
        name: Servers::SENTINEL_NAME,
        sentinels: Servers::SENTINELS.map do |sentinel|
          { host: sentinel.host, port: sentinel.port }
        end,
        timeout: 0.1,
        driver: ENV.fetch("DRIVER", "ruby").to_sym,
        **kwargs,
      )
    end

    def sentinel_refresh_command_mock(additional_sentinels: [])
      sentinels = Servers::SENTINELS.map do |sentinel|
        response_hash("ip" => sentinel.host, "port" => sentinel.port.to_s)
      end

      [["SENTINEL", "sentinels", "cache"], sentinels.concat(additional_sentinels)]
    end
  end

  class RESP2SentinelTest < SentinelTest
    def test_sentinel_refresh_password
      skip("On RESP2, AUTH require the redis server to have a default password")
    end

    private

    def new_config(**kwargs)
      super(**kwargs, protocol: 2)
    end

    def response_hash(hash)
      # In RESP2 hashes are returned as flat arrays
      hash.to_a.flatten(1)
    end
  end

  # Exercises role verification and connection replacement without a real
  # cluster: Sentinels and Redis nodes are stand-ins whose roles and
  # reachability can be flipped between switches.
  class SentinelDiscoveryTest < RedisClientTestCase
    # A node stand-in. It answers ROLE and simple commands, tracks how many
    # connections were opened/closed, and can be taken down to simulate a node
    # that has been demoted but still accepts Sentinel traffic.
    class NodeDouble
      attr_reader :name, :host, :port, :opens, :closes, :commands
      attr_accessor :role

      def initialize(name, host, port, role)
        @name = name
        @host = host
        @port = port
        @role = role
        @opens = 0
        @closes = 0
        @commands = []
        @up = true
      end

      def up!
        @up = true
      end

      def down!
        @up = false
      end

      def up?
        @up
      end

      def address
        "#{host}:#{port}"
      end

      def server_key
        [nil, host, port].freeze
      end
    end

    # Raw connection bound to a NodeDouble.
    class NodeConnection
      include ConnectionMixin

      attr_accessor :read_timeout, :write_timeout, :lose_reply_after

      def initialize(config, node)
        super(config)
        @node = node
        @written = []
        @server_key = node.server_key
        @lose_reply_after = nil
        node.instance_variable_set(:@opens, node.opens + 1)
      end

      def connect
        @server_key = @node.server_key
        true
      end

      def connected?
        @node.up?
      end

      def close
        @node.instance_variable_set(:@closes, @node.closes + 1)
        super
      end

      def write(command)
        raise connection_error("#{@node.name} is down") unless @node.up?

        @written << command
        @node.commands << command
      end

      def write_multi(commands)
        commands.each { |command| write(command) }
      end

      def read(_timeout = nil)
        raise connection_error("#{@node.name} is down") unless @node.up?

        command = @written.shift
        if @lose_reply_after
          if @lose_reply_after <= 0
            @lose_reply_after = nil
            raise connection_error("reply lost on #{@node.name}")
          end
          @lose_reply_after -= 1
        end

        case command.first
        when "ROLE" then [@node.role, 0, []]
        when "PING" then "PONG"
        when "GET" then "value"
        else "OK"
        end
      end

      def measure_round_trip_delay
        0.0
      end
    end

    # Driver resolving host:port to a NodeDouble.
    class NodeDriver < NodeConnection
      class << self
        attr_accessor :registry
      end

      self.registry = {}

      def self.register(node)
        registry[node.address] = node
      end

      def self.ssl_context(_params)
        nil
      end

      def initialize(config, connect_timeout:, read_timeout:, write_timeout:)
        node = self.class.registry.fetch("#{config.host}:#{config.port}")
        super(config, node)
        self.read_timeout = read_timeout
        self.write_timeout = write_timeout
      end
    end

    # A Sentinel stand-in with a swappable master and per-call close tracking.
    class SentinelDouble
      attr_accessor :master, :replicas, :error

      def initialize(master: nil, replicas: [], error: nil)
        @master = master
        @replicas = replicas
        @error = error
      end

      def call_v(command)
        raise error if error

        case command[1]
        when "get-master-addr-by-name"
          master && [master.host, master.port.to_s]
        when "replicas"
          replicas.map { |node| { "ip" => node.host, "port" => node.port.to_s, "flags" => "slave" } }
        when "sentinels"
          []
        else
          raise "unexpected #{command.inspect}"
        end
      end

      def close
        nil
      end
    end

    def setup
      super
      @previous_registry = NodeDriver.registry
      NodeDriver.registry = {}
      @master = register(NodeDouble.new("master", "10.0.1.1", 6379, "master"))
      @replica = register(NodeDouble.new("replica", "10.0.1.2", 6379, "slave"))
    end

    def teardown
      NodeDriver.registry = @previous_registry
      super
    end

    def test_stale_master_answer_is_rejected_by_role_probe
      sentinel = SentinelDouble.new(master: @replica)
      config = new_config(sentinels: [{ host: "s1", port: 26379, client: sentinel }])

      snapshot = config.discover
      candidate = snapshot.candidates.first

      assert_equal @replica.address, candidate.address
      assert_equal "s1:26379", candidate.sentinel_address
      assert_equal "slave", candidate.role
      assert_predicate candidate, :rejected?
      assert_instance_of FailoverError, candidate.error
      assert_nil snapshot.selected

      error = assert_raises SentinelUnavailableError do
        config.resolve!
      end
      refute_nil error.discovery
      assert_predicate error.discovery.candidates.first, :rejected?
      refute_predicate config, :resolved?
    end

    def test_confirmed_master_is_published
      sentinel = SentinelDouble.new(master: @master)
      config = new_config(sentinels: [{ host: "s1", port: 26379, client: sentinel }])

      snapshot = config.discover
      candidate = snapshot.candidates.first

      assert_equal "master", candidate.role
      assert_predicate candidate, :confirmed?
      assert_equal @master.address, snapshot.selected.address

      config.resolve!
      assert_equal @master.host, config.host
      assert_equal @master.port, config.port
    end

    def test_disagreeing_sentinels_without_a_majority_are_reported
      @replica.role = "master"
      s1 = SentinelDouble.new(master: @master)
      s2 = SentinelDouble.new(master: @replica)
      config = new_config(
        sentinels: [
          { host: "s1", port: 26379, client: s1 },
          { host: "s2", port: 26380, client: s2 },
        ],
        consensus: true
      )

      snapshot = config.discover
      assert_predicate snapshot, :disagreement?
      assert_equal ["s1:26379", "s2:26380"], snapshot.candidates.map(&:sentinel_address).sort

      error = assert_raises SentinelDisagreementError do
        config.resolve!
      end
      assert_includes error.message, @master.address
      assert_includes error.message, @replica.address
      refute_predicate config, :resolved?
    end

    def test_all_sentinels_failing_lists_each_source
      s1 = SentinelDouble.new(error: ConnectionError.new("first sentinel down"))
      s2 = SentinelDouble.new(error: ConnectionError.new("second sentinel down"))
      config = new_config(
        sentinels: [
          { host: "s1", port: 26379, client: s1 },
          { host: "s2", port: 26380, client: s2 },
        ]
      )

      snapshot = config.discover
      assert_predicate snapshot, :all_failed?
      assert_equal 2, snapshot.failures.size

      error = assert_raises SentinelUnavailableError do
        config.resolve!
      end
      assert_includes error.message, "s1:26379"
      assert_includes error.message, "s2:26380"
      assert_includes error.message, "first sentinel down"
    end

    def test_role_mismatch_evicts_only_the_rejected_candidate
      sentinel = SentinelDouble.new(master: @replica)
      config = new_config(sentinels: [{ host: "s1", port: 26379, client: sentinel }])

      # The lazy path publishes the Sentinel answer immediately; the connection
      # prelude then verifies the role before the connection is handed out.
      assert_equal @replica.host, config.host
      refute_predicate config, :confirmed?

      assert_raises FailoverError do
        config.check_role!("slave")
      end

      refute_predicate config, :resolved?
      assert_nil config.selected_candidate
      assert_predicate config.discovery.candidates.first, :rejected?
    end

    def test_in_flight_write_is_not_migrated
      sentinel = SentinelDouble.new(master: @master)
      config = new_config(
        sentinels: [{ host: "s1", port: 26379, client: sentinel }],
        reconnect_attempts: 2
      )
      client = config.new_client
      assert_equal "PONG", client.call("PING")

      raw = client.instance_variable_get(:@raw_connection)
      raw.lose_reply_after = 0

      error = assert_raises(ConnectionError) do
        client.call("SET", "counter", "1")
      end

      assert_predicate error, :unknown_outcome?
      refute_predicate error, :replayable?
      assert_equal [["SET", "counter", "1"]], error.in_flight_commands

      # The write was sent once and never replayed on another node.
      assert_equal 1, @master.commands.count { |command| command.first == "SET" }
      assert_equal 0, @replica.commands.count { |command| command.first == "SET" }
    end

    def test_in_flight_read_is_replayable
      sentinel = SentinelDouble.new(master: @master)
      config = new_config(
        sentinels: [{ host: "s1", port: 26379, client: sentinel }],
        reconnect_attempts: 1
      )
      client = config.new_client
      client.call("PING")

      client.instance_variable_get(:@raw_connection).lose_reply_after = 0
      assert_equal "value", client.call("GET", "key")
    end

    def test_pipeline_preserves_acked_prefix_and_drops_the_rest
      sentinel = SentinelDouble.new(master: @master)
      config = new_config(
        sentinels: [{ host: "s1", port: 26379, client: sentinel }],
        reconnect_attempts: 1
      )
      client = config.new_client
      client.call("PING")

      client.instance_variable_get(:@raw_connection).lose_reply_after = 1

      error = assert_raises(ConnectionError) do
        client.pipelined do |pipeline|
          pipeline.call("GET", "a")
          pipeline.call("SET", "b", "2")
          pipeline.call("SET", "c", "3")
        end
      end

      assert_equal ["value"], error.partial_results
      assert_equal [["SET", "b", "2"], ["SET", "c", "3"]], error.in_flight_commands
      refute_predicate error, :replayable?
      assert_equal 1, @master.commands.count { |command| command.first == "SET" && command[1] == "b" }
      assert_equal 0, @replica.commands.count { |command| command.first == "SET" }
    end

    def test_two_switches_are_attributed_to_their_generation
      s1 = SentinelDouble.new(master: @master)
      s2 = SentinelDouble.new(master: @master)
      config = new_config(
        sentinels: [
          { host: "s1", port: 26379, client: s1 },
          { host: "s2", port: 26380, client: s2 },
        ],
        reconnect_attempts: 1
      )
      client = config.new_client
      client.call("PING")
      client.call("PING")

      # First switch: the old master is demoted and stops answering.
      s1.master = s2.master = @replica
      @master.role = "slave"
      @replica.role = "master"
      @master.down!
      assert_equal "PONG", client.call("PING")
      assert_equal @replica.host, config.host
      assert_equal 1, config.generation

      # Second switch: back to the original master.
      s1.master = s2.master = @master
      @replica.role = "slave"
      @master.role = "master"
      @replica.down!
      @master.up!
      assert_equal "PONG", client.call("PING")
      assert_equal @master.host, config.host
      assert_equal 2, config.generation

      events = config.connection_events
      exit_old_master = events.find { |event| event.exit? && event.address == @master.address }
      exit_replica = events.find { |event| event.exit? && event.address == @replica.address }
      establish_replica = events.find { |event| event.establish? && event.address == @replica.address }
      establish_master = events.reverse.find { |event| event.establish? && event.address == @master.address }

      refute_nil exit_old_master
      refute_nil exit_replica
      refute_nil establish_replica
      refute_nil establish_master

      # The old connection's exit belongs to the previous generation; the new
      # connection's establishment belongs to the generation it created.
      assert_equal 0, exit_old_master.generation
      assert_equal 1, exit_replica.generation
      assert_equal 1, establish_replica.generation
      assert_equal 2, establish_master.generation
    end

    def test_legacy_lazy_lookup_keeps_first_answer
      sentinel = SentinelDouble.new(master: @master)
      config = new_config(sentinels: [{ host: "s1", port: 26379, client: sentinel }])

      assert_equal @master.host, config.host
      assert_empty @master.commands
      assert_empty @replica.commands
      assert_predicate config, :resolved?
      refute_predicate config, :confirmed?
    end

    private

    def register(node)
      NodeDriver.register(node)
      node
    end

    def new_config(**kwargs)
      entries = kwargs.fetch(:sentinels)
      doubles = {}
      sentinels = entries.map do |entry|
        doubles["#{entry[:host]}:#{entry[:port]}"] = entry.fetch(:client)
        entry.slice(:host, :port)
      end

      config = RedisClient.sentinel(
        name: "cache",
        timeout: 0.05,
        driver: NodeDriver,
        **kwargs.merge(sentinels: sentinels)
      )
      config.define_singleton_method(:sentinel_client) do |sentinel_config|
        doubles.fetch("#{sentinel_config.host}:#{sentinel_config.port}")
      end
      config
    end
  end
end
