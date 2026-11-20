# frozen_string_literal: false
require 'test/unit'
require 'net/http'
require 'socket'
require 'timeout'
begin
  require 'openssl'
rescue LoadError
  # TLS proxy tests are skipped when OpenSSL is unavailable.
end

# Tests for the explicit CONNECT tunnel entry point Net::HTTP#connect.
#
# A real loopback proxy is used; nothing leaves the machine and no protocol
# is spoken inside the tunnel by Net::HTTP itself.
class TestNetHTTPConnect < Test::Unit::TestCase
  # Exceptions a proxy thread may legitimately see when the client goes away.
  CONNECTION_ERRORS = [
    IOError, Errno::ECONNRESET, Errno::EPIPE, Errno::ECONNABORTED,
    EOFError, IO::WaitReadable, IO::WaitWritable,
  ].tap { |list| list << OpenSSL::SSL::SSLError if defined?(OpenSSL::SSL) }.freeze

  # Minimal one-shot proxy. It accepts a single connection, reads the CONNECT
  # request, writes a scripted reply (headers plus optional tunnel bytes) and
  # then keeps the connection open until the peer closes it.
  class Proxy
    attr_reader :socket, :received

    def initialize(status: nil, headers: {}, body: nil, preface: nil, ssl: false,
                   echo: false, raw_reply: nil)
      @status = status
      @headers = headers
      @body = body
      @preface = preface
      @ssl = ssl
      @echo = echo
      @raw_reply = raw_reply
      @received = +''.b
      @replied = false
      @queue = Queue.new
      @server = TCPServer.new('127.0.0.1', 0)
      @thread = Thread.new { run }
    end

    def port
      @server.addr[1]
    end

    def wait_replied(timeout = 5)
      Timeout.timeout(timeout) { @queue.pop }
    end

    def wait(timeout = 5)
      @thread.join(timeout) or raise 'proxy thread did not finish'
    end

    def request
      @request
    end

    def request_line
      @request && @request.lines.first
    end

    def header(name)
      return nil unless @request
      @request.lines.drop(1).each do |line|
        break if line == "\r\n"
        key, value = line.split(':', 2)
        return value.strip if key && key.downcase == name.downcase
      end
      nil
    end

    private

    def run
      raw = @server.accept
      @socket = @ssl ? accept_tls(raw) : raw
      @request = @socket.gets("\r\n\r\n")
      send_reply
      @replied = true
      @queue << :replied
      # Block until either the caller writes or closes the connection.
      read_loop
    rescue *CONNECTION_ERRORS
      nil
    ensure
      @queue << :failed unless @replied
      @socket.close if @socket && !@socket.closed?
      @server.close unless @server.closed?
    end

    def read_loop
      loop do
        chunk = @socket.readpartial(4096)
        @received << chunk
        @socket.write(chunk) if @echo
      end
    rescue *CONNECTION_ERRORS
      nil
    end

    def send_reply
      return unless @request
      if @raw_reply
        @socket.write(@raw_reply)
        return
      end
      reply = +"HTTP/1.1 #{@status}\r\n"
      @headers.each { |k, v| reply << "#{k}: #{v}\r\n" }
      if @body
        reply << "Content-Length: #{@body.bytesize}\r\n"
      elsif !@headers.key?('Content-Length') && !@headers.key?('Transfer-Encoding')
        reply << "Content-Length: 0\r\n"
      end
      reply << "\r\n"
      reply << @body if @body
      reply << @preface if @preface
      @socket.write(reply)
    end

    def accept_tls(raw)
      context = OpenSSL::SSL::SSLContext.new
      fixture = File.expand_path('../fixtures', __dir__)
      key = File.read(File.join(fixture, 'server.key'))
      context.key =
        begin
          OpenSSL::PKey.read(key)
        rescue OpenSSL::PKey::PKeyError
          OpenSSL::PKey::RSA.new(key)
        end
      context.cert = OpenSSL::X509::Certificate.new(File.read(File.join(fixture, 'server.crt')))
      ssl = OpenSSL::SSL::SSLSocket.new(raw, context)
      ssl.sync_close = true
      begin
        ssl.accept
      rescue *CONNECTION_ERRORS
        ssl.close unless ssl.closed?
        raise
      end
      ssl
    end
  end

  def http_for(proxy_or_port, address: 'target.example', port: 8443, user: nil, pass: nil, proxy_ssl: nil)
    proxy_port = proxy_or_port.respond_to?(:port) ? proxy_or_port.port : proxy_or_port
    Net::HTTP.new(address, port, '127.0.0.1', proxy_port, user, pass, nil, proxy_ssl)
  end

  # Reads exactly +length+ bytes from the tunneled stream, consuming the
  # handed-over prefix first and then whatever follows on the socket.
  # Buffering the prefix keeps the test independent of where the TCP/TLS
  # boundaries fell between the CONNECT reply and the first tunneled bytes.
  class TunnelReader
    def initialize(tunnel)
      @socket = tunnel.socket
      @buffer = +''.b
      @buffer << tunnel.prefix
    end

    def read(length, timeout = 5)
      deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + timeout
      while @buffer.bytesize < length
        chunk = @socket.read_nonblock(length - @buffer.bytesize, exception: false)
        case chunk
        when :wait_readable
          remaining = deadline - Process.clock_gettime(Process::CLOCK_MONOTONIC)
          raise 'timed out reading tunnel' if remaining <= 0
          @socket.to_io.wait_readable(remaining) or raise 'timed out reading tunnel'
        when nil
          raise EOFError, 'tunnel closed before expected bytes arrived'
        else
          @buffer << chunk
        end
      end
      @buffer.slice!(0, length)
    end
  end

  #
  # Successful handover
  #

  def test_returns_tunnel_handle_with_raw_socket
    proxy = Proxy.new(status: '200 Connection Established')
    tunnel = http_for(proxy).connect
    proxy.wait_replied

    assert_instance_of Net::HTTP::Tunnel, tunnel
    assert_kind_of TCPSocket, tunnel.socket
    assert_equal '200', tunnel.response.code
    assert_predicate tunnel.prefix, :empty?
    assert_not_predicate tunnel, :closed?
    assert_not_predicate tunnel.socket, :closed?
  ensure
    tunnel&.close
    proxy&.wait
  end

  def test_connect_request_line_and_host_header
    proxy = Proxy.new(status: '200 Connection Established')
    tunnel = http_for(proxy).connect
    proxy.wait_replied

    assert_equal "CONNECT target.example:8443 HTTP/1.1\r\n", proxy.request_line
    assert_equal 'target.example:8443', proxy.header('Host')
  ensure
    tunnel&.close
    proxy&.wait
  end

  def test_ipv6_authority_is_bracketed
    proxy = Proxy.new(status: '200 Connection Established')
    tunnel = http_for(proxy, address: '2001:db8::1').connect
    proxy.wait_replied

    assert_equal "CONNECT [2001:db8::1]:8443 HTTP/1.1\r\n", proxy.request_line
    assert_equal '2001:db8::1:8443', proxy.header('Host')
  ensure
    tunnel&.close
    proxy&.wait
  end

  def test_ipaddr_is_used_as_authority
    proxy = Proxy.new(status: '200 Connection Established')
    http = http_for(proxy)
    http.ipaddr = '192.0.2.1'
    tunnel = http.connect
    proxy.wait_replied

    assert_equal "CONNECT 192.0.2.1:8443 HTTP/1.1\r\n", proxy.request_line
    assert_equal 'target.example:8443', proxy.header('Host')
  ensure
    tunnel&.close
    proxy&.wait
  end

  def test_proxy_authorization_is_sent
    proxy = Proxy.new(status: '200 Connection Established')
    tunnel = http_for(proxy, user: 'user', pass: 'password').connect
    proxy.wait_replied

    expected = 'Basic ' + ['user:password'].pack('m0')
    assert_equal expected, proxy.header('Proxy-Authorization')
  ensure
    tunnel&.close
    proxy&.wait
  end

  def test_prefix_is_handed_over_and_removed_from_read_path
    preface = 'PREFETCHED-TUNNEL-BYTES'
    proxy = Proxy.new(status: '200 Connection Established', preface: preface)
    tunnel = http_for(proxy).connect
    proxy.wait_replied
    reader = TunnelReader.new(tunnel)

    proxy.socket.write('after')
    stream = reader.read(preface.bytesize + 'after'.bytesize)

    # The preface is delivered exactly once, at the head of the stream.
    assert_equal preface + 'after', stream
    assert_operator tunnel.prefix.bytesize, :<=, preface.bytesize
    assert_equal preface, stream.byteslice(0, preface.bytesize)
  ensure
    tunnel&.close
    proxy&.wait
  end

  def test_success_reply_is_not_parsed_as_http_body
    # The reply carries a Content-Length, yet those bytes belong to the
    # tunnel: a 2xx CONNECT reply has no body.
    proxy = Proxy.new(status: '200 Connection Established',
                      headers: { 'Content-Length' => '5' },
                      preface: 'HELLO')
    tunnel = http_for(proxy).connect
    proxy.wait_replied
    reader = TunnelReader.new(tunnel)

    proxy.socket.write('WORLD')
    assert_equal 'HELLOWORLD', reader.read('HELLOWORLD'.bytesize)

    # The response exposes no body: those bytes were handed over as tunnel
    # data, never parsed as an HTTP body.
    assert_nil tunnel.response.body
  ensure
    tunnel&.close
    proxy&.wait
  end

  def test_prefix_precedes_and_tunnel_is_bidirectional
    proxy = Proxy.new(status: '200 Connection Established', preface: 'PRE', echo: true)
    tunnel = http_for(proxy).connect
    proxy.wait_replied
    reader = TunnelReader.new(tunnel)

    assert_equal 'PRE', reader.read('PRE'.bytesize)
    tunnel.socket.write('ping')
    assert_equal 'ping', reader.read(4)
  ensure
    tunnel&.close
    proxy&.wait
  end

  def test_no_in_tunnel_protocol_is_started
    proxy = Proxy.new(status: '200 Connection Established')
    tunnel = http_for(proxy).connect
    proxy.wait_replied

    # Net::HTTP must not send anything into the tunnel on its own (no
    # destination TLS handshake, no protocol greeting).
    sleep 0.2
    assert_equal '', proxy.received
    assert_not_predicate tunnel, :closed?
  ensure
    tunnel&.close
    proxy&.wait
  end

  def test_informational_reply_before_success_is_consumed
    proxy = Proxy.new(raw_reply: "HTTP/1.1 100 Continue\r\n\r\n" \
                                "HTTP/1.1 200 Connection Established\r\n\r\n" \
                                "tunneled")
    tunnel = http_for(proxy).connect
    proxy.wait_replied
    reader = TunnelReader.new(tunnel)

    assert_equal '200', tunnel.response.code
    assert_equal 'tunneled', reader.read('tunneled'.bytesize)
  ensure
    tunnel&.close
    proxy&.wait
  end

  #
  # The tunnel is not part of the session nor of the keep-alive pool
  #

  def test_tunnel_is_not_registered_with_the_session
    proxy = Proxy.new(status: '200 Connection Established')
    http = http_for(proxy)
    tunnel = http.connect
    proxy.wait_replied

    assert_not_predicate http, :started?
    assert_nil http.instance_variable_get(:@socket)
    assert_nil http.instance_variable_get(:@last_communicated)
    assert_raise(IOError) { http.finish }
    # A regular request does not reuse the handed-over connection.
    assert_nil http.instance_variable_get(:@socket)
  ensure
    tunnel&.close
    proxy&.wait
  end

  def test_handed_over_socket_is_not_reused_for_later_requests
    proxy = Proxy.new(status: '200 Connection Established', preface: 'tunnel')
    server = TCPServer.new('127.0.0.1', 0)
    port = server.addr[1]
    served = Queue.new
    server_thread = Thread.new do
      sock = server.accept
      request_line = sock.gets("\r\n")
      served << request_line
      sock.gets("\r\n")
      sock.write("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi")
      sock.close
    end

    # The tunnel target is 127.0.0.1:port so that, after the tunnel is handed
    # over, the same object can talk to that server directly.
    http = Net::HTTP.new('127.0.0.1', port, '127.0.0.1', proxy.port)
    tunnel = http.connect
    proxy.wait_replied
    reader = TunnelReader.new(tunnel)
    assert_equal 'tunnel', reader.read('tunnel'.bytesize)

    # Stop using the proxy so a following request goes straight to the server.
    http.proxy_address = nil
    res = http.get('/fresh')
    assert_equal '200', res.code
    assert_equal 'hi', res.body
    assert_equal "GET /fresh HTTP/1.1\r\n", served.pop

    # The tunnel is untouched: still open and still the caller's to close.
    assert_not_predicate tunnel, :closed?
    proxy.socket.write('still-here')
    assert_equal 'still-here', reader.read('still-here'.bytesize)
  ensure
    tunnel&.close
    proxy&.wait
    server&.close unless server&.closed?
    server_thread&.join(5)
  end

  #
  # Non-success replies are still HTTP
  #

  def test_non_success_reply_is_read_as_http_and_raises
    proxy = Proxy.new(status: '407 Proxy Authentication Required',
                      headers: { 'Proxy-Authenticate' => 'Basic realm="x"' },
                      body: 'auth required')
    http = http_for(proxy)
    error = assert_raise(Net::HTTPClientException) { http.connect }
    proxy.wait_replied

    assert_equal '407', error.response.code
    assert_equal 'auth required', error.response.body
    assert_not_predicate http, :started?
    assert_nil http.instance_variable_get(:@socket)
    proxy.wait
    assert_predicate proxy.socket, :closed?
  end

  def test_server_error_with_chunked_body_is_read_before_raising
    proxy = Proxy.new(raw_reply: "HTTP/1.1 502 Bad Gateway\r\nTransfer-Encoding: chunked\r\n\r\n" \
                                "5\r\nboom!\r\n0\r\n\r\n")
    error = assert_raise(Net::HTTPFatalError) { http_for(proxy).connect }
    proxy.wait_replied

    assert_equal '502', error.response.code
    assert_equal 'boom!', error.response.body
    proxy.wait
  end

  def test_server_error_with_close_delimited_body_is_read_before_raising
    server = TCPServer.new('127.0.0.1', 0)
    port = server.addr[1]
    thread = Thread.new do
      sock = server.accept
      sock.gets("\r\n\r\n")
      sock.write "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\nupstream down"
      sock.close
    end
    error = assert_raise(Net::HTTPFatalError) { http_for(port).connect }

    assert_equal '502', error.response.code
    assert_equal 'upstream down', error.response.body
    thread.join(5)
  ensure
    server&.close unless server&.closed?
  end

  #
  # Accurate settlement of errors, cancellation and repeated close
  #

  def test_requires_a_proxy
    http = Net::HTTP.new('target.example', 8443)
    error = assert_raise(Net::HTTPError) { http.connect }
    assert_match(/proxy/i, error.message)
  end

  def test_raises_ioerror_when_session_already_open
    proxy = Proxy.new(status: '200 Connection Established')
    http = http_for(proxy)
    http.start
    begin
      assert_raise(IOError) { http.connect }
    ensure
      http.finish
    end
    proxy.wait
  ensure
    proxy&.wait
  end

  def test_connection_refused_is_settled
    http = Net::HTTP.new('target.example', 8443, '127.0.0.1', 1)
    assert_raise(Errno::ECONNREFUSED) { http.connect }
    assert_not_predicate http, :started?
    assert_nil http.instance_variable_get(:@socket)
  end

  def test_read_timeout_is_settled_and_closes_the_connection
    server = TCPServer.new('127.0.0.1', 0)
    port = server.addr[1]
    accepted = Queue.new
    thread = Thread.new do
      sock = server.accept
      accepted << sock
      sock.gets("\r\n\r\n")
      sleep 1.5
      sock.close
    end
    http = http_for(port)
    http.read_timeout = 0.2
    assert_raise(Net::ReadTimeout) { http.connect }
    assert_not_predicate http, :started?
    assert_nil http.instance_variable_get(:@socket)

    sock = accepted.pop
    # The establishment failure must have closed the connection.
    deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 3
    loop do
      break if sock.closed?
      ready = sock.to_io.wait_readable(0.05)
      if ready
        begin
          break if sock.read_nonblock(1, exception: false).nil?
        rescue IOError, Errno::ECONNRESET
          break
        end
      end
      raise 'connection was not closed after CONNECT read timeout' if
        Process.clock_gettime(Process::CLOCK_MONOTONIC) > deadline
    end
    thread.join(5)
  ensure
    server&.close unless server&.closed?
  end

  def test_cancellation_closes_the_connection
    server = TCPServer.new('127.0.0.1', 0)
    port = server.addr[1]
    thread = Thread.new do
      sock = server.accept
      sock.gets("\r\n\r\n")
      sleep 1.5
      sock.close
    end
    http = http_for(port)
    raised = assert_raise(Timeout::Error) do
      Timeout.timeout(0.2) { http.connect }
    end
    assert_not_nil raised
    assert_not_predicate http, :started?
    assert_nil http.instance_variable_get(:@socket)
    thread.join(5)
  ensure
    server&.close unless server&.closed?
  end

  def test_premature_eof_during_connect_is_settled
    server = TCPServer.new('127.0.0.1', 0)
    port = server.addr[1]
    thread = Thread.new do
      sock = server.accept
      sock.gets("\r\n\r\n")
      sock.close
    end
    http = http_for(port)
    raised = nil
    begin
      http.connect
    rescue StandardError => e
      raised = e
    end
    assert_not_nil raised
    assert_not_predicate http, :started?
    assert_nil http.instance_variable_get(:@socket)
    thread.join(5)
  ensure
    server&.close unless server&.closed?
  end

  def test_crlf_in_address_is_rejected
    proxy = Proxy.new(status: '200 Connection Established')
    http = http_for(proxy, address: "evil.example\r\nX-Injected: yes")
    assert_raise(ArgumentError) { http.connect }
    assert_nil http.instance_variable_get(:@socket)
  ensure
    proxy&.wait
  end

  def test_close_is_idempotent_and_owns_the_socket
    proxy = Proxy.new(status: '200 Connection Established')
    tunnel = http_for(proxy).connect
    proxy.wait_replied
    socket = tunnel.socket

    assert_nothing_raised { tunnel.close }
    assert_predicate tunnel, :closed?
    assert_predicate socket, :closed?
    assert_nothing_raised { tunnel.close }
    assert_nothing_raised { tunnel.close }
    assert_nil tunnel.close
  ensure
    proxy&.wait
  end

  def test_closing_one_tunnel_leaves_another_open
    proxy_a = Proxy.new(status: '200 Connection Established')
    proxy_b = Proxy.new(status: '200 Connection Established')
    tunnel_a = http_for(proxy_a).connect
    tunnel_b = http_for(proxy_b).connect
    proxy_a.wait_replied
    proxy_b.wait_replied

    assert_not_same tunnel_a.socket, tunnel_b.socket
    tunnel_a.close
    assert_predicate tunnel_a, :closed?
    assert_not_predicate tunnel_b, :closed?
    assert_equal '200', tunnel_b.response.code
  ensure
    tunnel_a&.close
    tunnel_b&.close
    proxy_a&.wait
    proxy_b&.wait
  end

  def test_inspect_reports_closure
    proxy = Proxy.new(status: '200 Connection Established')
    tunnel = http_for(proxy).connect
    proxy.wait_replied

    assert_match(/closed=false/, tunnel.inspect)
    tunnel.close
    assert_match(/closed=true/, tunnel.inspect)
  ensure
    tunnel&.close
    proxy&.wait
  end

  if defined?(OpenSSL::SSL)
    def test_tls_proxy_hands_over_the_proxy_tls_socket
      proxy = Proxy.new(status: '200 Connection Established', ssl: true)
      http = http_for(proxy, proxy_ssl: true)
      http.open_timeout = 5
      http.read_timeout = 5
      tunnel = http.connect
      proxy.wait_replied
      reader = TunnelReader.new(tunnel)

      assert_instance_of OpenSSL::SSL::SSLSocket, tunnel.socket
      assert_equal "CONNECT target.example:8443 HTTP/1.1\r\n", proxy.request_line
      proxy.socket.write('inside-tls')
      assert_equal 'inside-tls', reader.read('inside-tls'.bytesize)
    ensure
      tunnel&.close
      proxy&.wait
    end

    def test_tls_proxy_delivers_early_tunnel_bytes
      proxy = Proxy.new(status: '200 Connection Established',
                        preface: 'early-bytes', ssl: true)
      http = http_for(proxy, proxy_ssl: true)
      http.open_timeout = 5
      http.read_timeout = 5
      tunnel = http.connect
      proxy.wait_replied
      reader = TunnelReader.new(tunnel)

      assert_equal 'early-bytes', reader.read('early-bytes'.bytesize)
      proxy.socket.write('later')
      assert_equal 'later', reader.read(5)
    ensure
      tunnel&.close
      proxy&.wait
    end
  end
end

