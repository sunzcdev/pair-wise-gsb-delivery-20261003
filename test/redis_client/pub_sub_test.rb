# frozen_string_literal: true

# These tests exercise the Pub/Sub subscription bookkeeping with a scripted,
# in-memory connection. They intentionally neither start nor talk to a real
# Redis server: the behaviour under test is the client side state machine
# (desired vs confirmed subscriptions, acknowledgements, reconnection gaps).
require "redis_client"

# Prefer the megatest base class when the suite is running, so these tests are
# picked up; fall back to plain Minitest otherwise (e.g. running this file
# directly). Either way the tests never require `test_helper`, hence never
# boot a Redis server.
module PubSubTestBase
  def self.resolve
    if defined?(Megatest::Test)
      begin
        Megatest.registry
        return Megatest::Test
      rescue StandardError
        # Megatest is loaded but no suite is being built.
      end
    end
    require "minitest/autorun"
    Minitest::Test
  end
end

class RedisClient
  class PubSubStateTest < PubSubTestBase.resolve
    # Minimal stand-in for a raw connection. Writes are recorded; reads pop
    # scripted events. `fail_reads!` makes the next read raise a connection
    # error, simulating a lost socket.
    class FakeConnection
      attr_reader :written, :generation
      attr_accessor :closed

      def initialize(*events)
        @events = events
        @written = []
        @closed = false
        @fail_reads = false
      end

      def write(command)
        raise RedisClient::ConnectionError, "closed" if @closed

        @written << command
        nil
      end

      def read(_timeout = nil)
        if @fail_reads
          @fail_reads = false
          raise RedisClient::ConnectionError, "simulated lost connection"
        end

        event = @events.shift
        raise RedisClient::ReadTimeoutError, "timeout" if event == :timeout

        return event unless event.nil?

        raise RedisClient::ReadTimeoutError, "empty"
      end

      def fail_reads!
        @fail_reads = true
      end

      def push(*events)
        @events.concat(events)
      end

      def close
        @closed = true
        nil
      end

      def connected?
        !@closed
      end
    end

    def setup
      @connection = FakeConnection.new
      @builder = RedisClient::CommandBuilder
      @pubsub = RedisClient::PubSub.new(@connection, @builder)
    end

    def test_written_subscribe_is_not_confirmed
      @pubsub.call("SUBSCRIBE", "news")

      assert_equal [["SUBSCRIBE", "news"]], @connection.written
      assert_equal ["news"], @pubsub.desired_channels
      assert_equal [], @pubsub.confirmed_channels
      assert_equal ["news"], @pubsub.pending_channels
      assert_equal 0, @pubsub.subscription_count
      refute @pubsub.confirmed?
      refute @pubsub.subscribed?("news")
    end

    def test_acknowledgement_confirms_subscription
      @pubsub.subscribe("news")
      @pubsub.handle_event(["subscribe", "news", 1])

      assert_equal ["news"], @pubsub.confirmed_channels
      assert_equal 1, @pubsub.subscription_count
      assert @pubsub.subscribed?("news")
      assert @pubsub.confirmed?
      assert_empty @pubsub.pending_channels
    end

    def test_duplicate_acknowledgement_does_not_change_count
      @pubsub.subscribe("news")
      @pubsub.handle_event(["subscribe", "news", 1])
      @pubsub.handle_event(["subscribe", "news", 1])
      @pubsub.handle_event(["subscribe", "news", 1])

      assert_equal 1, @pubsub.subscription_count
      assert_equal ["news"], @pubsub.confirmed_channels
      assert @pubsub.settled?
    end

    def test_multiple_channels_share_one_count
      @pubsub.subscribe("a", "b")
      assert_equal [["SUBSCRIBE", "a", "b"]], @connection.written

      @pubsub.handle_event(["subscribe", "a", 1])
      assert_equal 1, @pubsub.subscription_count

      @pubsub.handle_event(["subscribe", "b", 2])
      assert_equal 2, @pubsub.subscription_count
      assert_equal %w[a b], @pubsub.confirmed_channels.sort
    end

    def test_patterns_are_tracked_separately_from_channels
      @pubsub.psubscribe("news.*")
      assert_equal [["PSUBSCRIBE", "news.*"]], @connection.written
      assert_equal [], @pubsub.desired_channels
      assert_equal ["news.*"], @pubsub.desired_patterns

      @pubsub.handle_event(["psubscribe", "news.*", 1])
      assert_equal ["news.*"], @pubsub.confirmed_patterns
      assert_equal 1, @pubsub.subscription_count
    end

    def test_continuous_subscribe_and_unsubscribe_before_acknowledgement
      @pubsub.subscribe("a")
      @pubsub.unsubscribe("a")

      # The removal is desired immediately, but nothing is confirmed yet.
      assert_empty @pubsub.desired_channels
      assert_empty @pubsub.confirmed_channels
      assert_equal 0, @pubsub.subscription_count

      # A late `subscribe` acknowledgement confirms `a` (the server really is
      # subscribed), which is then reported as stale because the caller already
      # asked to remove it.
      @pubsub.handle_event(["subscribe", "a", 1])
      assert_equal ["a"], @pubsub.confirmed_channels
      assert_equal ["a"], @pubsub.state.stale_channels
      refute @pubsub.settled?

      @pubsub.handle_event(["unsubscribe", "a", 0])
      assert_empty @pubsub.confirmed_channels
      assert_equal 0, @pubsub.subscription_count
      assert @pubsub.settled?
    end

    def test_subscribe_unsubscribe_subscribe_again
      @pubsub.subscribe("a")
      @pubsub.unsubscribe("a")
      @pubsub.subscribe("a")

      assert_equal ["a"], @pubsub.desired_channels
      @pubsub.handle_event(["subscribe", "a", 1])
      assert_equal ["a"], @pubsub.confirmed_channels
      assert @pubsub.settled?
    end

    def test_bare_unsubscribe_clears_desired_set_only
      @pubsub.subscribe("a", "b")
      @pubsub.handle_event(["subscribe", "a", 1])
      @pubsub.handle_event(["subscribe", "b", 2])

      @pubsub.call("UNSUBSCRIBE")
      assert_equal [["SUBSCRIBE", "a", "b"], ["UNSUBSCRIBE"]], @connection.written
      assert_empty @pubsub.desired_channels
      assert_equal %w[a b], @pubsub.confirmed_channels.sort
      assert_equal 2, @pubsub.subscription_count

      @pubsub.handle_event(["unsubscribe", "a", 1])
      @pubsub.handle_event(["unsubscribe", "b", 0])
      assert_empty @pubsub.confirmed_channels
      assert @pubsub.settled?
    end

    def test_message_events_are_returned_unchanged
      @pubsub.subscribe("news")
      @connection.push(["subscribe", "news", 1], ["message", "news", "hello"])

      assert_equal ["subscribe", "news", 1], @pubsub.next_event(0.1)
      assert_equal ["message", "news", "hello"], @pubsub.next_event(0.1)
      assert_equal 1, @pubsub.subscription_count
    end

    def test_next_event_returns_nil_on_timeout
      @connection.push(:timeout)
      assert_nil @pubsub.next_event(0.01)
    end

    def test_next_event_marks_connection_lost_and_opens_gap
      @pubsub.subscribe("news")
      @pubsub.handle_event(["subscribe", "news", 1])
      @connection.fail_reads!

      assert_raises(RedisClient::ConnectionError) { @pubsub.next_event(0.1) }

      refute @pubsub.connected?
      assert_empty @pubsub.confirmed_channels
      # The desired set survives; only the confirmed set died with the socket.
      assert_equal ["news"], @pubsub.desired_channels
      assert @pubsub.state.gap?
      assert @pubsub.state.gap.may_have_missed_messages?
      refute @pubsub.state.gap.replayable?
    end

    def test_reconnect_preserves_desired_set_and_discards_confirmed_set
      @pubsub.subscribe("a", "b")
      @pubsub.psubscribe("p.*")
      @pubsub.handle_event(["subscribe", "a", 1])
      @pubsub.handle_event(["subscribe", "b", 2])
      @pubsub.handle_event(["psubscribe", "p.*", 3])
      assert_equal 3, @pubsub.subscription_count

      second = FakeConnection.new
      @pubsub.reconnect(second)

      assert_equal 2, @pubsub.generation
      assert_empty @pubsub.confirmed_channels
      assert_empty @pubsub.confirmed_patterns
      assert_equal 0, @pubsub.subscription_count
      assert_equal %w[a b], @pubsub.desired_channels.sort
      assert_equal ["p.*"], @pubsub.desired_patterns

      assert_equal [["SUBSCRIBE", "a", "b"], ["PSUBSCRIBE", "p.*"]], second.written
    end

    def test_reconnect_gap_is_observable_and_closes_after_acknowledgements
      @pubsub.subscribe("a")
      @pubsub.handle_event(["subscribe", "a", 1])

      second = FakeConnection.new
      @pubsub.reconnect(second)

      gap = @pubsub.current_gap
      refute_nil gap
      assert gap.open?
      assert gap.reconnected?
      assert_equal 1, gap.generation
      assert_equal 2, gap.next_generation
      assert_equal ["a"], gap.resubscribed_channels
      assert gap.may_have_missed_messages?
      refute gap.replayable?
      refute gap.replayed?

      # The gap remains open while the new connection has not confirmed yet.
      assert @pubsub.state.gap?

      @pubsub.handle_event(["subscribe", "a", 1])
      assert_nil @pubsub.current_gap
      refute @pubsub.state.gap?

      gaps = @pubsub.gaps
      assert_equal 1, gaps.size
      assert gaps.first.closed?
      refute gaps.first.replayed?
    end

    def test_late_events_from_previous_generation_are_dropped
      @pubsub.subscribe("old")
      @pubsub.handle_event(["subscribe", "old", 1])

      @pubsub.reconnect(FakeConnection.new)
      @pubsub.subscribe("new")
      assert_equal 2, @pubsub.generation
      assert_equal [], @pubsub.confirmed_channels

      # An adapter still holding the old socket feeds a late acknowledgement
      # and a late message. Neither may touch the new connection state.
      accepted = @pubsub.handle_event(["subscribe", "old", 1], generation: 1)
      refute accepted
      assert_empty @pubsub.confirmed_channels

      accepted = @pubsub.handle_event(["message", "old", "stale"], generation: 1)
      refute accepted
      assert_equal 2, @pubsub.dropped_events

      @pubsub.handle_event(["subscribe", "new", 1], generation: 2)
      assert_equal ["new"], @pubsub.confirmed_channels
      assert_equal 1, @pubsub.subscription_count
    end

    def test_generation_tracks_connections
      assert_equal 1, @pubsub.generation
      @pubsub.reconnect(FakeConnection.new)
      assert_equal 2, @pubsub.generation
      @pubsub.reconnect(FakeConnection.new)
      assert_equal 3, @pubsub.generation
      assert_equal 2, @pubsub.state.reconnects
    end

    def test_reconnect_without_factory_raises
      assert_raises(RedisClient::ConnectionError) { @pubsub.reconnect }
      # The failure is recorded, and the connection is not silently reused.
      refute @pubsub.connected?
      assert_equal 1, @pubsub.state.reconnect_attempts
    end

    def test_close_only_unsubscribes_owned_subscriptions
      @pubsub.subscribe("owned-a", "owned-b")
      @pubsub.handle_event(["subscribe", "owned-a", 1])
      @pubsub.handle_event(["subscribe", "owned-b", 2])

      assert_equal @pubsub, @pubsub.close
      assert @pubsub.closed?
      refute @pubsub.connected?

      assert_equal [
        ["SUBSCRIBE", "owned-a", "owned-b"],
        ["UNSUBSCRIBE", "owned-a", "owned-b"],
      ], @connection.written
      assert @connection.closed

      assert_empty @pubsub.desired_channels
      assert_empty @pubsub.confirmed_channels
      assert_raises(RedisClient::ConnectionError) { @pubsub.next_event }
      assert_raises(RedisClient::ConnectionError) { @pubsub.call("SUBSCRIBE", "x") }
    end

    def test_close_after_lost_connection_is_safe
      @pubsub.subscribe("a")
      @pubsub.handle_event(["subscribe", "a", 1])
      @connection.fail_reads!
      assert_raises(RedisClient::ConnectionError) { @pubsub.next_event(0.1) }
      assert @pubsub.gap?

      assert_equal @pubsub, @pubsub.close
      assert @pubsub.closed?
      refute @pubsub.gap?
      assert @pubsub.gaps.last.closed?
    end

    def test_close_never_issues_bare_unsubscribe
      @pubsub.subscribe("a")
      @pubsub.handle_event(["subscribe", "a", 1])
      @pubsub.close

      refute_includes @connection.written, ["UNSUBSCRIBE"]
      refute_includes @connection.written, ["PUNSUBSCRIBE"]
    end

    def test_closing_one_client_leaves_the_other_alone
      other_connection = FakeConnection.new
      other = RedisClient::PubSub.new(other_connection, @builder)

      @pubsub.subscribe("mine")
      @pubsub.handle_event(["subscribe", "mine", 1])
      other.subscribe("theirs")
      other.handle_event(["subscribe", "theirs", 1])

      @pubsub.close

      assert_equal [["SUBSCRIBE", "mine"], ["UNSUBSCRIBE", "mine"]], @connection.written
      assert_equal [["SUBSCRIBE", "theirs"]], other_connection.written
      refute other.closed?
      assert_equal ["theirs"], other.confirmed_channels
      assert_equal 1, other.subscription_count
    end

    def test_state_snapshot_is_detached
      @pubsub.subscribe("a")
      snapshot = @pubsub.state

      @pubsub.handle_event(["subscribe", "a", 1])

      assert_equal [], snapshot.confirmed_channels
      refute snapshot.settled?
      assert_equal ["a"], @pubsub.state.confirmed_channels
      assert @pubsub.state.settled?
    end

    def test_state_reports_pending_and_stale
      @pubsub.subscribe("a", "b")
      @pubsub.handle_event(["subscribe", "a", 1])
      @pubsub.unsubscribe("b")

      state = @pubsub.state
      assert_equal ["a"], state.confirmed_channels
      assert_empty state.pending_channels
      assert_empty state.stale_channels # b was never confirmed
      assert_equal 1, state.confirmed_count
      assert_equal 1, state.desired_count

      @pubsub.handle_event(["subscribe", "b", 2])
      state = @pubsub.state
      assert_equal %w[a b], state.confirmed_channels.sort
      assert_equal ["b"], state.stale_channels
      refute state.settled?
    end

    def test_writes_after_connection_loss_raise
      @connection.fail_reads!
      assert_raises(RedisClient::ConnectionError) { @pubsub.next_event(0.1) }

      assert_raises(RedisClient::ConnectionError) { @pubsub.subscribe("a") }
      assert_empty @pubsub.confirmed_channels
    end

    def test_resubscribe_keeps_generation
      @pubsub.subscribe("a")
      @pubsub.handle_event(["subscribe", "a", 1])
      generation = @pubsub.generation

      @pubsub.resubscribe

      assert_equal generation, @pubsub.generation
      assert_equal [["SUBSCRIBE", "a"], ["SUBSCRIBE", "a"]], @connection.written
    end

    def test_to_h_exposes_the_same_facts
      @pubsub.subscribe("a")
      @pubsub.handle_event(["subscribe", "a", 1])
      h = @pubsub.state.to_h

      assert_equal 1, h[:generation]
      assert_equal({ channels: ["a"], patterns: [] }, h[:desired])
      assert_equal({ channels: ["a"], patterns: [] }, h[:confirmed])
      assert_equal 1, h[:confirmed_count]
      assert_equal true, h[:settled]
      assert_nil h[:gap]
    end

    def test_counts_track_desired_pending_and_confirmed
      @pubsub.subscribe("a", "b")
      @pubsub.psubscribe("p.*")

      assert_equal 3, @pubsub.desired_count
      assert_equal 3, @pubsub.pending_count
      assert_equal 0, @pubsub.subscription_count

      @pubsub.handle_event(["subscribe", "a", 1])
      assert_equal 3, @pubsub.desired_count
      assert_equal 2, @pubsub.pending_count
      assert_equal 1, @pubsub.subscription_count

      @pubsub.handle_event(["subscribe", "b", 2])
      @pubsub.handle_event(["psubscribe", "p.*", 3])
      assert_equal 0, @pubsub.pending_count
      assert_equal 3, @pubsub.subscription_count
      assert @pubsub.settled?
    end

    def test_unsubscribe_acknowledgement_decrements_count
      @pubsub.subscribe("a", "b")
      @pubsub.handle_event(["subscribe", "a", 1])
      @pubsub.handle_event(["subscribe", "b", 2])
      assert_equal 2, @pubsub.subscription_count

      @pubsub.unsubscribe("a")
      # Still confirmed until the server answers.
      assert_equal 2, @pubsub.subscription_count
      assert_equal ["a"], @pubsub.state.stale_channels

      @pubsub.handle_event(["unsubscribe", "a", 1])
      assert_equal 1, @pubsub.subscription_count
      assert_equal ["b"], @pubsub.confirmed_channels
      assert @pubsub.settled?
    end

    def test_confirmed_is_about_pending_while_settled_also_covers_stale
      @pubsub.subscribe("a")
      @pubsub.handle_event(["subscribe", "a", 1])
      @pubsub.unsubscribe("a")

      # Nothing desired is pending, but the removal acknowledgement is still
      # outstanding, so the connection is not settled.
      assert @pubsub.confirmed?
      refute @pubsub.settled?
      refute @pubsub.status.settled?
      assert @pubsub.status.confirmed?
    end

    def test_disconnect_open_gap_is_reported_in_state
      @pubsub.subscribe("a")
      @pubsub.handle_event(["subscribe", "a", 1])
      @connection.fail_reads!
      assert_raises(RedisClient::ConnectionError) { @pubsub.next_event(0.1) }

      assert @pubsub.gap?
      assert @pubsub.disconnected?
      state = @pubsub.state
      assert state.gap?
      assert state.disconnected?
      refute state.connected?
      assert_equal 1, state.disconnects
      assert_equal 1, state.generation
      assert state.gap.may_have_missed_messages?
      # The desired set survived the disconnect.
      assert_equal ["a"], state.desired_channels
      assert_empty state.confirmed_channels
    end

    def test_event_read_from_a_replaced_connection_is_dropped_and_skipped
      replacement = FakeConnection.new(["subscribe", "a", 1])
      subject = nil

      # Simulates an adapter that swaps the connection while a read is in
      # flight: the old socket then returns a late `subscribe` ack.
      stale_source = Class.new(FakeConnection) do
        define_method(:read) do |*|
          subject.reconnect(replacement)
          ["subscribe", "a", 1]
        end
      end.new

      subject = RedisClient::PubSub.new(stale_source, @builder)
      subject.subscribe("a")
      subject.handle_event(["subscribe", "a", 1])
      assert_equal 1, subject.generation

      # The late event from generation 1 is dropped, and the loop moves on to
      # read the replacement (generation 2) connection instead.
      assert_equal ["subscribe", "a", 1], subject.next_event(0.1)
      assert_equal 2, subject.generation
      assert_equal 1, subject.dropped_events
      assert_equal ["a"], subject.confirmed_channels
      assert subject.settled?
    end

    def test_failed_reconnect_keeps_gap_open_and_desired_set
      @pubsub.subscribe("a")
      @pubsub.handle_event(["subscribe", "a", 1])
      failing_factory = -> { raise RedisClient::ConnectionError, "cannot connect" }
      pubsub = RedisClient::PubSub.new(FakeConnection.new, @builder, reconnect: failing_factory)
      pubsub.subscribe("a")
      pubsub.handle_event(["subscribe", "a", 1])

      assert_raises(RedisClient::ConnectionError) { pubsub.reconnect }

      refute pubsub.connected?
      assert pubsub.gap?
      assert_equal ["a"], pubsub.desired_channels
      assert_empty pubsub.confirmed_channels
      assert_equal 1, pubsub.state.reconnect_attempts
      assert_equal 0, pubsub.state.reconnects
    end
  end

  # Integration of RedisClient#pubsub with a scripted driver. Still no real
  # Redis: `driver:` points at an in-memory class.
  class PubSubClientTest < PubSubTestBase.resolve
    class FakeDriver
      class << self
        attr_accessor :inbox, :created
      end
      self.inbox = []
      self.created = []

      attr_reader :written, :events
      attr_writer :read_timeout, :write_timeout

      def initialize(_config, connect_timeout:, read_timeout:, write_timeout:)
        @written = []
        @events = self.class.inbox.shift || []
        @closed = false
        @connect_timeout = connect_timeout
        @read_timeout = read_timeout
        @write_timeout = write_timeout
        self.class.created << self
      end

      def write(command)
        raise RedisClient::ConnectionError, "closed" if @closed

        @written << command
        nil
      end

      def write_multi(commands)
        commands.each { |command| write(command) }
        nil
      end

      # Replies for the connection prelude (HELLO/AUTH/SELECT/SETNAME).
      def call_pipelined(commands, _timeouts, exception: true) # rubocop:disable Lint/UnusedMethodArgument
        commands.map { "OK" }
      end

      # Regular (non Pub/Sub) calls, used to prove the client keeps its own
      # connection separate from the Pub/Sub one.
      def call(command, _timeout)
        return "PONG" if command.first == "PING"

        "OK"
      end

      def read(_timeout = nil)
        return @events.shift unless @events.empty?

        raise RedisClient::ReadTimeoutError, "no event"
      end

      def close
        @closed = true
        nil
      end

      def revalidate # rubocop:disable Naming/PredicateMethod
        !@closed
      end

      def retry_attempt=(_value)
      end

      def measure_round_trip_delay
        0
      end
    end

    def setup
      FakeDriver.inbox = []
      FakeDriver.created = []
      @config = RedisClient.config(driver: FakeDriver)
    end

    def test_client_pubsub_tracks_desired_and_confirmed
      # The Pub/Sub connection is the first one created.
      FakeDriver.inbox = [[["subscribe", "news", 1]]]

      pubsub = @config.new_client.pubsub
      pubsub.subscribe("news")
      assert_equal 1, FakeDriver.created.size
      assert_equal [["SUBSCRIBE", "news"]], FakeDriver.created.first.written
      assert_empty pubsub.confirmed_channels

      assert_equal ["subscribe", "news", 1], pubsub.next_event(0.01)
      assert_equal ["news"], pubsub.confirmed_channels
      assert_equal 1, pubsub.subscription_count
    end

    def test_client_pubsub_reconnect_uses_configured_factory
      FakeDriver.inbox = [[], [["subscribe", "news", 1]]]

      pubsub = @config.new_client.pubsub(reconnect: true)
      pubsub.subscribe("news")
      pubsub.handle_event(["subscribe", "news", 1])
      assert_equal 1, pubsub.generation

      pubsub.reconnect
      assert_equal 2, pubsub.generation
      assert_equal 2, FakeDriver.created.size
      assert_equal [["SUBSCRIBE", "news"]], FakeDriver.created.last.written
      assert_empty pubsub.confirmed_channels
      assert pubsub.state.gap?
      # The gap only closes once the new connection acknowledges.
      pubsub.handle_event(["subscribe", "news", 1], generation: 2)
      refute pubsub.state.gap?
      assert pubsub.settled?
    end

    def test_client_pubsub_reconnect_requires_opt_in
      FakeDriver.inbox = [[]]
      pubsub = @config.new_client.pubsub

      error = assert_raises(RedisClient::ConnectionError) { pubsub.reconnect }
      assert_match(/no reconnect handler/i, error.message)
      refute pubsub.connected?
    end

    def test_client_pubsub_close_is_scoped_to_the_instance
      FakeDriver.inbox = [[], []]

      first = @config.new_client.pubsub
      first.psubscribe("p.*")
      first.handle_event(["psubscribe", "p.*", 1])

      second = @config.new_client.pubsub
      second.subscribe("other")
      second.handle_event(["subscribe", "other", 1])

      first.close

      assert_equal [["PSUBSCRIBE", "p.*"], ["PUNSUBSCRIBE", "p.*"]], FakeDriver.created[0].written
      assert_equal [["SUBSCRIBE", "other"]], FakeDriver.created[1].written
      refute second.closed?
      assert_equal ["other"], second.confirmed_channels
    end

    def test_client_pubsub_does_not_touch_client_raw_connection
      FakeDriver.inbox = [[]]
      client = @config.new_client
      pubsub = client.pubsub

      assert_equal "PONG", client.call("PING")
      # The client opened its own connection for the regular calls, separate
      # from the Pub/Sub one.
      assert_equal 2, FakeDriver.created.size
      refute pubsub.closed?
      assert pubsub.connected?
    end
  end
end
