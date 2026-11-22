# frozen_string_literal: true

require_relative "helper"
require_relative "helpers/test_puma/puma_socket"
require "puma/events"
require "puma/server"

# Tests for Rack response bodies that respond to `call` (streaming bodies).
#
# When a body implements both `each` and `call`, the server decides which
# interface to use based on its own capability (the `streaming_body` option),
# before any response headers are sent, so headers and body termination stay
# consistent (Content-Length, chunked, or connection close).
#
# Bodies that only implement `each` (plain old bodies) keep working.
class TestPumaServerStreamBody < PumaTest
  parallelize_me!

  include TestPuma
  include TestPuma::PumaSocket

  HOST = HOST4

  # A body implementing both `each` and `call`, counting calls to each
  # interface so tests can verify which one the server used.
  class DualBody
    attr_reader :each_count, :call_count

    def initialize(parts)
      @parts = parts
      @each_count = 0
      @call_count = 0
    end

    def each(&block)
      @each_count += 1
      @parts.each(&block)
    end

    def call(stream)
      @call_count += 1
      @parts.each { |part| stream.write part }
      stream.close
    end
  end

  # A plain old body, only implements `each`
  class EachOnlyBody
    def initialize(parts)
      @parts = parts
    end

    def each(&block)
      @parts.each(&block)
    end
  end

  def setup
    @host = HOST
    @log_writer = Puma::LogWriter.strings
    @events = Puma::Events.new
  end

  def teardown
    @server.stop(true)
  end

  def server_run(**options, &block)
    options[:log_writer]  ||= @log_writer
    options[:min_threads] ||= 1
    @server = Puma::Server.new block, @events, options
    @bind_port = (@server.add_tcp_listener @host, 0).addr[1]
    @server.run
  end

  # Server supports streaming bodies (default): a dual-interface body must be
  # consumed via `call`, and the response must be chunked with proper
  # termination.  Also verifies the exact write order on the wire.
  def test_dual_body_uses_call_when_streaming_supported
    body = DualBody.new ["Hello ", "World"]
    server_run { |_env| [200, { 'content-type' => 'text/plain' }, body] }

    resp = send_http("GET / HTTP/1.1\r\nHost: test.com\r\nConnection: close\r\n\r\n").read_all

    expected = "HTTP/1.1 200 OK\r\n" \
      "content-type: text/plain\r\n" \
      "connection: close\r\n" \
      "transfer-encoding: chunked\r\n" \
      "\r\n" \
      "6\r\nHello \r\n" \
      "5\r\nWorld\r\n" \
      "0\r\n\r\n"

    assert_equal expected, resp
    assert_equal 1, body.call_count
    assert_equal 0, body.each_count
  end

  # Server does not support streaming bodies: the same dual-interface body
  # must be consumed via `each`, with identical wire format.
  def test_dual_body_uses_each_when_streaming_not_supported
    body = DualBody.new ["Hello ", "World"]
    server_run(streaming_body: false) { |_env| [200, { 'content-type' => 'text/plain' }, body] }

    resp = send_http("GET / HTTP/1.1\r\nHost: test.com\r\nConnection: close\r\n\r\n").read_all

    expected = "HTTP/1.1 200 OK\r\n" \
      "content-type: text/plain\r\n" \
      "connection: close\r\n" \
      "transfer-encoding: chunked\r\n" \
      "\r\n" \
      "6\r\nHello \r\n" \
      "5\r\nWorld\r\n" \
      "0\r\n\r\n"

    assert_equal expected, resp
    assert_equal 0, body.call_count
    assert_equal 1, body.each_count
  end

  # Streaming body with a known length: Content-Length header is formed and
  # the connection stays keep-alive, so a second request can be served on the
  # same connection.
  def test_dual_body_streaming_with_content_length_keep_alive
    body = DualBody.new ["Hello World"]
    server_run { |_env| [200, { 'content-length' => '11' }, body] }

    socket = send_http GET_11
    resp1 = socket.read_response
    assert_equal "HTTP/1.1 200 OK", resp1.status
    assert_equal "11", resp1.headers_hash['content-length']
    refute resp1.headers_hash.key? 'transfer-encoding'
    assert_equal "Hello World", resp1.body

    socket.req_write GET_11
    resp2 = socket.read_response
    assert_equal "Hello World", resp2.body

    assert_equal 2, body.call_count
    assert_equal 0, body.each_count
  end

  # Streaming body over HTTP/1.0: no length and no chunking available, so the
  # body is terminated by closing the connection.  Headers must not contain
  # Content-Length or Transfer-Encoding.
  def test_dual_body_streaming_http10_terminated_by_close
    body = DualBody.new ["Hello World"]
    server_run { |_env| [200, {}, body] }

    resp = send_http(GET_10).read_all

    assert_equal "HTTP/1.0 200 OK\r\n\r\nHello World", resp
    assert_equal 1, body.call_count
    assert_equal 0, body.each_count
  end

  # An empty streaming body still gets headers and the chunked terminator.
  def test_streaming_body_empty
    body = DualBody.new []
    server_run { |_env| [200, {}, body] }

    resp = send_http("GET / HTTP/1.1\r\nHost: test.com\r\nConnection: close\r\n\r\n").read_all

    expected = "HTTP/1.1 200 OK\r\n" \
      "connection: close\r\n" \
      "transfer-encoding: chunked\r\n" \
      "\r\n" \
      "0\r\n\r\n"

    assert_equal expected, resp
    assert_equal 1, body.call_count
    assert_equal 0, body.each_count
  end

  # A plain old body that only implements `each` keeps working; verifies the
  # exact write order of the chunked response on the wire.
  def test_each_only_body_write_order
    body = EachOnlyBody.new ["one", "two", "three"]
    server_run { |_env| [200, {}, body] }

    resp = send_http("GET / HTTP/1.1\r\nHost: test.com\r\nConnection: close\r\n\r\n").read_all

    expected = "HTTP/1.1 200 OK\r\n" \
      "connection: close\r\n" \
      "transfer-encoding: chunked\r\n" \
      "\r\n" \
      "3\r\none\r\n" \
      "3\r\ntwo\r\n" \
      "5\r\nthree\r\n" \
      "0\r\n\r\n"

    assert_equal expected, resp
  end

  # A plain old body that only implements `each`, with a Content-Length set by
  # the app: no chunking, exact write order on the wire.
  def test_each_only_body_with_content_length_write_order
    body = EachOnlyBody.new ["Hello World"]
    server_run { |_env| [200, { 'content-length' => '11' }, body] }

    resp = send_http("GET / HTTP/1.1\r\nHost: test.com\r\nConnection: close\r\n\r\n").read_all

    expected = "HTTP/1.1 200 OK\r\n" \
      "connection: close\r\n" \
      "content-length: 11\r\n" \
      "\r\n" \
      "Hello World"

    assert_equal expected, resp
  end
end