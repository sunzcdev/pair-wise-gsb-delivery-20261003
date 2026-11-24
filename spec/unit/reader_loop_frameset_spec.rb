# frozen_string_literal: true
require "spec_helper"
require "stringio"

describe Bunny::ReaderLoop do
  # The reader loop assembles a full frameset (method + header + one or more
  # body frames) before anything is handed to the session. A message split
  # across body frames must therefore be reconstructed as one content object
  # and routed by its leading method frame.
  let(:channel_id) { 1 }

  it "reassembles a get-ok message split across several body frames and keeps its objects" do
    body = "hello world"
    frames = get_ok_frames(channel_id, delivery_tag: 7, routing_key: "rk", body: body)
    session = FakeReaderSession.new
    transport = StringTransport.new(frames.map(&:encode).join)
    reader_loop = Bunny::ReaderLoop.new(transport, session, Object.new)

    reader_loop.run_once

    expect(session.calls.size).to eq(1)
    kind, (method, properties, content) = session.calls.first
    expect(kind).to eq(:frameset)
    expect(method).to be_a(AMQ::Protocol::Basic::GetOk)
    expect(method.delivery_tag).to eq(7)
    expect(content).to eq(body)
    expect(content.bytesize).to eq(11)
    expect(properties).to be_a(Hash)
  end

  it "routes an interleaved get-ok, get-empty and basic.deliver sequence correctly" do
    frames = []
    frames.concat(get_ok_frames(channel_id, delivery_tag: 1, routing_key: "a", body: "got"))
    frames << get_empty_frame(channel_id)
    frames.concat(deliver_frames(channel_id, consumer_tag: "ctag", delivery_tag: 2, body: "pushed"))

    session = FakeReaderSession.new
    transport = StringTransport.new(frames.map(&:encode).join)
    reader_loop = Bunny::ReaderLoop.new(transport, session, Object.new)

    3.times { reader_loop.run_once }

    methods = session.calls.map { |kind, payload| [kind, payload.first.class] }
    expect(methods).to eq([
      [:frameset, AMQ::Protocol::Basic::GetOk],
      [:frame, AMQ::Protocol::Basic::GetEmpty],
      [:frameset, AMQ::Protocol::Basic::Deliver]
    ])
    expect(session.calls.first.last.last).to eq("got")
    expect(session.calls.last.last.last).to eq("pushed")
  end

  it "preserves the exact assembled content even when every byte is its own body frame" do
    body = +"abc"
    frames = get_ok_frames(channel_id, delivery_tag: 3, routing_key: "rk", body: body, frame_size: 9)
    session = FakeReaderSession.new
    transport = StringTransport.new(frames.map(&:encode).join)
    reader_loop = Bunny::ReaderLoop.new(transport, session, Object.new)

    reader_loop.run_once

    expect(session.calls.first.last.last).to eq("abc")
    body_frames = frames.count { |f| f.is_a?(AMQ::Protocol::BodyFrame) }
    expect(body_frames).to eq(3)
  end

  def get_ok_frames(channel, delivery_tag:, routing_key:, body:, frame_size: 4096)
    buffer = AMQ::Protocol::Basic::GetOk.instance_variable_get(:@packed_indexes).dup
    buffer << AMQ::Pack.pack_uint64_big_endian(delivery_tag)
    buffer << [0].pack("C")
    buffer << "amq.direct".bytesize.chr << "amq.direct"
    buffer << routing_key.bytesize.chr << routing_key
    buffer << [0].pack("N")

    [AMQ::Protocol::MethodFrame.new(buffer, channel),
     AMQ::Protocol::HeaderFrame.new(encode_header(body), channel)] +
      AMQ::Protocol::Method.encode_body(body, channel, frame_size)
  end

  def deliver_frames(channel, consumer_tag:, delivery_tag:, body:)
    buffer = AMQ::Protocol::Basic::Deliver.instance_variable_get(:@packed_indexes).dup
    buffer << consumer_tag.bytesize.chr << consumer_tag
    buffer << AMQ::Pack.pack_uint64_big_endian(delivery_tag)
    buffer << [0].pack("C")
    buffer << "amq.direct".bytesize.chr << "amq.direct"
    buffer << "rk".bytesize.chr << "rk"

    [AMQ::Protocol::MethodFrame.new(buffer, channel),
     AMQ::Protocol::HeaderFrame.new(encode_header(body), channel),
     AMQ::Protocol::BodyFrame.new(body, channel)]
  end

  def get_empty_frame(channel)
    buffer = AMQ::Protocol::Basic::GetEmpty.instance_variable_get(:@packed_indexes).dup
    buffer << "".bytesize.chr
    AMQ::Protocol::MethodFrame.new(buffer, channel)
  end

  def encode_header(body)
    AMQ::Protocol::Basic.encode_properties(body.bytesize, content_type: "application/octet-stream")
  end

  class StringTransport
    def initialize(wire)
      @io = StringIO.new(wire)
    end

    def read_next_frame
      header = @io.read(7)
      return nil if header.nil? || header.empty?
      type, channel, size = AMQ::Protocol::Frame.decode_header(header)
      payload = @io.read(size)
      frame_end = @io.read(1)
      raise "missing frame end octet" unless frame_end == AMQ::Protocol::Frame::FINAL_OCTET
      AMQ::Protocol::Frame.new(type, payload, channel)
    end
  end

  class FakeReaderSession
    attr_reader :calls

    def initialize
      @calls = []
    end

    def handle_frameset(channel, frames)
      @calls << [:frameset, frames]
    end

    def handle_frame(channel, method)
      @calls << [:frame, [method]]
    end

    def logger
      Logger.new(IO::NULL)
    end

    def closing?; false; end
    def closed?; false; end
    def automatically_recover?; false; end
  end
end
