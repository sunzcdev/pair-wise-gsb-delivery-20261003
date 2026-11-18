# frozen_string_literal: true

$LOAD_PATH.unshift File.expand_path("../../lib", __dir__)

require "minitest/autorun"
require "redis-client"
require_relative "memory_driver"

class RedisClient
  class PushChannelTest < Minitest::Test
    def setup
      MemoryConnection.reset!
    end

    # Build a client wired to an externally supplied channel so tests can drive
    # it directly. Pass owned: true to have the client create and own its own
    # channel instead (that channel is closed together with the client).
    def build_client(channel: nil, owned: false, **channel_options)
      channel ||= PushChannel.new(**channel_options)
      config = Config.new(
        driver: MemoryConnection,
        # Frames are fed as in-memory Ruby objects, so the negotiated protocol
        # is irrelevant here. protocol 2 also yields an empty connection
        # prelude and keeps the scripted frames for the commands under test.
        protocol: 2,
        push_channel: (channel unless owned),
        read_timeout: 1,
        connect_timeout: 1,
        write_timeout: 1,
      )
      client = config.new_client
      [client, owned ? client.push_channel : channel]
    end

    def push(*payload)
      Push.new(payload)
    end

    def test_push_is_a_distinct_type
      frame = push("message", "chan", "hi")
      assert_kind_of Array, frame
      assert_kind_of Push, frame
      refute_kind_of Push, ["message", "chan", "hi"]
    end

    def test_push_does_not_count_as_command_reply
      client, channel = build_client
      inject(client, [push("message", "chan", "a"), "OK"])
      received = []
      channel.subscribe { |frame| received << frame }

      assert_equal "OK", client.call("SET", "k", "v")
      assert_equal [push("message", "chan", "a")], received
    ensure
      client&.close
    end

    def test_pipeline_keeps_command_order_while_pushes_are_interleaved
      client, channel = build_client
      inject(client, [
        push("pub", "1"),
        "r1",
        push("pub", "2"),
        "r2",
        push("pub", "3"),
        "r3",
      ])
      pushes = []
      channel.subscribe { |frame| pushes << frame }

      results = client.pipelined do |p|
        p.call("PING")
        p.call("PING")
        p.call("PING")
      end

      assert_equal %w[r1 r2 r3], results
      assert_equal [push("pub", "1"), push("pub", "2"), push("pub", "3")], pushes
    ensure
      client&.close
    end

    def test_pushes_delivered_in_arrival_order
      client, channel = build_client
      inject(client, [
        push("m", "1"), push("m", "2"), "OK",
      ])
      order = []
      channel.subscribe { |frame| order << frame }

      client.call("PING")
      assert_equal [push("m", "1"), push("m", "2")], order
    ensure
      client&.close
    end

    def test_buffered_without_subscriber_then_drained
      _client, channel = build_client(capacity: 5)
      channel.dispatch(push("m", "1"))
      channel.dispatch(push("m", "2"))

      assert_equal push("m", "1"), channel.pop
      assert_equal push("m", "2"), channel.pop
      assert_nil channel.pop(0.01)
    end

    def test_buffer_rejects_when_full_by_default
      _client, channel = build_client(capacity: 2)
      channel.dispatch(push("m", "1"))
      channel.dispatch(push("m", "2"))

      error = assert_raises(PushChannel::BufferOverflowError) do
        channel.dispatch(push("m", "3"))
      end
      assert_kind_of PushChannel::Error, error
      assert_equal [push("m", "1"), push("m", "2")], drain(channel)
    end

    def test_buffer_drop_oldest_policy
      _client, channel = build_client(capacity: 2, overflow_policy: :drop_oldest)
      result1 = channel.dispatch(push("m", "1"))
      channel.dispatch(push("m", "2"))
      result3 = channel.dispatch(push("m", "3"))

      assert result1
      assert result3
      assert_equal [push("m", "2"), push("m", "3")], drain(channel)
    end

    def test_zero_capacity_drops_without_subscriber
      _client, channel = build_client(capacity: 0, overflow_policy: :drop_oldest)
      assert_equal false, channel.dispatch(push("m", "1"))
      assert_nil channel.pop(0.01)
    end

    def test_zero_capacity_rejects_without_subscriber
      _client, channel = build_client(capacity: 0)
      assert_raises(PushChannel::BufferOverflowError) do
        channel.dispatch(push("m", "1"))
      end
    end

    def test_close_wakes_waiting_pop_and_raises_after_drain
      _client, channel = build_client
      channel.dispatch(push("m", "1"))

      assert_equal push("m", "1"), channel.pop # drain the buffered frame

      # With an empty buffer, pop blocks until the channel is closed.
      closed_error = blocking_pop_error(channel) do
        channel.close
      end

      assert_kind_of PushChannel::ChannelClosedError, closed_error
      assert_raises(PushChannel::ChannelClosedError) { channel.pop }
      assert channel.closed?
    end

    def test_pop_timeout_returns_nil
      _client, channel = build_client
      assert_nil channel.pop(0.01)
    end

    def test_subscribe_unsubscribe
      _client, channel = build_client
      block = ->(frame) { @seen << frame }
      @seen = []
      channel.subscribe(&block)
      channel.dispatch(push("m", "1"))
      channel.unsubscribe(block)
      channel.dispatch(push("m", "2")) # buffered, not delivered to the callback

      assert_equal [push("m", "1")], @seen
      assert_equal [push("m", "2")], drain(channel)
    end

    def test_isolated_callback_error_keeps_connection_working
      seen = []
      errors = []
      _client, channel = build_client(
        error_policy: :isolate,
        on_error: ->(error) { errors << error },
      )
      channel.subscribe { |frame| raise "boom: #{frame.last}" }
      channel.subscribe { |frame| seen << frame }

      channel.dispatch(push("m", "1"))

      assert_equal [push("m", "1")], seen
      assert_equal 1, errors.size
      assert_equal "boom: 1", errors.first.message
    end

    def test_raise_callback_error_terminates
      _client, channel = build_client(error_policy: :raise)
      channel.subscribe { |frame| raise "boom: #{frame.last}" }

      assert_raises(PushChannel::CallbackError) do
        channel.dispatch(push("m", "1"))
      end
    end

    def test_callback_runs_outside_read_lock_and_can_use_another_connection
      client1, channel = build_client
      client2, _channel2 = build_client(channel: channel)
      inject(client1, [push("m", "1"), "ok-1"])
      inject(client2, ["ok-2"])

      inner = nil
      channel.subscribe do |frame|
        # Issuing a command on a *different* connection must not deadlock.
        inner = client2.call("PING") if frame.last == "1"
      end

      assert_equal "ok-1", client1.call("PING")
      assert_equal "ok-2", inner
    ensure
      client1&.close
      client2&.close
    end

    def test_reentrant_read_on_same_connection_raises_instead_of_deadlocking
      # A push callback re-entering the same connection must fail fast with a
      # ReentrantDispatchError instead of blocking forever on the read lock.
      # The re-entrant call is rejected at the call entry, before any frame is
      # consumed, so the normal reply is still returned afterwards.
      client, channel = build_client(error_policy: :isolate)
      inject(client, [push("m", "1"), "ok-outer"])
      nested_error = nil
      channel.subscribe do |frame|
        begin
          client.call("PING") if frame.last == "1"
        rescue PushChannel::ReentrantDispatchError => e
          nested_error = e
        end
      end

      assert_equal "ok-outer", client.call("PING")
      assert_kind_of PushChannel::ReentrantDispatchError, nested_error
      assert_equal 0, pending_reads(client)
    ensure
      client&.close
    end

    def test_reentrant_dispatch_under_raise_policy_terminates_the_command
      # With :raise the re-entrant error is not contained: it surfaces as a
      # CallbackError whose cause is the original ReentrantDispatchError.
      client, channel = build_client(error_policy: :raise)
      inject(client, [push("m", "1"), "ok-outer"])
      channel.subscribe do |frame|
        client.call("PING") if frame.last == "1"
      end

      error = assert_raises(PushChannel::CallbackError) { client.call("PING") }
      assert_kind_of PushChannel::ReentrantDispatchError, error
    ensure
      client&.close
    end

    def test_reentrant_error_isolated_keeps_connection_usable
      # With :isolate the rejected re-entrant call is contained, the outer
      # command completes and the connection stays usable for later commands.
      client, channel = build_client(error_policy: :isolate)
      inject(client, [push("m", "1"), "ok-outer", "second"])
      nested_error = nil
      channel.subscribe do |frame|
        begin
          client.call("PING") if frame.last == "1"
        rescue PushChannel::ReentrantDispatchError => e
          nested_error = e
        end
      end

      assert_equal "ok-outer", client.call("PING")
      assert_kind_of PushChannel::ReentrantDispatchError, nested_error
      assert_equal 0, pending_reads(client)
      assert_equal "second", client.call("PING")
    ensure
      client&.close
    end

    def test_normal_reply_is_not_lost_when_callback_raises_with_raise_policy
      client, channel = build_client(error_policy: :raise)
      inject(client, [push("m", "1"), "the-reply"])
      channel.subscribe { |frame| raise "boom" if frame.last == "1" }

      assert_raises(PushChannel::CallbackError) { client.call("PING") }
    ensure
      client&.close
    end

    def test_invalid_constructor_arguments
      assert_raises(ArgumentError) { PushChannel.new(capacity: -1) }
      assert_raises(ArgumentError) { PushChannel.new(overflow_policy: :nope) }
      assert_raises(ArgumentError) { PushChannel.new(error_policy: :nope) }
      assert_raises(ArgumentError) { PushChannel.new(on_error: Object.new) }
    end

    def test_config_validates_push_channel_interface
      assert_raises(ArgumentError) do
        Config.new(driver: MemoryConnection, push_channel: Object.new)
      end
    end

    def test_default_channel_is_created_per_client
      config = Config.new(driver: MemoryConnection)
      client = config.new_client
      assert_kind_of PushChannel, client.push_channel
    ensure
      client&.close
    end

    def test_close_closes_the_owned_channel_and_wakes_waiters
      client, channel = build_client(owned: true)

      closed_error = blocking_pop_error(channel) { client.close }

      assert_kind_of PushChannel::ChannelClosedError, closed_error
      assert channel.closed?
    end

    def test_close_keeps_an_externally_supplied_channel_open
      shared = PushChannel.new
      config = Config.new(driver: MemoryConnection, protocol: 2, push_channel: shared)
      client = config.new_client

      client.close

      refute shared.closed?
      assert_nil shared.pop(0.01)
    ensure
      shared&.close
    end

    private

    # Run a blocking #pop on the channel in a thread, and only invoke the block
    # once that thread has actually registered as a waiter. This makes the
    # wake-on-close assertion deterministic instead of relying on sleep timing.
    def blocking_pop_error(channel)
      error = nil
      thread = Thread.new do
        begin
          channel.pop
        rescue PushChannel::ChannelClosedError => e
          error = e
        end
      end

      deadline = RedisClient.now + 5
      sleep 0.01 until channel.waiting? || RedisClient.now > deadline
      raise "pop thread never became a waiter" unless channel.waiting?

      yield
      thread.join(5)
      error
    end

    # Feed frames to the connection a client is currently using. The memory
    # driver connects eagerly in RedisClient#new, so the class-level script
    # queue would be consumed by that initial connection; injecting onto the
    # live connection avoids any reconnect race.
    def inject(client, frames)
      client.send(:raw_connection).script = frames
    end

    def pending_reads(client)
      client.send(:raw_connection).instance_variable_get(:@pending_reads)
    end

    def drain(channel)
      frames = []
      while (frame = channel.pop(0.01))
        frames << frame
      end
      frames
    end
  end
end
