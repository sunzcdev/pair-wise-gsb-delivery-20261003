# frozen_string_literal: true

require "spec_helper"

describe Bunny::NetworkActivityMonitor do
  # A deterministic clock: time only moves when the test says so.
  let(:clock) { FakeMonotonicClock.new(100.0) }

  def new_monitor(connection_id: 1)
    described_class.new(connection_id: connection_id, clock: clock)
  end

  it "stamps both directions with the current time when created" do
    monitor = new_monitor

    expect(monitor.last_write_at).to eq(100.0)
    expect(monitor.last_read_at).to eq(100.0)
    expect(monitor.frames_written).to eq(0)
    expect(monitor.frames_read).to eq(0)
  end

  it "records the exact bytes and timestamp of a completed write" do
    monitor = new_monitor
    frame   = AMQ::Protocol::HeartbeatFrame.encode

    clock.advance(5)
    monitor.record_write!(frame)

    expect(monitor.last_written_frame).to eq(frame)
    expect(monitor.frames_written).to eq(1)
    expect(monitor.last_write_at).to eq(105.0)
  end

  it "records a received frame and its timestamp on the read side only" do
    monitor = new_monitor
    frame   = AMQ::Protocol::Frame.new(1, "payload", 1)

    clock.advance(7)
    monitor.record_read!(frame)

    expect(monitor.last_read_frame).to equal(frame)
    expect(monitor.frames_read).to eq(1)
    expect(monitor.last_read_at).to eq(107.0)

    # reading never moves the write-side timer
    expect(monitor.last_write_at).to eq(100.0)
    expect(monitor.frames_written).to eq(0)
  end

  it "keeps the two directions independent: writes do not touch the read timer" do
    monitor = new_monitor

    clock.advance(3)
    monitor.record_write!(AMQ::Protocol::HeartbeatFrame.encode)

    expect(monitor.last_write_at).to eq(103.0)
    expect(monitor.last_read_at).to eq(100.0)
  end

  describe "#write_idle_for? / #read_idle_for?" do
    it "answers against the timestamp of the matching direction" do
      monitor = new_monitor

      clock.advance(4)
      monitor.record_write!("frame-1")

      clock.advance(4)
      monitor.record_read!(AMQ::Protocol::Frame.new(1, "x", 0))

      # last write was 4 seconds ago (at 104), last read was just now (at 108)
      expect(monitor.write_idle_for?(4)).to be(true)
      expect(monitor.write_idle_for?(5)).to be(false)
      expect(monitor.read_idle_for?(0.1)).to be(false)
      expect(monitor.read_idle_for?(1)).to be(false)

      clock.advance(2)
      expect(monitor.read_idle_for?(1)).to be(true)
      expect(monitor.write_idle_for?(5)).to be(true)
    end  end

  describe "#to_h and #report" do
    it "produces a snapshot that explains progress in each direction" do
      monitor = new_monitor(connection_id: 42)
      written = AMQ::Protocol::HeartbeatFrame.encode
      read    = AMQ::Protocol::Frame.new(1, "p", 0)
      monitor.record_write!(written)
      monitor.record_read!(read)

      report = monitor.to_h

      expect(report[:connection_id]).to eq(42)
      expect(report[:frames_written]).to eq(1)
      expect(report[:frames_read]).to eq(1)
      expect(report[:last_written_frame]).to eq(written)
      expect(report[:last_read_frame]).to equal(read)
      expect(report[:last_write_at]).to eq(report[:last_read_at])

      text = monitor.report
      expect(text).to include("connection=42")
      expect(text).to include("writes=1")
      expect(text).to include("reads=1")
    end
  end

  describe "connection isolation" do
    it "lets two monitors track two connections without cross-talk" do
      old_connection = new_monitor(connection_id: 1)
      new_connection = new_monitor(connection_id: 2)

      clock.advance(10)
      # a write that eventually completes on the *old* connection
      old_connection.record_write!("late-frame")

      expect(old_connection.last_write_at).to eq(110.0)
      expect(new_connection.last_write_at).to eq(100.0)
      expect(new_connection.frames_written).to eq(0)
    end
  end

  # Minimal controllable monotonic clock used across the heartbeat specs.
  class FakeMonotonicClock
    def initialize(start = 0.0)
      @now = start
    end

    def advance(seconds)
      @now += seconds
    end

    def call
      @now
    end
  end
end
