# -*- coding: utf-8 -*-
# frozen_string_literal: true
require "spec_helper"

describe Bunny::Transport, "network activity instrumentation" do
  # Drives the monitor of every Transport created in the example.
  let(:clock)  { ManualClock.new(500.0) }
  let(:logger) { Logger.new(IO::NULL) }

  # Minimal session surface Transport#initialize relies on.
  let(:session) do
    double("Bunny::Session",
           logger:        logger,
           mutex_impl:    Monitor,
           automatically_recover?: false)
  end

  def with_loopback
    server = TCPServer.new("127.0.0.1", 0)
    port   = server.addr[1]
    client = Bunny::SocketImpl.open("127.0.0.1", port)
    peer   = server.accept

    transport = Bunny::Transport.new(
      session, "127.0.0.1", port,
      session_error_handler: double("error handler"),
      network_activity_clock: clock
    )
    transport.instance_variable_set(:@socket, client)
    transport.instance_variable_set(:@status, :connected)

    yield transport, peer
  ensure
    peer&.close
    client&.close
    server&.close
  end

  it "exposes a monitor with a unique connection id" do
    with_loopback do |transport, _peer|
      first  = transport.network_activity
      second = Bunny::Transport.new(
        session, "127.0.0.1", 1,
        session_error_handler: double("error handler"),
        network_activity_clock: clock
      ).network_activity

      expect(first).to be_a(Bunny::NetworkActivityMonitor)
      expect(second.connection_id).to eq(first.connection_id + 1)
    end
  end

  it "records the exact frame bytes only once the write completed" do
    with_loopback do |transport, peer|
      frame = AMQ::Protocol::HeartbeatFrame.encode

      clock.advance(10)
      transport.write_without_timeout(frame)

      monitor = transport.network_activity
      expect(monitor.frames_written).to eq(1)
      expect(monitor.last_written_frame).to eq(frame)
      expect(monitor.last_write_at).to eq(510.0)

      # the bytes really did reach the local peer
      received = peer.read_nonblock(8)
      expect(received).to eq(frame)
    end
  end

  it "records a complete legal frame on the read side" do
    with_loopback do |transport, peer|
      frame = AMQ::Protocol::HeartbeatFrame.encode
      peer.write(frame)
      peer.flush

      clock.advance(4)
      read_frame = transport.read_next_frame

      monitor = transport.network_activity
      expect(read_frame).to be_a(AMQ::Protocol::HeartbeatFrame)
      expect(monitor.frames_read).to eq(1)
      expect(monitor.last_read_frame).to be_a(AMQ::Protocol::HeartbeatFrame)
      expect(monitor.last_read_at).to eq(504.0)
      # reading leaves the write timer untouched
      expect(monitor.frames_written).to eq(0)
      expect(monitor.last_write_at).to eq(500.0)
    end
  end

  class ManualClock
    def initialize(start)
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
