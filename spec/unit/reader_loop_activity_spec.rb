# -*- coding: utf-8 -*-
# frozen_string_literal: true
require "spec_helper"

describe Bunny::ReaderLoop, "read-side activity" do
  let(:clock)         { ManualClock.new(0.0) }
  let(:logger)        { Logger.new(IO::NULL) }
  let(:session)       { double("Bunny::Session", logger: logger, closing?: false, closed?: false) }
  let(:error_handler) { double("error handler") }

  # One real Transport over a loopback socket, fed by +peer+.
  def with_transport
    server = TCPServer.new("127.0.0.1", 0)
    port   = server.addr[1]
    client = Bunny::SocketImpl.open("127.0.0.1", port)
    peer   = server.accept

    transport = Bunny::Transport.new(
      double("session", logger: logger, mutex_impl: Monitor, automatically_recover?: false),
      "127.0.0.1", port,
      session_error_handler: error_handler,
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

  it "records every complete frame the reader loop actually reads, heartbeats included" do
    with_transport do |transport, peer|
      allow(session).to receive(:handle_frame)
      loop = Bunny::ReaderLoop.new(transport, session, error_handler)

      peer.write(AMQ::Protocol::HeartbeatFrame.encode)
      peer.flush
      clock.advance(9)
      loop.run_once # heartbeat: not dispatched, but still observed on the read side

      method_frame = AMQ::Protocol::Connection::Close.encode(200, "ok", 0, 0)
      peer.write(method_frame.encode)
      peer.flush
      clock.advance(1)
      loop.run_once # method frame: recorded and dispatched to the session

      activity = transport.network_activity
      expect(activity.frames_read).to eq(2)
      expect(activity.last_read_at).to eq(10.0)
      expect(activity.last_read_frame).to be_a(AMQ::Protocol::MethodFrame)
      expect(activity.frames_written).to eq(0)
      expect(session).to have_received(:handle_frame).with(0, instance_of(AMQ::Protocol::Connection::Close)).once
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
