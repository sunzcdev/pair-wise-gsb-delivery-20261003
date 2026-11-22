# frozen_string_literal: true

require_relative "test_helper"

class GRPCMessageTest < Minitest::Test
  include HTTPX

  GRPC_HEADER_SIZE = 5

  def setup
    # make sure the grpc plugin dependencies (message/encoding/call) are loaded
    HTTPX.plugin(:grpc)
  end

  # builds a length-prefixed GRPC frame for +payload+
  def grpc_frame(payload, compressed: 0)
    [compressed, payload.bytesize].pack("CL>") + payload.b
  end

  # minimal response double which behaves like the streaming responses passed
  # to the GRPC message decoder: it yields the precomputed transport chunks in
  # order and exposes response headers/trailers.
  def build_stream_response(chunks, grpc_status: 0, grpc_message: "OK")
    headers = Headers.new(
      "grpc-status" => grpc_status.to_s,
      "grpc-message" => grpc_message
    )
    response = Object.new
    body = Object.new
    def body.initialize_inflater_by_encoding(*, **); raise "compressed frames aren't expected in these tests"; end
    response.define_singleton_method(:headers) { headers }
    response.define_singleton_method(:body) { body }
    response.define_singleton_method(:trailing_metadata) { headers.to_h }
    response.define_singleton_method(:raise_for_status) { self }
    response.define_singleton_method(:close) {}
    response.define_singleton_method(:each) do |&blk|
      chunks.each(&blk)
    end
    response
  end

  def stream_messages(response)
    messages = []
    HTTPX::Plugins::GRPC::Message.stream(response) do |message|
      messages << message
    end
    messages
  end

  def test_grpc_stream_decodes_messages_split_across_chunks
    payload = grpc_frame("first message") + grpc_frame("second")

    # feed the stream one byte at a time, including a 3-way split of the first header
    chunks = payload.each_char.to_a

    response = build_stream_response(chunks)

    assert_equal ["first message", "second"], stream_messages(response)
  end

  def test_grpc_stream_delivers_legal_zero_length_message
    payload = grpc_frame("data") + grpc_frame("")

    # cut in the middle of the zero-length header, then again inside the data message
    chunks = [
      payload.byteslice(0, 7),
      payload.byteslice(7, 3),
      payload.byteslice(10..-1)
    ]

    response = build_stream_response(chunks)

    assert_equal ["data", ""], stream_messages(response)
  end

  def test_grpc_stream_decodes_when_a_single_header_arrives_in_three_reads
    frame = grpc_frame("payload-body")
    # 5-byte header split as 1 + 3 + 1 bytes, payload delivered afterwards
    chunks = [
      frame.byteslice(0, 1),
      frame.byteslice(1, 3),
      frame.byteslice(4..-1)
    ]

    response = build_stream_response(chunks)

    assert_equal ["payload-body"], stream_messages(response)
  end

  def test_grpc_stream_complete_transport_with_failing_grpc_status
    payload = grpc_frame("first") + grpc_frame("second")
    response = build_stream_response([payload], grpc_status: 13, grpc_message: "boom")

    delivered = []
    error = assert_raises(GRPCError) do
      HTTPX::Plugins::GRPC::Message.stream(response) { |message| delivered << message }
    end

    assert_equal 13, error.status
    assert_equal "boom", error.details
    # messages fully delivered before the trailing business failure stay visible
    assert_equal %w[first second], delivered
  end

  def test_grpc_stream_truncated_header_after_complete_message
    first_frame = grpc_frame("first")
    truncated_header = [0, 9].pack("CL>")

    # cut the truncated header at every boundary before it becomes complete (1..4 bytes)
    1.upto(GRPC_HEADER_SIZE - 1) do |cut|
      response = build_stream_response([first_frame, truncated_header.byteslice(0, cut)])

      delivered = []
      error = assert_raises(GRPCMessageHeaderError) do
        HTTPX::Plugins::GRPC::Message.stream(response) { |message| delivered << message }
      end

      assert_equal cut, error.message[/got (\d+)/, 1].to_i
      assert_equal ["first"], delivered
    end
  end

  def test_grpc_stream_truncated_header_with_error_terminal
    first_frame = grpc_frame("first")
    truncated_header = [0, 9].pack("CL>").byteslice(0, 2)

    response = build_stream_response([first_frame, truncated_header], grpc_status: 13, grpc_message: "boom")

    delivered = []
    # framing is broken before grpc-status can be evaluated: framing error wins
    assert_raises(GRPCMessageHeaderError) do
      HTTPX::Plugins::GRPC::Message.stream(response) { |message| delivered << message }
    end

    assert_equal ["first"], delivered
  end

  def test_grpc_stream_truncated_payload_after_complete_message
    first_frame = grpc_frame("first")
    second_payload = "partial"
    second_frame = grpc_frame(second_payload + "missing-tail")

    # cut the second payload at every available boundary
    0.upto(second_payload.bytesize) do |cut|
      chunks = [first_frame, second_frame.byteslice(0, GRPC_HEADER_SIZE + cut)]
      response = build_stream_response(chunks)

      delivered = []
      error = assert_raises(GRPCMessagePayloadError) do
        HTTPX::Plugins::GRPC::Message.stream(response) { |message| delivered << message }
      end

      assert_equal second_frame.bytesize - GRPC_HEADER_SIZE, error.message[/expected (\d+)/, 1].to_i
      assert_equal cut, error.message[/got (\d+)/, 1].to_i
      assert_equal ["first"], delivered
    end
  end

  def test_grpc_stream_truncated_payload_with_error_terminal
    first_frame = grpc_frame("first")
    truncated_frame = grpc_frame("full-payload-here").byteslice(0, GRPC_HEADER_SIZE + 4)

    response = build_stream_response([first_frame, truncated_frame], grpc_status: 13, grpc_message: "boom")

    delivered = []
    assert_raises(GRPCMessagePayloadError) do
      HTTPX::Plugins::GRPC::Message.stream(response) { |message| delivered << message }
    end

    assert_equal ["first"], delivered
  end

  def test_grpc_stream_every_byte_boundary_delivers_consistent_messages
    first = grpc_frame("one")
    second = grpc_frame("two-two")
    payload = first + second

    # slice the whole byte stream at each possible cut point, checking how many
    # complete messages the client receives before either a clean end or an error
    0.upto(payload.bytesize) do |cut|
      response = build_stream_response([payload.byteslice(0, cut)])

      delivered = []
      begin
        HTTPX::Plugins::GRPC::Message.stream(response) { |message| delivered << message }
      rescue GRPCMessageError
        # expected once the cut lands inside a header or payload
      end

      case cut
      when 0...first.bytesize
        # cut inside the first message: nothing delivered
        assert_equal [], delivered, "unexpected delivery at cut #{cut}"
      when first.bytesize...(first.bytesize + GRPC_HEADER_SIZE)
        # cut inside the second header
        assert_equal ["one"], delivered, "unexpected delivery at cut #{cut}"
      when (first.bytesize + GRPC_HEADER_SIZE)...payload.bytesize
        # cut inside the second payload
        assert_equal ["one"], delivered, "unexpected delivery at cut #{cut}"
      else
        assert_equal %w[one two-two], delivered, "unexpected delivery at cut #{cut}"
      end
    end
  end

  def test_grpc_stream_every_byte_boundary_with_error_terminal
    first = grpc_frame("one")
    second = grpc_frame("two-two")
    payload = first + second

    0.upto(payload.bytesize) do |cut|
      response = build_stream_response([payload.byteslice(0, cut)], grpc_status: 13, grpc_message: "boom")

      delivered = []
      begin
        HTTPX::Plugins::GRPC::Message.stream(response) { |message| delivered << message }
        flunk "expected an error terminal at cut #{cut}"
      rescue GRPCMessageError
        # a cut strictly inside a header or a payload is a framing error, regardless
        # of the grpc-status trailer, and already delivered messages stay visible
        assert_operator cut, :<, payload.bytesize
        assert_equal ["one"], delivered unless cut < first.bytesize
        assert_equal [], delivered if cut < first.bytesize
      rescue GRPCError
        # no half-delivered message remains (boundary cuts, including the empty
        # stream): the grpc business failure is the terminal instead
        assert [0, first.bytesize, payload.bytesize].include?(cut), "unexpected business error at cut #{cut}"
        assert_equal %w[one two-two], delivered if cut == payload.bytesize
        assert_equal ["one"], delivered if cut == first.bytesize
        assert_equal [], delivered if cut.zero?
      end
    end
  end

  def test_grpc_unary_decodes_zero_length_message
    response = build_stream_response([grpc_frame("")])

    assert_equal "", HTTPX::Plugins::GRPC::Message.unary(response)
  end

  def test_grpc_unary_assembles_frame_split_across_reads
    frame = grpc_frame("unary-reply")
    chunks = [
      frame.byteslice(0, 2),
      frame.byteslice(2, 2),
      frame.byteslice(4..-1)
    ]

    response = build_stream_response(chunks)

    assert_equal "unary-reply", HTTPX::Plugins::GRPC::Message.unary(response)
  end

  def test_grpc_unary_raises_on_truncated_header
    response = build_stream_response([[0, 4].pack("CL>").byteslice(0, 3)])

    assert_raises(GRPCMessageHeaderError) do
      HTTPX::Plugins::GRPC::Message.unary(response)
    end
  end

  def test_grpc_unary_raises_on_truncated_payload
    frame = grpc_frame("complete-please").byteslice(0, GRPC_HEADER_SIZE + 8)
    response = build_stream_response([frame])

    assert_raises(GRPCMessagePayloadError) do
      HTTPX::Plugins::GRPC::Message.unary(response)
    end
  end
end
