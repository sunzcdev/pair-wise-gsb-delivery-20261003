# frozen_string_literal: false
require 'test/unit'
require 'net/http'
require 'stringio'
require 'socket'

# Exercises the Expect: 100-continue body admission boundary:
#
# - The upload source must not be read until the server admits the body
#   (a real 100 Continue) or the client explicitly adopts the wait-expiry
#   policy after the continue timeout.
# - A final response given before the body starts must leave the source
#   untouched.
# - Informational responses other than 100 do not admit the body.
# - A response that arrives after the body started is handled according to
#   what was actually sent.
#
# The source counts its reads, and the response sides are scripted sequences,
# so no external host is used.
class TestNetHTTPExpectContinue < Test::Unit::TestCase

  # A body source that records how often and how much it was read.
  class CountingSource
    attr_reader :reads, :bytes_read

    def initialize(data)
      @data = data.b
      @offset = 0
      @reads = 0
      @bytes_read = 0
    end

    def read(length = nil, outbuf = nil)
      @reads += 1
      if @offset >= @data.bytesize
        return nil
      end
      length ||= @data.bytesize - @offset
      chunk = @data.byteslice(@offset, length)
      @offset += chunk.bytesize
      @bytes_read += chunk.bytesize
      if outbuf
        outbuf.replace(chunk)
      else
        chunk
      end
    end

    def eof?
      @offset >= @data.bytesize
    end
  end

  # A stand-in for Net::BufferedIO that replays a scripted sequence of
  # responses and records everything written to the connection. A :timeout
  # step makes the next readability check fail, which models the continue
  # timeout expiring with nothing received.
  class ScriptedSocket
    attr_accessor :continue_timeout, :debug_output
    attr_reader :written

    def initialize(steps)
      @steps = steps.dup
      @rbuf = ''.b
      @written = ''.b
      @continue_timeout = 0.5
      @debug_output = nil
    end

    def io
      self
    end

    def to_io
      self
    end

    def wait_readable(_timeout)
      until @rbuf.include?("\n")
        step = @steps.shift
        return false if step.nil? || step == :timeout
        @rbuf << step.b
      end
      true
    end

    def readuntil(terminator, ignore_eof = false, limit: nil)
      until (idx = @rbuf.index(terminator))
        step = @steps.shift
        if step.nil? || step == :timeout
          raise EOFError, 'end of stream' unless ignore_eof
          return @rbuf.slice!(0, @rbuf.bytesize)
        end
        @rbuf << step.b
      end
      len = idx + terminator.bytesize
      raise Net::ReadLimitExceeded if limit && len > limit
      @rbuf.slice!(0, len)
    end

    def write(*strs)
      strs.each { |s| @written << s.b }
      strs.inject(0) { |sum, s| sum + s.bytesize }
    end

    def remaining_steps
      @steps.dup
    end
  end

  def build_request(method, path, body: nil, body_stream: nil, expect: true, headers: {})
    req = Net::HTTPGenericRequest.new(method, true, true, path,
                                      ({'expect' => '100-continue'}.merge(headers) if expect) || headers)
    if body
      req.body = body
    elsif body_stream
      req.body_stream = body_stream
    end
    req
  end

  def run_request(req, steps, continue_timeout: 0.5)
    sock = ScriptedSocket.new(steps)
    sock.continue_timeout = continue_timeout
    sock.debug_output = @debug = StringIO.new(+'')
    thrown = catch(:response) { req.exec(sock, '1.1', req.path); nil }
    [sock, thrown]
  end

  def test_continue_admits_body_and_reads_source_once
    source = CountingSource.new('hello')
    req = build_request('POST', '/', body_stream: source)
    req.content_length = 5
    sock, thrown = run_request(req, ["HTTP/1.1 100 Continue\r\n\r\n"])

    assert_nil thrown
    admission = req.body_admission
    assert_equal :continue, admission.state
    assert_predicate admission, :admitted?
    assert_predicate admission, :body_sent?
    assert_predicate admission, :body_finished?
    assert_equal 5, admission.body_bytes
    assert_equal 5, source.bytes_read
    assert_operator source.reads, :>, 0
    assert_match(/hello\z/, sock.written)
  end

  def test_informational_response_is_not_100_and_keeps_waiting
    source = CountingSource.new('hello')
    req = build_request('POST', '/', body_stream: source)
    req.content_length = 5
    sock, thrown = run_request(req, [
      "HTTP/1.1 103 Early Hints\r\nLink: </style.css>\r\n\r\n",
      "HTTP/1.1 100 Continue\r\n\r\n",
    ])

    assert_nil thrown
    admission = req.body_admission
    assert_equal :continue, admission.state
    assert_equal 1, admission.informational_responses
    assert_equal 5, admission.body_bytes
    assert_equal 5, source.bytes_read
  end

  def test_informational_response_alone_does_not_admit_before_timeout
    source = CountingSource.new('hello')
    req = build_request('POST', '/', body_stream: source)
    req.content_length = 5
    sock, thrown = run_request(req, [
      "HTTP/1.1 103 Early Hints\r\n\r\n",
      :timeout,
    ])

    assert_nil thrown
    admission = req.body_admission
    assert_equal :timeout, admission.state
    assert_equal 1, admission.informational_responses
    assert_equal 5, source.bytes_read
    assert_match(/hello\z/, sock.written)
  end

  def test_final_response_before_body_leaves_source_untouched
    source = CountingSource.new('hello')
    req = build_request('POST', '/', body_stream: source)
    req.content_length = 5
    sock, thrown = run_request(req, [
      "HTTP/1.1 417 Expectation Failed\r\nContent-Length: 0\r\n\r\n",
    ])

    refute_nil thrown
    assert_equal '417', thrown.code
    admission = req.body_admission
    assert_equal :declined, admission.state
    refute_predicate admission, :admitted?
    refute_predicate admission, :body_sent?
    assert_predicate admission, :declined?
    assert_equal 0, admission.body_bytes
    assert_equal 0, source.reads
    assert_equal 0, source.bytes_read
    assert_not_match(/hello/, sock.written)
  end

  def test_no_expect_header_sends_without_waiting
    source = CountingSource.new('hello')
    req = build_request('POST', '/', body_stream: source, expect: false)
    req.content_length = 5
    sock, thrown = run_request(req, [])

    assert_nil thrown
    admission = req.body_admission
    assert_equal :not_requested, admission.state
    assert_equal 5, source.bytes_read
    assert_match(/hello\z/, sock.written)
  end

  def test_nil_continue_timeout_sends_without_waiting
    source = CountingSource.new('hello')
    req = build_request('POST', '/', body_stream: source)
    req.content_length = 5
    sock, thrown = run_request(req, [], continue_timeout: nil)

    assert_nil thrown
    admission = req.body_admission
    assert_equal :not_requested, admission.state
    assert_equal 'continue_timeout is not set', admission.reason
    assert_equal 5, source.bytes_read
  end

  def test_response_after_body_uses_actual_sending_facts
    source = CountingSource.new('hello')
    req = build_request('POST', '/', body_stream: source)
    req.content_length = 5
    sock, thrown = run_request(req, [
      "HTTP/1.1 100 Continue\r\n\r\n",
      "HTTP/1.1 413 Payload Too Large\r\nContent-Length: 0\r\n\r\n",
    ])

    # The body was admitted and fully sent, so exec returns normally and the
    # later response is read by the transport loop, not thrown from the wait.
    assert_nil thrown
    admission = req.body_admission
    assert_equal :continue, admission.state
    assert_predicate admission, :body_finished?
    assert_equal 5, admission.body_bytes
    assert_equal 5, source.bytes_read

    # The 413 is still pending for the transport loop, exactly once.
    assert_equal ["HTTP/1.1 413 Payload Too Large\r\nContent-Length: 0\r\n\r\n"],
                 sock.remaining_steps
    res = Net::HTTPResponse.read_new(sock)
    assert_equal '413', res.code
  end

  def test_chunked_stream_reports_sent_bytes_and_reads_once
    source = CountingSource.new('hello')
    req = build_request('POST', '/', body_stream: source,
                        headers: {'transfer-encoding' => 'chunked'})
    sock, thrown = run_request(req, ["HTTP/1.1 100 Continue\r\n\r\n"])

    assert_nil thrown
    admission = req.body_admission
    assert_equal :continue, admission.state
    assert_equal 5, source.bytes_read
    # "5\r\n" + "hello" + "\r\n" + "0\r\n\r\n" is what reached the connection.
    assert_equal 15, admission.body_bytes
    assert_match(/\A5\r\nhello\r\n0\r\n\r\n\z/, sock.written.split("\r\n\r\n", 2).last)
  end

  def test_diagnostics_distinguish_waiting_and_sending
    source = CountingSource.new('hello')
    req = build_request('POST', '/', body_stream: source)
    req.content_length = 5
    run_request(req, ["HTTP/1.1 100 Continue\r\n\r\n"])

    log = @debug.string
    assert_match(/expect-continue: waiting up to .* for 100 Continue/, log)
    assert_match(/expect-continue: received 100 Continue; body admitted/, log)
    assert_match(/expect-continue: sending body stream/, log)
    assert_match(/expect-continue: sent 5 body bytes/, log)
    assert_operator log.index('waiting up to'), :<, log.index('sending body stream')
  end

  def test_declined_upload_logs_waiting_without_sending
    source = CountingSource.new('hello')
    req = build_request('POST', '/', body_stream: source)
    req.content_length = 5
    run_request(req, ["HTTP/1.1 417 Expectation Failed\r\nContent-Length: 0\r\n\r\n"])

    log = @debug.string
    assert_match(/waiting up to/, log)
    assert_match(/upload declined/, log)
    assert_not_match(/sending body stream/, log)
  end

  def test_retriable_body_after_stream_was_sent
    source = CountingSource.new('hello')
    req = build_request('POST', '/', body_stream: source)
    req.content_length = 5
    run_request(req, ["HTTP/1.1 100 Continue\r\n\r\n"])
    refute_predicate req, :retriable_body?
  end

  def test_retriable_body_when_declined_before_reading
    source = CountingSource.new('hello')
    req = build_request('POST', '/', body_stream: source)
    req.content_length = 5
    run_request(req, ["HTTP/1.1 417 Expectation Failed\r\nContent-Length: 0\r\n\r\n"])
    assert_predicate req, :retriable_body?
  end

  def test_retriable_body_for_string_body
    req = build_request('POST', '/', body: 'hello')
    sock = ScriptedSocket.new([])
    catch(:response) { req.exec(sock, '1.1', '/') }
    assert_predicate req, :retriable_body?
  end

  # When writing fails partway through, the admission must report the bytes
  # that actually reached the connection instead of pretending the body was
  # fully sent or not sent at all.
  class FailingSocket < ScriptedSocket
    def initialize(steps, fail_after_writes)
      super(steps)
      @fail_after_writes = fail_after_writes
      @header_written = false
      @body_writes = 0
    end

    def write(*strs)
      unless @header_written
        # The request header is the first thing written.
        @header_written = true
        return super
      end
      @body_writes += 1
      raise Errno::EPIPE, 'simulated write failure' if @body_writes > @fail_after_writes
      super
    end
  end

  def test_partial_send_failure_reports_bytes_written
    source = CountingSource.new('hello world')
    req = build_request('POST', '/', body_stream: source)
    req.content_length = 11
    sock = FailingSocket.new(["HTTP/1.1 100 Continue\r\n\r\n"], 0)
    assert_raise(Errno::EPIPE) do
      catch(:response) { req.exec(sock, '1.1', req.path) }
    end

    admission = req.body_admission
    assert_equal :continue, admission.state
    assert_predicate admission, :body_sent?
    refute_predicate admission, :body_finished?
    # The source was read, but the write failed, so what was sent may be less
    # than the source produced; the record must reflect the connection, not
    # the source.
    assert_operator source.bytes_read, :>=, 0
    assert_operator admission.body_bytes, :<=, source.bytes_read
    # A partly sent stream must not be retried, or the source would be read
    # again and the wrong bytes uploaded.
    refute_predicate req, :retriable_body?
  end

  def test_many_informational_responses_are_bounded
    source = CountingSource.new('hello')
    req = build_request('POST', '/', body_stream: source)
    req.content_length = 5
    limit = Net::HTTPResponse::MAX_INFORMATIONAL_RESPONSES
    steps = Array.new(limit + 1) { "HTTP/1.1 103 Early Hints\r\n\r\n" }
    assert_raise(Net::HTTPBadResponse) do
      run_request(req, steps)
    end
    assert_equal 0, source.reads
  end

  # A minimal loopback server for end-to-end checks. It never contacts an
  # external host: the scripted handler decides what to answer and whether to
  # consume the request body at all.
  class LoopbackServer
    attr_reader :port, :body_received

    def initialize(&handler)
      @server = TCPServer.new('127.0.0.1', 0)
      @port = @server.addr[1]
      @handler = handler
      @thread = Thread.new { serve }
    end

    def serve
      socket = @server.accept
      headers = read_headers(socket)
      @handler.call(socket, headers)
    rescue IOError, Errno::ECONNRESET, Errno::EPIPE
      # The client may hang up before the server answers; that is expected.
    ensure
      begin
        socket&.close
      rescue IOError
      end
      begin
        @server.close
      rescue IOError
      end
    end

    def read_headers(socket)
      headers = {}
      request_line = socket.gets
      return headers unless request_line
      while (line = socket.gets)
        break if line == "\r\n"
        key, value = line.split(':', 2)
        headers[key.downcase] = value.strip
      end
      headers
    end

    def join
      @thread.join(5)
    end
  end

  def test_e2e_final_response_before_body_leaves_source_unread
    source = CountingSource.new('hello')
    received = nil
    server = LoopbackServer.new {|socket, headers|
      assert_equal '100-continue', headers['expect']
      received = headers['content-length']
      socket.write "HTTP/1.1 417 Expectation Failed\r\nContent-Length: 0\r\n\r\n"
      socket.flush
    }
    begin
      http = Net::HTTP.new('127.0.0.1', server.port)
      http.open_timeout = 5
      http.read_timeout = 5
      http.continue_timeout = 5
      req = Net::HTTP::Post.new('/upload')
      req['Expect'] = '100-continue'
      req.content_length = 5
      req.body_stream = source
      res = http.request(req)
      assert_equal '417', res.code
      assert_equal 0, source.reads, 'upload source must not be read'
      assert_equal 0, source.bytes_read
      admission = req.body_admission
      assert_equal :declined, admission.state
      refute_predicate admission, :body_sent?
      assert_equal 0, admission.body_bytes
    ensure
      server.join
    end
    assert_equal '5', received
  end

  def test_e2e_continue_then_body_is_read_once
    source = CountingSource.new('hello')
    server_body = nil
    server = LoopbackServer.new {|socket, headers|
      socket.write "HTTP/1.1 100 Continue\r\n\r\n"
      socket.flush
      server_body = socket.read(headers['content-length'].to_i)
      socket.write "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
      socket.flush
    }
    begin
      http = Net::HTTP.new('127.0.0.1', server.port)
      http.open_timeout = 5
      http.read_timeout = 5
      http.continue_timeout = 5
      req = Net::HTTP::Post.new('/upload')
      req['Expect'] = '100-continue'
      req.content_length = 5
      req.body_stream = source
      res = http.request(req)
      assert_equal '200', res.code
      assert_equal 'ok', res.body
      assert_equal 5, source.bytes_read
      assert_predicate req.body_admission, :body_finished?
      assert_equal 5, req.body_admission.body_bytes
    ensure
      server.join
    end
    assert_equal 'hello', server_body
  end

  def test_e2e_early_hints_then_continue
    source = CountingSource.new('hello')
    server = LoopbackServer.new {|socket, headers|
      # Send both responses in a single write, so the client sees the 100
      # already buffered when it processes the 103.
      socket.write "HTTP/1.1 103 Early Hints\r\nLink: </a.css>\r\n\r\n" \
                   "HTTP/1.1 100 Continue\r\n\r\n"
      socket.flush
      socket.read(headers['content-length'].to_i)
      socket.write "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
      socket.flush
    }
    begin
      http = Net::HTTP.new('127.0.0.1', server.port)
      http.open_timeout = 5
      http.read_timeout = 5
      http.continue_timeout = 5
      req = Net::HTTP::Post.new('/upload')
      req['Expect'] = '100-continue'
      req.content_length = 5
      req.body_stream = source
      res = http.request(req)
      assert_equal '200', res.code
      assert_equal 1, req.body_admission.informational_responses
      assert_equal :continue, req.body_admission.state
      assert_equal 5, source.bytes_read
    ensure
      server.join
    end
  end

  # After a declined upload the connection still expects the announced body
  # bytes, so a follow-up request must not reuse it. The server below models
  # that faithfully: after answering 417 it keeps reading the announced body,
  # so anything else the client sends there is swallowed as body bytes and
  # never answered. Retries are disabled so a bad reuse surfaces directly.
  def test_e2e_declined_upload_does_not_reuse_connection
    connections = []
    mutex = Mutex.new
    server = TCPServer.new('127.0.0.1', 0)
    port = server.addr[1]
    thread = Thread.new do
      loop do
        socket = server.accept
        mutex.synchronize { connections << socket }
        Thread.new(socket) do |s|
          begin
            request_line = s.gets
            next unless request_line
            headers = {}
            while (line = s.gets)
              break if line == "\r\n"
              key, value = line.split(':', 2)
              headers[key.downcase] = value.strip
            end
            if headers['expect'] == '100-continue'
              # No Connection: close, so the only reason the client may close
              # is the fact that the announced body never arrived.
              s.write "HTTP/1.1 417 Expectation Failed\r\nContent-Length: 0\r\n\r\n"
              s.flush
              # Stay in the "reading the request body" state. A correct client
              # closes and reconnects; a wrong one would have its next request
              # consumed here and receive no answer.
              declared = headers['content-length'].to_i
              s.read(declared) if declared > 0
              sleep 5
            else
              s.write "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
              s.flush
            end
          rescue IOError, Errno::ECONNRESET
          ensure
            begin
              s.close
            rescue IOError
            end
          end
        end
      end
    rescue IOError, Errno::ECONNRESET
    end

    begin
      http = Net::HTTP.new('127.0.0.1', port)
      http.open_timeout = 5
      http.read_timeout = 1
      http.continue_timeout = 5
      http.max_retries = 0
      source = CountingSource.new('hello')
      # Start the session explicitly so the request is not given an implicit
      # Connection: close, which would hide the reuse decision.
      http.start do
        req = Net::HTTP::Post.new('/upload')
        req['Expect'] = '100-continue'
        req.content_length = 5
        req.body_stream = source
        first = http.request(req)
        assert_equal '417', first.code
        assert_equal 0, source.reads

        # A second request on the same Net::HTTP object must succeed, which
        # requires a fresh connection rather than the unusable one.
        second = http.get('/status')
        assert_equal '200', second.code
        assert_equal 'ok', second.body
      end
      assert_equal 2, mutex.synchronize { connections.size },
                   'the declined connection must not be reused'
    ensure
      thread.kill
      thread.join(2)
      begin
        server.close
      rescue IOError
      end
    end
  end
end
