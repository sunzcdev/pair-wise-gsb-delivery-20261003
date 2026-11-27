# -*- coding: utf-8 -*-
# frozen_string_literal: true
require "spec_helper"

# A transport that records the bytes it is given instead of talking to a
# broker. It can be told to accept only some of them before failing, which is
# what makes "some of the message is on the wire" reproducible locally.
class RecordingPublishTransport
  attr_reader :writes
  # @return [Proc, nil] called during a failing write, e.g. to simulate a
  #   reconnection racing with the failure
  attr_accessor :on_failure

  # @param [Integer, nil] fail_after how many bytes to accept before failing,
  #   nil never fails
  def initialize(fail_after: nil)
    @fail_after = fail_after
    @writes     = []
    @open       = true
    @on_failure = nil
  end

  def write_for_publish(data, progress, with_timeout: true)
    if @fail_after
      acceptable = [@fail_after - progress.bytes_written, 0].max
      chunk      = data.byteslice(0, acceptable)
      unless chunk.empty?
        @writes << chunk.dup
        progress.advance(chunk.bytesize)
      end
      @open = false
      @on_failure.call if @on_failure
      raise Bunny::PublishWriteError.new(IOError.new("simulated write failure"))
    end

    @writes << data.dup
    progress.advance(data.bytesize)
    progress.bytes_written
  end

  def open?
    @open
  end

  def close(reason = nil)
    @open = false
  end

  def bytes_written
    @writes.sum(&:bytesize)
  end

  def bytes
    @writes.join
  end
end

describe Bunny::Session do
  # A published message is attributed to the connection it is written to. A
  # message no transport has seen any byte of can be encoded again for the
  # connection that replaced the one it was encoded for; a message a transport
  # accepted only partially has an unknown write result and is neither
  # continued on another connection nor re-sent.

  let(:logger) { Logger.new(IO::NULL) }
  let(:payload) { "z" * 20_000 }

  def new_session
    Bunny.new(host: "127.0.0.1", port: 65_534, logger: logger, threaded: false)
  end

  # Puts the session into the state a started one is in, without connecting.
  def session_with(transport, frame_max:, generation: 1)
    session = new_session
    session.instance_variable_set(:@transport, transport)
    session.instance_variable_set(:@transport_generation, generation)
    session.instance_variable_set(:@frame_max, frame_max)
    session.instance_variable_set(:@status, :open)
    session.instance_variable_set(:@channel_id_allocator, Bunny::ChannelIdAllocator.new(2047))
    allow(session).to receive(:handle_network_failure)
    allow(session).to receive(:signal_activity!)
    session
  end

  def channel_for(session, frame_max:)
    channel = Bunny::Channel.allocate
    channel.instance_variable_set(:@connection, session)
    channel.instance_variable_set(:@logger, logger)
    channel.instance_variable_set(:@id, 1)
    channel.instance_variable_set(:@status, :open)
    channel.instance_variable_set(:@frame_max, frame_max)
    channel.instance_variable_set(:@publishing_mutex, Monitor.new)
    channel
  end

  # Replaces the connection the session uses, the way recovery does.
  def replace_connection(session, transport, frame_max:, generation:)
    session.instance_variable_set(:@transport, transport)
    session.instance_variable_set(:@transport_generation, generation)
    session.instance_variable_set(:@frame_max, frame_max)
  end

  def encode_publish(payload, frame_max, channel_id = 1)
    AMQ::Protocol::Basic::Publish.encode(channel_id, payload,
      { content_type: "application/octet-stream" }, "", "q", false, false, frame_max)
  end

  def publish_frames(channel, payload, frame_max)
    encode_publish(payload, frame_max, channel.id)
  end

  # Publishes through the same entry point Channel#basic_publish uses.
  def publish(session, channel, payload, frame_max:)
    frames = publish_frames(channel, payload, frame_max)
    session.send_frameset(frames, frame_max, channel) do |reencode_for|
      publish_frames(channel, payload, reencode_for)
    end
  end

  # Encoded frames of a recorded write stream: [type, channel, payload size].
  def recorded_frames(bytes)
    frames = []
    offset = 0
    while offset < bytes.bytesize
      type, channel, size = AMQ::Protocol::Frame.decode_header(bytes.byteslice(offset, 7))
      frames << [type, channel, size]
      offset += 7 + size + 1
    end
    frames
  end

  def total_bytes(payload, frame_max)
    encode_publish(payload, frame_max).sum { |frame| frame.encode.bytesize }
  end

  # Bytes of the method frame, the header frame and part of the first body one.
  def bytes_through_first_body_segment(payload, frame_max)
    frames = encode_publish(payload, frame_max)
    frames[0].encode.bytesize + frames[1].encode.bytesize + 100
  end

  describe "when the connection is replaced after a message is encoded but before it is written" do
    it "writes the message to the new connection from a complete new header, honouring its frame_max" do
      old_transport = RecordingPublishTransport.new
      session       = session_with(old_transport, frame_max: 131_072)
      channel       = channel_for(session, frame_max: 131_072)

      # the message was encoded for the connection in use at the time
      stale_frames = publish_frames(channel, payload, 131_072)

      # then a reconnection replaced that connection before anything was written
      new_transport = RecordingPublishTransport.new
      replace_connection(session, new_transport, frame_max: 4_096, generation: 2)

      session.send_frameset(stale_frames, 131_072, channel) do |reencode_for|
        publish_frames(channel, payload, reencode_for)
      end

      expect(old_transport.writes).to be_empty

      frames = recorded_frames(new_transport.bytes)

      # the stream starts with this message's method frame, not with a
      # continuation of frames encoded for the old connection
      expect(frames.first[0]).to eq :method
      expect(frames.first[1]).to eq channel.id
      expect(frames.first[2]).to be > 0

      # and every fragment was framed for the newly negotiated limit
      expect(frames.map(&:last)).to all(be <= 4_096 - 8)
    end

    it "leaves nothing on the old transport" do
      old_transport = RecordingPublishTransport.new
      session       = session_with(old_transport, frame_max: 131_072)
      channel       = channel_for(session, frame_max: 131_072)

      replace_connection(session, RecordingPublishTransport.new, frame_max: 131_072, generation: 2)

      publish(session, channel, payload, frame_max: 131_072)

      expect(old_transport.writes).to be_empty
    end
  end

  describe "when the old transport fails after the header and the first body segment were written" do
    let(:fail_after) { bytes_through_first_body_segment(payload, 4_096) }

    def failing_session(fail_after)
      old_transport = RecordingPublishTransport.new(fail_after: fail_after)
      session       = session_with(old_transport, frame_max: 4_096)
      channel       = channel_for(session, frame_max: 4_096)
      [session, channel, old_transport]
    end

    it "reports a partial write precisely" do
      session, channel, = failing_session(fail_after)

      expect do
        publish(session, channel, payload, frame_max: 4_096)
      end.to raise_error(Bunny::IncompletePublish) do |error|
        expect(error.bytes_written).to eq fail_after
        expect(error.bytes_total).to eq total_bytes(payload, 4_096)
      end
    end

    it "neither continues the message on a replacement connection nor re-sends it" do
      session, channel, old_transport = failing_session(fail_after)

      new_transport = RecordingPublishTransport.new
      old_transport.on_failure = proc do
        replace_connection(session, new_transport, frame_max: 4_096, generation: 2)
      end

      expect do
        publish(session, channel, payload, frame_max: 4_096)
      end.to raise_error(Bunny::IncompletePublish)

      # only the failed transport saw any of the message
      expect(old_transport.bytes_written).to eq fail_after
      expect(new_transport.writes).to be_empty
    end

    it "lets the next publish start from a complete new header and satisfy the new frame_max" do
      session, channel, old_transport = failing_session(fail_after)

      new_transport = RecordingPublishTransport.new
      old_transport.on_failure = proc do
        replace_connection(session, new_transport, frame_max: 2_048, generation: 2)
      end

      begin
        publish(session, channel, payload, frame_max: 4_096)
      rescue Bunny::IncompletePublish
        # expected: the first message has an unknown write result
      end

      publish(session, channel, payload, frame_max: 2_048)

      frames = recorded_frames(new_transport.bytes)

      # the next write begins with a method frame for a fresh message
      expect(frames.first[0]).to eq :method
      expect(frames.first[1]).to eq channel.id
      # and every fragment satisfies the newly negotiated limit
      expect(frames.map(&:last)).to all(be <= 2_048 - 8)
    end
  end

  describe "the receipts of the two switches are distinguishable" do
    it "reports the first as a completed publish and the second as an unknown write result" do
      # first switch: after encoding, before writing
      session = session_with(RecordingPublishTransport.new, frame_max: 4_096)
      channel = channel_for(session, frame_max: 4_096)
      stale   = publish_frames(channel, payload, 4_096)

      replace_connection(session, RecordingPublishTransport.new, frame_max: 4_096, generation: 2)

      first = begin
                session.send_frameset(stale, 4_096, channel) do |reencode_for|
                  publish_frames(channel, payload, reencode_for)
                end
                :accepted
              rescue Bunny::IncompletePublish => e
                e
              end

      # second switch: the old transport wrote the header and the first body
      # segment before failing
      fail_after    = bytes_through_first_body_segment(payload, 4_096)
      old_transport = RecordingPublishTransport.new(fail_after: fail_after)
      session2      = session_with(old_transport, frame_max: 4_096)
      channel2      = channel_for(session2, frame_max: 4_096)

      second = begin
                 publish(session2, channel2, payload, frame_max: 4_096)
                 :accepted
               rescue Bunny::IncompletePublish => e
                 e
               end

      # the first receipt says the message never reached the old connection,
      # the second says its write result is unknown
      expect(first).to eq :accepted
      expect(second).to be_a Bunny::IncompletePublish
      expect(second.bytes_written).to eq fail_after
    end
  end

  describe "when a zero-byte failure is followed by a successful recovery" do
    it "prepares the message again for the connection now in use" do
      old_transport = RecordingPublishTransport.new(fail_after: 0)
      session       = session_with(old_transport, frame_max: 4_096)
      channel       = channel_for(session, frame_max: 4_096)

      replacement = RecordingPublishTransport.new
      allow(session).to receive(:handle_network_failure) do |_reason|
        replace_connection(session, replacement, frame_max: 2_048, generation: 2)
      end

      publish(session, channel, payload, frame_max: 4_096)

      expect(old_transport.bytes_written).to eq 0
      expect(replacement.writes.size).to eq 1

      # the message was encoded for the replacement connection's frame_max
      expect(recorded_frames(replacement.bytes).map(&:last)).to all(be <= 2_048 - 8)
    end
  end

  describe "when the write result cannot be determined" do
    # a blocking socket write can fail mid-buffer with no way to tell how many
    # bytes it accepted, unlike the non-blocking one used by default
    class UnknownProgressTransport < RecordingPublishTransport
      def write_for_publish(data, progress, with_timeout: true)
        progress.unknown!
        @open = false
        raise Bunny::PublishWriteError.new(IOError.new("simulated blocking write failure"))
      end
    end

    it "reports the write result as unknown rather than as a completed publish" do
      transport = UnknownProgressTransport.new
      session   = session_with(transport, frame_max: 4_096)
      channel   = channel_for(session, frame_max: 4_096)

      expect do
        publish(session, channel, payload, frame_max: 4_096)
      end.to raise_error(Bunny::IncompletePublish) do |error|
        expect(error.bytes_total).to eq total_bytes(payload, 4_096)
      end
    end
  end
end
