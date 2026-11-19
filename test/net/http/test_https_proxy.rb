# frozen_string_literal: false
begin
  require 'net/https'
rescue LoadError
end
require 'test/unit'
require 'socket'
require 'stringio'

return unless defined?(OpenSSL::SSL)

class HTTPSProxyTest < Test::Unit::TestCase
  def test_https_proxy_authentication
    TCPServer.open("127.0.0.1", 0) {|serv|
      _, port, _, _ = serv.addr
      client_thread = Thread.new {
        proxy = Net::HTTP.Proxy("127.0.0.1", port, 'user', 'password')
        http = proxy.new("foo.example.org", 8000)
        http.use_ssl = true
        http.verify_mode = OpenSSL::SSL::VERIFY_NONE
        begin
          http.start
        rescue EOFError
        end
      }
      server_thread = Thread.new {
        sock = serv.accept
        begin
          proxy_request = sock.gets("\r\n\r\n")
          assert_equal(
            "CONNECT foo.example.org:8000 HTTP/1.1\r\n" +
            "Host: foo.example.org:8000\r\n" +
            "Proxy-Authorization: Basic dXNlcjpwYXNzd29yZA==\r\n" +
            "\r\n",
            proxy_request,
            "[ruby-dev:25673]")
        ensure
          sock.close
        end
      }
      assert_join_threads([client_thread, server_thread])
    }
  end


  def test_https_proxy_rejects_crlf_in_address
    TCPServer.open("127.0.0.1", 0) {|serv|
      _, port, _, _ = serv.addr
      client_thread = Thread.new {
        proxy = Net::HTTP.Proxy("127.0.0.1", port)
        http = proxy.new("foo.example.org\r\nX-Injected: 1\r\nbar.example.org", 8000)
        http.use_ssl = true
        http.verify_mode = OpenSSL::SSL::VERIFY_NONE
        assert_raise(ArgumentError) { http.start }
      }
      server_thread = Thread.new {
        sock = serv.accept
        sock.close
      }
      assert_join_threads([client_thread, server_thread])
    }
  end

  def read_fixture(key)
    File.read(File.expand_path("../fixtures/#{key}", __dir__))
  end

  def test_https_proxy_ssl_connection
    TCPServer.open("127.0.0.1", 0) {|tcpserver|
      ctx = OpenSSL::SSL::SSLContext.new
      ctx.key = OpenSSL::PKey.read(read_fixture("server.key"))
      ctx.cert = OpenSSL::X509::Certificate.new(read_fixture("server.crt"))
      serv = OpenSSL::SSL::SSLServer.new(tcpserver, ctx)

      _, port, _, _ = serv.addr
      client_thread = Thread.new {
        proxy = Net::HTTP.Proxy("127.0.0.1", port, 'user', 'password', true)
        http = proxy.new("foo.example.org", 8000)
        http.use_ssl = true
        http.verify_mode = OpenSSL::SSL::VERIFY_NONE
        begin
          http.start
        rescue EOFError
        end
      }
      server_thread = Thread.new {
        sock = serv.accept
        begin
          proxy_request = sock.gets("\r\n\r\n")
          assert_equal(
            "CONNECT foo.example.org:8000 HTTP/1.1\r\n" +
            "Host: foo.example.org:8000\r\n" +
            "Proxy-Authorization: Basic dXNlcjpwYXNzd29yZA==\r\n" +
            "\r\n",
            proxy_request,
            "[ruby-core:96672]")
        ensure
          sock.close
        end
      }
      assert_join_threads([client_thread, server_thread])
    }
  end

  #
  # Layered TLS: an HTTPS proxy carrying an HTTPS target.
  #
  # Everything here uses local sockets and the fixture certificates, so the
  # tests check the observable stage order, per-layer configuration and
  # resource ownership without any external service.
  #

  def test_layered_tls_options_are_separate
    http = Net::HTTP.new('target.example', 443, 'proxy.example', 8443)
    http.use_ssl = true
    http.ca_file = 'target-ca.pem'
    http.cert = :target_client_cert
    http.verify_mode = OpenSSL::SSL::VERIFY_PEER
    http.verify_hostname = true

    http.proxy_use_ssl = true
    http.proxy_ca_file = 'proxy-ca.pem'
    http.proxy_cert = :proxy_client_cert
    http.proxy_ssl_server_name = 'proxy.example'

    assert_true http.proxy_use_ssl?
    assert_equal 'proxy-ca.pem', http.proxy_ca_file
    assert_equal :proxy_client_cert, http.proxy_cert
    assert_equal 'proxy.example', http.proxy_ssl_server_name
    # The proxy options never overwrite the target options, nor the reverse.
    assert_equal 'target-ca.pem', http.ca_file
    assert_equal :target_client_cert, http.cert
    assert_nil http.proxy_verify_mode, 'proxy verify_mode starts unset'
    assert_nil http.proxy_ssl_context, 'no proxy context until a connection is made'
    assert_nil http.last_connect_stage
  end

  def test_layered_tls_uses_each_layer_server_name
    proxy_sni = []
    target_sni = []
    seen = []
    observed = {}
    log = StringIO.new

    with_layered_tls_servers(proxy_sni: proxy_sni, target_sni: target_sni, seen: seen,
                             log: log) do |http|
      response = http.get('/')
      observed[:code] = response.code
      observed[:body] = response.body
      observed[:stage] = http.last_connect_stage
    end

    assert_equal '200', observed[:code]
    assert_equal 'layered', observed[:body]
    assert_equal ['proxy.example'], proxy_sni,
      'the proxy handshake uses only the proxy server name'
    assert_equal ['localhost'], target_sni,
      'the target handshake uses only the target server name'
    assert_nil observed[:stage], 'a successful connection has no failing stage'
    assert_not_include log.string, 's3cr3t',
      'the proxy password never reaches the debug output'
    assert_not_match(/Proxy-Authorization/i, log.string,
      'the CONNECT exchange is not echoed into the debug output')
    assert_match(/proxy CONNECT succeeded/, log.string,
      'the CONNECT stage is still visible as a diagnostic')
    assert_equal 1, seen.size
    assert_not_match(/Proxy-Authorization/i, seen.first,
      'the proxy credentials stay on the CONNECT request')
    assert_match(/\AGET \/ HTTP\/1\.1\r\n/, seen.first)
  end

  def test_layered_tls_keeps_the_tunnel_across_requests
    requests = []
    codes = []
    started = nil

    with_layered_tls_servers(seen: requests) do |http|
      http.start
      begin
        codes << http.get('/').code
        codes << http.get('/again').code
        started = http.started?
      ensure
        http.finish
      end
    end

    assert_equal ['200', '200'], codes
    assert_true started, 'two requests share one open session'
    assert_equal 2, requests.size
    assert_match(/\AGET \/ HTTP\/1\.1\r\n/, requests[0])
    assert_match(/\AGET \/again HTTP\/1\.1\r\n/, requests[1])
    requests.each do |headers|
      assert_not_match(/Proxy-Authorization/i, headers)
    end
  end

  def test_layered_tls_proxy_and_target_verification_are_separate
    # The proxy certificate is trusted only by the proxy store; the target
    # certificate is not trusted at all.  The proxy hop must succeed with its
    # own store and the target hop must then fail, which shows neither layer
    # borrows the other's CA.
    proxy_key, proxy_cert = make_self_signed_cert('proxy.example')
    target_key, target_cert = make_self_signed_cert('localhost')
    proxy_store = OpenSSL::X509::Store.new.tap {|store| store.add_cert(proxy_cert) }
    unrelated_store = OpenSSL::X509::Store.new
    recorded = {}

    target_server = TCPServer.new('127.0.0.1', 0)
    target_port = target_server.addr[1]
    target_ctx = OpenSSL::SSL::SSLContext.new
    target_ctx.key = target_key
    target_ctx.cert = target_cert
    target_thread = Thread.new do
      raw = tls = nil
      begin
        raw = target_server.accept
        tls = OpenSSL::SSL::SSLSocket.new(raw, target_ctx)
        tls.sync_close = true
        tls.accept
      rescue StandardError
      ensure
        close_quietly(tls)
        close_quietly(raw)
      end
    end

    proxy_server = TCPServer.new('127.0.0.1', 0)
    proxy_port = proxy_server.addr[1]
    proxy_ctx = OpenSSL::SSL::SSLContext.new
    proxy_ctx.key = proxy_key
    proxy_ctx.cert = proxy_cert
    proxy_thread = Thread.new do
      tcp = tls = target = nil
      begin
        tcp = proxy_server.accept
        tls = OpenSSL::SSL::SSLSocket.new(tcp, proxy_ctx)
        tls.sync_close = true
        tls.accept
        # Reaching this line means the proxy hop accepted the proxy
        # certificate with the proxy store.
        recorded[:proxy_handshake] = true
        drain_headers(tls)
        tls.write("HTTP/1.1 200 Connection established\r\n\r\n")
        tls.flush
        target = TCPSocket.new('127.0.0.1', target_port)
        relay_both_ways(tls, target)
      rescue StandardError
      ensure
        close_quietly(target)
        close_quietly(tls)
        close_quietly(tcp)
      end
    end

    http = Net::HTTP.new('localhost', 443, '127.0.0.1', proxy_port)
    http.use_ssl = true
    http.cert_store = unrelated_store
    http.proxy_use_ssl = true
    http.proxy_ssl_server_name = 'proxy.example'
    http.proxy_cert_store = proxy_store
    http.proxy_verify_mode = OpenSSL::SSL::VERIFY_PEER
    http.proxy_verify_hostname = true
    http.open_timeout = 5

    begin
      error = assert_raise(OpenSSL::SSL::SSLError) { http.start }
      assert_equal :target_tls, error.stage
      assert_equal :target_tls, http.last_connect_stage
      assert_true recorded[:proxy_handshake],
        'the proxy hop was verified with the proxy store before the target failed'
    ensure
      http.finish if http.started?
      [proxy_thread, target_thread].each {|thread| thread.join(5) }
      proxy_server.close
      target_server.close
    end
  end

  def test_layered_tls_connect_refusal_does_not_start_target_handshake
    proxy_reply = "HTTP/1.1 407 Proxy Authentication Required\r\n" \
                  "Proxy-Authenticate: Basic realm=\"proxy\"\r\n" \
                  "Content-Length: 0\r\n\r\n"
    after_connect = []

    with_scripted_proxy(proxy_reply: proxy_reply, after_connect: after_connect) do |http|
      http.proxy_user = 'user'
      http.proxy_pass = 'wrong'
      error = assert_raise(Net::HTTPClientException) { http.start }
      assert_equal :proxy_connect, error.stage
      assert_equal :proxy_connect, http.last_connect_stage
      assert_equal '407', error.response.code
      assert_false http.started?
    end

    assert_equal [], after_connect,
      'the refused CONNECT response body is never parsed as target TLS'
  end

  def test_layered_tls_proxy_handshake_failure_is_attributed
    with_scripted_proxy(proxy_handshake: :plain_http) do |http|
      error = assert_raise(OpenSSL::SSL::SSLError) { http.start }
      assert_equal :proxy_tls, error.stage
      assert_equal :proxy_tls, http.last_connect_stage
      assert_match(/proxy TLS handshake/, error.message)
      assert_false http.started?
      assert_nil http.instance_variable_get(:@connection_chain),
        'a failed attempt publishes no connection chain'
    end
  end

  def test_layered_tls_target_handshake_failure_is_attributed
    with_scripted_proxy(target_handshake: :reject) do |http|
      error = assert_raise(OpenSSL::SSL::SSLError) { http.start }
      assert_equal :target_tls, error.stage
      assert_equal :target_tls, http.last_connect_stage
      assert_match(/target TLS handshake/, error.message)
      assert_false http.started?
    end
  end

  def test_layered_tls_cleans_only_owned_resources_on_failure
    # The target handshake fails mid-way; an unrelated connection held by
    # another client must survive, and the failed attempt must publish
    # nothing.
    bystander_server = open_tls_echo_server
    bystander = Net::HTTP.new('localhost', bystander_server.addr[1])
    bystander.use_ssl = true
    bystander.verify_mode = OpenSSL::SSL::VERIFY_NONE
    bystander.start
    bystander_socket = bystander.instance_variable_get(:@socket)
    assert_false bystander_socket.closed?

    with_scripted_proxy(target_handshake: :reject) do |http|
      assert_raise(OpenSSL::SSL::SSLError) { http.start }
      assert_false http.started?
      assert_nil http.instance_variable_get(:@socket),
        'a half-built socket is never published as the connection'
      assert_nil http.instance_variable_get(:@connection_chain),
        'a failed attempt publishes no connection chain'
      assert_false bystander_socket.closed?,
        'a failed attempt closes only its own resources'
      assert_equal '200', bystander.get('/').code
    end
  ensure
    begin
      bystander.finish if bystander&.started?
    rescue StandardError
    end
    bystander_server&.close
  end

  def test_layered_tls_tunnel_relay_close_is_idempotent
    relay_class = Net::HTTP.const_get(:TunnelRelay)
    upstream_read, upstream_write = Socket.pair(:INET, :STREAM)
    relay = relay_class.new(upstream_read)
    peer = relay.io

    relay.close
    assert_true relay.closed?
    assert_true peer.closed?, "the relay's own endpoint is released"
    # A second close (for example from do_finish after a failed connect) must
    # neither raise nor touch a socket it does not own.
    assert_nothing_raised { relay.close }
    assert_nothing_raised { relay.close }
    assert_false upstream_read.closed?, 'the relay never closes the upstream'
  ensure
    begin
      relay&.close
    rescue StandardError
    end
    close_quietly(upstream_read)
    close_quietly(upstream_write)
  end

  #
  # Helpers
  #

  # Runs a complete local HTTPS proxy + HTTPS target setup.
  #
  # The target speaks TLS with the server fixture (CN=localhost); the proxy
  # speaks TLS with a freshly made self-signed certificate named
  # "proxy.example", so SNI can be observed separately for each layer.
  #
  # +seen+ receives the request headers the proxy relays to the target, which
  # is how a test checks what actually reached the origin.
  def with_layered_tls_servers(proxy_sni: [], target_sni: [], seen: [], log: StringIO.new)
    server_key = OpenSSL::PKey.read(read_fixture("server.key"))
    server_cert = OpenSSL::X509::Certificate.new(read_fixture("server.crt"))
    ca_cert = OpenSSL::X509::Certificate.new(read_fixture("cacert.pem"))
    store = OpenSSL::X509::Store.new.tap {|s| s.add_cert(ca_cert) }

    proxy_key, proxy_cert = make_self_signed_cert('proxy.example')

    target_ctx = OpenSSL::SSL::SSLContext.new
    target_ctx.key = server_key
    target_ctx.cert = server_cert
    target_ctx.servername_cb = proc {|socket, name| target_sni << name; nil }

    proxy_ctx = OpenSSL::SSL::SSLContext.new
    proxy_ctx.key = proxy_key
    proxy_ctx.cert = proxy_cert
    proxy_ctx.servername_cb = proc {|socket, name| proxy_sni << name; nil }

    target_server = TCPServer.new('127.0.0.1', 0)
    target_port = target_server.addr[1]
    target_thread = Thread.new do
      raw = tls = nil
      begin
        raw = target_server.accept
        tls = OpenSSL::SSL::SSLSocket.new(raw, target_ctx)
        tls.sync_close = true
        tls.accept
        # The target terminates the inner TLS, so it sees the plaintext
        # requests exactly as the origin would.
        loop do
          headers = read_headers(tls)
          break if headers.nil? || headers.empty?
          seen << headers
          tls.write("HTTP/1.1 200 OK\r\nContent-Length: 7\r\n\r\nlayered")
          tls.flush
        end
      rescue StandardError
        # unreachable clients surface the error on their side
      ensure
        close_quietly(tls)
        close_quietly(raw)
      end
    end

    proxy_server = TCPServer.new('127.0.0.1', 0)
    proxy_port = proxy_server.addr[1]
    proxy_thread = Thread.new do
      tcp = tls = target = nil
      begin
        tcp = proxy_server.accept
        tls = OpenSSL::SSL::SSLSocket.new(tcp, proxy_ctx)
        tls.sync_close = true
        tls.accept
        drain_headers(tls)
        tls.write("HTTP/1.1 200 Connection established\r\n\r\n")
        tls.flush
        target = TCPSocket.new('127.0.0.1', target_port)
        relay_both_ways(tls, target)
      rescue StandardError
      ensure
        close_quietly(target)
        close_quietly(tls)
        close_quietly(tcp)
      end
    end

    http = Net::HTTP.new('localhost', 443, '127.0.0.1', proxy_port, 'user', 's3cr3t')
    http.ipaddr = '127.0.0.1'
    http.use_ssl = true
    http.cert_store = store
    http.proxy_use_ssl = true
    http.proxy_ssl_server_name = 'proxy.example'
    http.proxy_verify_mode = OpenSSL::SSL::VERIFY_NONE
    http.open_timeout = 5
    http.read_timeout = 5
    http.set_debug_output(log)

    begin
      yield http
    ensure
      http.finish if http.started?
      [proxy_thread, target_thread].each {|thread| thread.join(5) }
      proxy_server.close
      target_server.close
    end
  end

  # Runs a minimal HTTPS proxy whose behaviour at each layer is scripted.
  #
  # +proxy_reply+ is sent verbatim as the CONNECT response.  Anything the
  # client sends afterwards is recorded in +after_connect+ instead of being
  # answered, which is how a test sees whether a target handshake started.
  # +proxy_handshake: :plain_http+ answers the proxy ClientHello with plain
  # HTTP, so the proxy TLS handshake genuinely fails.
  # +target_handshake: :reject+ answers the relayed ClientHello with plain
  # HTTP inside the tunnel, so the target handshake fails at once.
  def with_scripted_proxy(proxy_reply: nil, after_connect: nil,
                          proxy_handshake: nil, target_handshake: nil,
                          log: StringIO.new)
    proxy_key, proxy_cert = make_self_signed_cert('proxy.example')
    proxy_ctx = OpenSSL::SSL::SSLContext.new
    proxy_ctx.key = proxy_key
    proxy_ctx.cert = proxy_cert

    server = TCPServer.new('127.0.0.1', 0)
    port = server.addr[1]
    thread = Thread.new do
      tcp = tls = nil
      begin
        tcp = server.accept
        if proxy_handshake == :plain_http
          # Read the TLS ClientHello and answer with non-TLS bytes.
          begin
            tcp.readpartial(4096)
          rescue EOFError, IOError, SystemCallError
          end
          tcp.write("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
          tcp.flush
          next
        end
        tls = OpenSSL::SSL::SSLSocket.new(tcp, proxy_ctx)
        tls.sync_close = true
        tls.accept
        drain_headers(tls)

        if proxy_reply
          tls.write(proxy_reply)
          tls.flush
          loop do
            data = tls.read_nonblock(65536, exception: false)
            break if data == :wait_readable or data == :wait_writable or data.nil?
            after_connect << data if after_connect
          end
        else
          tls.write("HTTP/1.1 200 Connection established\r\n\r\n")
          tls.flush
          if target_handshake == :reject
            begin
              tls.readpartial(4096)
            rescue EOFError, IOError, SystemCallError, OpenSSL::SSL::SSLError
            end
            begin
              tls.write("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
              tls.flush
            rescue IOError, SystemCallError, OpenSSL::SSL::SSLError
            end
          end
        end
      rescue StandardError
        # unreachable clients surface the error on their side
      ensure
        close_quietly(tls)
        close_quietly(tcp)
      end
    end

    http = Net::HTTP.new('target.example', 443, '127.0.0.1', port)
    http.use_ssl = true
    http.verify_mode = OpenSSL::SSL::VERIFY_NONE
    http.proxy_use_ssl = true
    http.proxy_verify_mode = OpenSSL::SSL::VERIFY_NONE
    http.proxy_ssl_server_name = 'proxy.example'
    http.open_timeout = 3
    http.read_timeout = 3
    http.set_debug_output(log)

    begin
      yield http
    ensure
      http.finish if http.started?
      thread.join(5)
      server.close
    end
  end

  # A local TLS server that answers every request with "ok"; #close stops it.
  class LocalTLSServer
    attr_reader :port

    def initialize(ctx)
      @server = TCPServer.new('127.0.0.1', 0)
      @port = @server.addr[1]
      @thread = Thread.new do
        begin
          loop do
            tcp = @server.accept
            Thread.new { serve(tcp, ctx) }
          end
        rescue StandardError
          # stopped by #close
        rescue IOError, SystemCallError
          # stopped by #close
        end
      end
      @thread.report_on_exception = false
    end

    def addr
      @server.addr
    end

    def close
      @thread.kill
      @thread.join(1)
      @server.close unless @server.closed?
    rescue IOError, SystemCallError
      # already closed
    end

    private

    def serve(tcp, ctx)
      tls = nil
      tls = OpenSSL::SSL::SSLSocket.new(tcp, ctx)
      tls.sync_close = true
      tls.accept
      loop do
        headers = HTTPSProxyTest.read_headers(tls)
        break if headers.nil? || headers.empty?
        tls.write("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
        tls.flush
      end
    rescue StandardError
      # client hung up
    ensure
      begin
        tls.close if tls and not tls.closed?
      rescue IOError, SystemCallError, OpenSSL::SSL::SSLError
      end
    end
  end

  def self.read_headers(socket)
    headers = +""
    while (line = socket.gets)
      headers << line
      break if line == "\r\n"
    end
    headers
  end

  # Opens a local TLS server that answers every request with "ok".
  def open_tls_echo_server
    server_key, server_cert = make_self_signed_cert('localhost')
    ctx = OpenSSL::SSL::SSLContext.new
    ctx.key = server_key
    ctx.cert = server_cert
    LocalTLSServer.new(ctx)
  end

  def make_self_signed_cert(common_name)
    key = OpenSSL::PKey::RSA.new(2048)
    cert = OpenSSL::X509::Certificate.new
    cert.version = 2
    cert.serial = rand(1 << 20)
    cert.subject = OpenSSL::X509::Name.parse("/CN=#{common_name}")
    cert.issuer = cert.subject
    cert.public_key = key.public_key
    cert.not_before = Time.now - 60
    cert.not_after = Time.now + 3600
    cert.sign(key, OpenSSL::Digest::SHA256.new)
    [key, cert]
  end

  def read_headers(socket)
    headers = +""
    while (line = socket.gets)
      headers << line
      break if line == "\r\n"
    end
    headers
  end

  def drain_headers(socket)
    while (line = socket.gets)
      break if line == "\r\n"
    end
  end

  # Copies bytes both ways between +a+ (TLS) and +b+ (TCP) until both are
  # closed, then lets the caller close them.
  def relay_both_ways(a, b)
    threads = [
      Thread.new { pump_stream(a, b) },
      Thread.new { pump_stream(b, a) },
    ]
    threads.each {|thread| thread.report_on_exception = false }
    threads.each {|thread| thread.join(10) }
  end

  def pump_stream(from, to)
    loop do
      data = from.readpartial(16 * 1024)
      to.write(data)
      to.flush
    end
  rescue EOFError
    begin; to.close_write; rescue IOError, SystemCallError, OpenSSL::SSL::SSLError; end
  rescue IOError, SystemCallError, OpenSSL::SSL::SSLError
  end

  def close_quietly(io)
    io.close if io and not io.closed?
  rescue IOError, SystemCallError, OpenSSL::SSL::SSLError
  end
end
