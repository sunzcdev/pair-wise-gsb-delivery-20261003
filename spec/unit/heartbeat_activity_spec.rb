# frozen_string_literal: true

require "spec_helper"

describe "Heartbeat I/O observability" do
  # Deterministic monotonic clock shared by all fakes in an example.
  class FakeClock
    def initialize(start = 1000.0)
      @now = start
    end

    def advance(seconds)
      @now += seconds
    end

    def call
      @now
    end
  end

  # Local transport stand-in. Writes go through a swappable callback, so a
  # test can block the very first publish write and later "release" it.
  # Only writes the callback actually completes are handed to the per-
  # connection NetworkActivityMonitor.
  class FakeTransport
    attr_reader :network_activity, :written, :connection_id

    def initialize(clock:, connection_id:, initial_time: nil)
      @clock         = clock
      @connection_id = connection_id
      @network_activity = Bunny::NetworkActivityMonitor.new(
        connection_id: connection_id,
        clock:         clock,
        initial_time:  initial_time
      )
      @written       = []
      @write_callback = method(:default_write)
    end

    # Replace the code that performs a write. The new callback receives
    # +self+ and the frame bytes; it decides whether (and when) the write
    # completes.
    def on_write(&block)
      @write_callback = block
    end

    def restore_write_callback
      @write_callback = method(:default_write)
    end

    # Mirrors Bunny::Transport#write_without_timeout: the monitor is touched
    # only when the write callback completes without blocking.
    def write_without_timeout(data, _raise_exceptions = false)
      @write_callback.call(self, data)
    end
    alias write write_without_timeout

    # Simulates the reader loop receiving one complete, legal frame.
    def receive(frame)
      @network_activity.record_read!(frame)
    end

    def complete_write(data)
      @written << data
      @network_activity.record_write!(data)
    end

    private

    def default_write(transport, data)
      transport.complete_write(data)
    end
  end

  # A legal AMQP method frame, as assembled by Transport#read_next_frame.
  def legal_frame
    AMQ::Protocol::Frame.new(1, "method-payload", 0)
  end

  # Encoded bytes of an ordinary outgoing (publish-like) method frame.
  def publish_frame
    AMQ::Protocol::Channel::Open.encode(1, AMQ::Protocol::EMPTY_STRING).encode
  end

  let(:clock)             { FakeClock.new }
  let(:blocked_publishes) { [] }

  def new_transport(connection_id)
    FakeTransport.new(clock: clock, connection_id: connection_id, initial_time: 1000.0)
  end

  # Installs a write callback that blocks every write until the gate is
  # released, i.e. the local transport cannot confirm the first publish.
  def block_writes(transport, queue)
    transport.on_write do |_t, data|
      queue << data
      # the frame is only queued here; it never reaches the monitor
    end
  end

  describe "write-side timer reflects completed writes only" do
    it "keeps queued publishes off the timer until the transport write returns" do
      transport = new_transport(1)
      block_writes(transport, blocked_publishes)

      # a publishing thread enqueues frames while the write is stuck
      transport.write(publish_frame)
      transport.write(publish_frame)
      transport.write(publish_frame)

      expect(blocked_publishes.size).to eq(3)
      expect(transport.network_activity.frames_written).to eq(0)
      expect(transport.network_activity.last_written_frame).to be_nil
      expect(transport.written).to be_empty
    end

    it "stamps the write side at completion time once the write goes through" do
      transport = new_transport(1)

      clock.advance(2)
      transport.write(publish_frame)

      expect(transport.network_activity.frames_written).to eq(1)
      expect(transport.network_activity.last_written_frame).to eq(publish_frame)
      expect(transport.network_activity.last_write_at).to eq(1002.0)
    end
  end

  describe "read-side timer reflects frames actually received" do
    it "records a legal incoming frame without touching the write side" do
      transport = new_transport(1)
      frame     = legal_frame

      clock.advance(6)
      transport.receive(frame)

      monitor = transport.network_activity
      expect(monitor.frames_read).to eq(1)
      expect(monitor.last_read_frame).to equal(frame)
      expect(monitor.last_read_at).to eq(1006.0)

      # read progress must not be mistaken for write progress
      expect(monitor.frames_written).to eq(0)
      expect(monitor.last_write_at).to eq(1000.0)
    end
  end

  describe "the blocked-write / live-read / reconnect scenario" do
    it "shows read progress and an unconfirmed write side, and isolates the next connection" do
      #
      # Connection 1: the first publish write is stuck in the local transport.
      #
      old_transport = new_transport(1)
      block_writes(old_transport, blocked_publishes)

      old_transport.write(publish_frame)
      old_transport.write(publish_frame)

      old_monitor = old_transport.network_activity

      # Meanwhile the reader direction is healthy: legal frames keep arriving.
      clock.advance(5)
      old_transport.receive(legal_frame)
      clock.advance(5)
      old_transport.receive(legal_frame)

      # Report on connection 1: reads are progressing, writes are not confirmed.
      expect(old_monitor.frames_read).to eq(2)
      expect(old_monitor.last_read_at).to eq(1010.0)
      expect(old_monitor.frames_written).to eq(0)
      expect(old_monitor.last_written_frame).to be_nil
      expect(old_monitor.last_write_at).to eq(1000.0)
      expect(old_monitor.write_idle_for?(10)).to be(true)
      expect(old_monitor.read_idle_for?(10)).to be(false)
      expect(blocked_publishes.size).to eq(2)

      #
      # The connection is replaced (as in recovery). The new connection gets a
      # fresh monitor bound to its own transport.
      #
      new_transport = new_transport(2)
      new_monitor   = new_transport.network_activity

      expect(new_monitor.connection_id).to eq(2)
      expect(new_monitor.frames_written).to eq(0)
      expect(new_monitor.frames_read).to eq(0)

      # The old write callback is eventually restored and its queued writes
      # complete -- on the *old* transport/monitor only.
      old_transport.restore_write_callback
      blocked_publishes.each { |frame| old_transport.write(frame) }

      expect(old_monitor.frames_written).to eq(2)
      expect(old_monitor.last_written_frame).to eq(publish_frame)
      expect(old_monitor.last_write_at).to eq(1010.0)

      # The stale callback cannot refresh the next connection's timers. The
      # new monitor still reports the initial time and no I/O in either
      # direction, even though the old connection just completed writes.
      expect(new_monitor.frames_written).to eq(0)
      expect(new_monitor.last_write_at).to eq(1000.0)
      expect(new_monitor.frames_read).to eq(0)
      expect(new_monitor.last_read_at).to eq(1000.0)

      #
      # When the next connection actually emits a frame, only its own write
      # side is updated.
      #
      clock.advance(3)
      new_transport.write(publish_frame)

      expect(new_monitor.frames_written).to eq(1)
      expect(new_monitor.last_write_at).to eq(1013.0)
      expect(new_monitor.last_written_frame).to eq(publish_frame)

      # the old connection is left exactly as it was
      expect(old_monitor.frames_written).to eq(2)
      expect(old_monitor.last_write_at).to eq(1010.0)
    end
  end

  describe Bunny::HeartbeatSender do
    let(:logger) { Logger.new(IO::NULL) }

    # Builds a sender without spawning its background thread: the examples
    # drive #beat deterministically against the fake clock. The interval
    # mirrors HeartbeatSender#start for a period of 10: (10 / 2) - 1 = 4.
    def sender_for(transport, interval = 4)
      Bunny::HeartbeatSender.new(transport, logger).tap do |sender|
        sender.instance_variable_set(:@interval, interval)
      end
    end

    it "does not send while the write side made progress within the interval" do
      transport = new_transport(1)
      sender    = sender_for(transport)

      transport.write(publish_frame)

      clock.advance(3) # less than the 4s interval
      sender.send(:beat)

      expect(transport.written).not_to include(AMQ::Protocol::HeartbeatFrame.encode)
      expect(transport.network_activity.frames_written).to eq(1)
    end

    it "sends on an idle connection once the write interval has elapsed" do
      transport = new_transport(1)
      sender    = sender_for(transport)

      clock.advance(5) # past the 4s interval
      sender.send(:beat)

      expect(transport.written).to include(AMQ::Protocol::HeartbeatFrame.encode)
      # the heartbeat itself is a real completed write, so it is recorded
      expect(transport.network_activity.frames_written).to eq(1)
      expect(transport.network_activity.last_written_frame).to eq(
        AMQ::Protocol::HeartbeatFrame.encode
      )
    end

    it "keeps the idle-connection sending convention: incoming frames do not defer heartbeats" do
      transport = new_transport(1)
      sender    = sender_for(transport)

      clock.advance(5)
      transport.receive(legal_frame) # peer is still sending...

      sender.send(:beat)

      # ...but read-side activity alone does not suppress the heartbeat
      expect(transport.written).to include(AMQ::Protocol::HeartbeatFrame.encode)
    end

    it "does not let a stuck write defer the heartbeat even while reads succeed" do
      transport = new_transport(1)
      sender    = sender_for(transport)

      block_writes(transport, blocked_publishes)
      transport.write(publish_frame) # queued, never confirmed

      clock.advance(5)
      transport.receive(legal_frame)

      # the heartbeat uses write_without_timeout which is also stuck here,
      # so restore the callback first to let just the heartbeat through and
      # assert it was due based on the unconfirmed write side
      transport.restore_write_callback
      sender.send(:beat)

      heartbeat = AMQ::Protocol::HeartbeatFrame.encode
      expect(transport.written).to include(heartbeat)
      # only the heartbeat completed; the original publish is still queued
      expect(blocked_publishes.size).to eq(1)
    end

    it "drives the monitor of its own transport and never another connection's" do
      old_transport = new_transport(1)
      new_transport = new_transport(2)
      sender        = sender_for(new_transport)

      clock.advance(5)
      sender.send(:beat)

      expect(new_transport.network_activity.frames_written).to eq(1)
      expect(old_transport.network_activity.frames_written).to eq(0)
    end
  end
end
