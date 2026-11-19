# frozen_string_literal: false
require 'net/http'
require 'socket'
require 'stringio'
require 'test/unit'
require 'openssl'

# Tests for proxy credential rotation:
#
# - a new connection / new request uses the newest published snapshot;
# - a CONNECT handshake (including its 407 round-trip) keeps the
#   generation it started with, even when new credentials are published
#   while the handshake is in flight;
# - a failed publication keeps the old snapshot;
# - proxy authentication failures are reported separately from target
#   service failures;
# - Proxy-Authorization is never copied to a request inside a CONNECT
#   tunnel;
# - diagnostics and the debug log expose generation identifiers only.
class ProxyCredentialRotationTest < Test::Unit::TestCase
  def setup
    @servers = []
    @threads = []
  end

  def teardown
    @servers.each do |server|
      begin
        server.close
      rescue IOError, SystemCallError
      end
    end
    @threads.each do |thread|
      thread.kill if thread.alive?
      thread.join(1)
    end
  end

  def basic(user, pass)
    'Basic ' + ["#{user}:#{pass}"].pack('m0')
  end

  # Starts a TCP server whose whole handling flow is written by the block.
  # The block receives the server socket.  Any exception raised by the
  # block is re-raised in the test thread by #join_server.
  def start_server(&block)
    server = TCPServer.new('127.0.0.1', 0)
    @servers << server
    errors = []
    thread = Thread.new do
      Thread.current.report_on_exception = false
      begin
        block.call(server)
      rescue IOError, SystemCallError, OpenSSL::SSL::SSLError => e
        errors << e
      end
    end
    @threads << thread
    [server.local_address.ip_port, thread, errors]
  end

  def join_server(thread, errors)
    flunk 'controlled proxy did not finish in time' unless thread.join(10)
    if errors.any?
      flunk "controlled proxy failed: #{errors.first.class}: #{errors.first.message}"
    end
  end

  def read_request(sock)
    head = sock.gets("\r\n\r\n")
    flunk 'proxy connection closed before a request arrived' if head.nil?
    head
  end

  # Runs a CONNECT handshake against the controlled proxy.  The proxy
  # answers CONNECT 200 and then closes, so the following TLS handshake is
  # expected to fail; only what was written up to that point matters.
  def start_ssl_client(http)
    http.start
  rescue EOFError, IOError, SystemCallError, OpenSSL::SSL::SSLError
  end

  def ssl_proxy_client(store, port, debug_output = nil)
    http = Net::HTTP.new('origin.example', 443, '127.0.0.1', port)
    http.use_ssl = true
    http.verify_mode = OpenSSL::SSL::VERIFY_NONE
    http.proxy_credential_store = store
    http.set_debug_output(debug_output) if debug_output
    http
  end

  # Two handshakes are interleaved by the controlled proxy: handshake 1 is
  # paused between its first CONNECT and the 407 retry while the platform
  # publishes generation 2 and handshake 2 starts.  Each connection must
  # carry exactly one credential identity.
  def test_interleaved_connect_handshakes_keep_their_generation
    store = Net::HTTP::ProxyCredentialStore.new('user1', 'pass1') # generation 1
    assert_equal 1, store.published_generation

    first_407_sent = Queue.new
    resume_handshake_1 = Queue.new
    observed = Queue.new

    port, thread, errors = start_server do |server|
      conn1 = server.accept
      observed << [1, read_request(conn1)]
      conn1.write(
        "HTTP/1.1 407 Proxy Authentication Required\r\n" \
        "Proxy-Authenticate: Basic realm=\"controlled\"\r\n" \
        "Content-Length: 0\r\n\r\n")
      first_407_sent << true
      resume_handshake_1.pop
      observed << [1, read_request(conn1)]
      conn1.write("HTTP/1.1 200 Connection established\r\nContent-Length: 0\r\n\r\n")
      conn1.close

      conn2 = server.accept
      observed << [2, read_request(conn2)]
      conn2.write("HTTP/1.1 200 Connection established\r\nContent-Length: 0\r\n\r\n")
      conn2.close
    end

    http1 = ssl_proxy_client(store, port)
    client1 = Thread.new { start_ssl_client(http1) }

    first_407_sent.pop
    # Rotation happens while handshake 1 sits between its CONNECT and the
    # 407 retry.
    assert_equal 2, store.publish('user2', 'pass2')

    http2 = ssl_proxy_client(store, port)
    client2 = Thread.new { start_ssl_client(http2) }
    resume_handshake_1 << true

    [client1, client2].each { |t| assert t.join(10), 'client handshake did not finish' }
    join_server(thread, errors)

    requests = []
    requests << observed.pop until observed.empty?
    assert_equal [1, 1, 2], requests.map { |conn, _| conn }

    req1a = requests[0][1]
    req1b = requests[1][1]
    req2  = requests[2][1]

    assert_include req1a, "Proxy-Authorization: #{basic('user1', 'pass1')}\r\n"
    assert_include req1b, "Proxy-Authorization: #{basic('user1', 'pass1')}\r\n"
    assert_not_include req1b, basic('user2', 'pass2')
    assert_include req2, "Proxy-Authorization: #{basic('user2', 'pass2')}\r\n"
    assert_not_include req2, basic('user1', 'pass1')

    [req1a, req1b, req2].each do |request|
      assert_equal 1, request.scan(/^Proxy-Authorization:/).size,
                   "exactly one credential identity per CONNECT:\n#{request}"
    end

    # Generation 1 was really used even though generation 2 was already
    # published when that handshake finished.
    assert_equal 2, store.published_generation
    assert_equal 1, http1.proxy_credential_generation
    assert_equal 2, http2.proxy_credential_generation
    assert_equal({published_generation: 2, in_use_generation: 1},
                 http1.proxy_credential_diagnostics)
    assert_equal({published_generation: 2, in_use_generation: 2},
                 http2.proxy_credential_diagnostics)
  end

  def test_plain_proxy_requests_use_the_new_snapshot
    store = Net::HTTP::ProxyCredentialStore.new('user1', 'pass1')
    observed = Queue.new

    port, thread, errors = start_server do |server|
      conn = server.accept
      observed << read_request(conn)
      conn.write("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\nok")
      observed << read_request(conn)
      conn.write("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
      conn.close
    end

    http = Net::HTTP.new('origin.example', 80, '127.0.0.1', port)
    http.proxy_credential_store = store
    http.start
    assert_equal 'ok', http.get('/first').body
    assert_equal 1, http.proxy_credential_generation

    assert_equal 2, store.publish('user2', 'pass2')
    assert_equal 'ok', http.get('/second').body
    assert_equal 2, http.proxy_credential_generation
    http.finish

    join_server(thread, errors)

    first = observed.pop
    second = observed.pop
    assert_include first, "GET http://origin.example/first HTTP/1.1\r\n"
    assert_include first, "Proxy-Authorization: #{basic('user1', 'pass1')}\r\n"
    assert_include second, "GET http://origin.example/second HTTP/1.1\r\n"
    assert_include second, "Proxy-Authorization: #{basic('user2', 'pass2')}\r\n"
    assert_not_include second, basic('user1', 'pass1')
  end

  def test_failed_publication_keeps_the_old_snapshot
    store = Net::HTTP::ProxyCredentialStore.new('user1', 'pass1')
    original = store.current
    assert_equal 1, store.published_generation

    [
      [nil, 'pass'],
      ['', 'pass'],
      ['user:name', 'pass'],
      ['user', nil],
      ['user', 42],
      ["user\r\nX-Injected: 1", 'pass'],
      ['user', "pass\r\nX-Injected: 1"],
    ].each do |user, pass|
      error = assert_raise(Net::HTTP::ProxyCredentialError) { store.publish(user, pass) }
      assert_not_nil error.message
      assert_equal 1, store.published_generation
      assert_same original, store.current
    end

    assert_equal basic('user1', 'pass1'), store.current.authorization
    assert_predicate store.current, :frozen?
    assert_equal 2, store.publish('user2', 'pass2')
    assert_equal basic('user2', 'pass2'), store.current.authorization
  end

  def test_proxy_authentication_failure_is_separate_from_target_failure
    store = Net::HTTP::ProxyCredentialStore.new('user1', 'pass1')
    log = StringIO.new
    connects = Queue.new

    port, thread, errors = start_server do |server|
      conn = server.accept
      2.times do
        connects << read_request(conn)
        conn.write(
          "HTTP/1.1 407 Proxy Authentication Required\r\n" \
          "Proxy-Authenticate: Basic realm=\"controlled\"\r\n" \
          "Content-Length: 0\r\n\r\n")
      end
      conn.close
    end

    http = ssl_proxy_client(store, port, log)
    error = assert_raise(Net::HTTP::ProxyAuthenticationError) { http.start }
    assert_equal 1, error.generation
    assert_equal '407', error.response.code
    assert_kind_of Net::HTTPProxyAuthenticationRequired, error.response
    assert_kind_of Net::HTTPClientException, error
    join_server(thread, errors)

    attempts = []
    attempts << connects.pop until connects.empty?
    assert_equal 2, attempts.size, 'one initial CONNECT and one 407 retry'
    attempts.each do |attempt|
      assert_include attempt, "Proxy-Authorization: #{basic('user1', 'pass1')}\r\n"
    end

    # The failed handshake leaks neither the credentials nor their
    # encoded form; the diagnosis names only the generation.
    assert_include log.string, 'proxy CONNECT with credential generation 1'
    assert_not_include log.string, 'pass1'
    assert_not_include log.string, 'user1'
    assert_not_include log.string, basic('user1', 'pass1')
    assert_include log.string, 'proxy authentication failed (credential generation 1)'
  end

  def test_target_failure_is_not_reported_as_proxy_authentication_failure
    store = Net::HTTP::ProxyCredentialStore.new('user1', 'pass1')

    port, thread, errors = start_server do |server|
      conn = server.accept
      read_request(conn)
      conn.write("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
      conn.close
    end

    http = ssl_proxy_client(store, port)
    error = assert_raise(Net::HTTPFatalError) { http.start }
    assert_not_kind_of Net::HTTP::ProxyAuthenticationError, error
    assert_equal '502', error.response.code
    join_server(thread, errors)
  end

  def test_proxy_authorization_is_not_copied_into_the_tunnel
    store = Net::HTTP::ProxyCredentialStore.new('tunnel-user', 'tunnel-pass')
    tunneled_requests = Queue.new
    connect_requests = Queue.new
    key = OpenSSL::PKey.read(File.read(File.expand_path('../fixtures/server.key', __dir__)))
    cert = OpenSSL::X509::Certificate.new(File.read(File.expand_path('../fixtures/server.crt', __dir__)))

    port, thread, errors = start_server do |server|
      conn = server.accept
      connect_requests << read_request(conn)
      conn.write("HTTP/1.1 200 Connection established\r\nContent-Length: 0\r\n\r\n")

      context = OpenSSL::SSL::SSLContext.new
      context.cert = cert
      context.key = key
      ssl = OpenSSL::SSL::SSLSocket.new(conn, context)
      ssl.sync_close = true
      ssl.accept
      tunneled_requests << read_request(ssl)
      ssl.write("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
      ssl.close
    end

    http = ssl_proxy_client(store, port)
    http.start
    request = Net::HTTP::Get.new('/inside')
    # Even an explicitly added header must not travel inside the tunnel.
    request['Proxy-Authorization'] = basic('leak', 'leak')
    response = http.request(request)
    assert_equal 'ok', response.body
    http.finish
    join_server(thread, errors)

    connect = connect_requests.pop
    assert_include connect, "Proxy-Authorization: #{basic('tunnel-user', 'tunnel-pass')}\r\n"

    inside = tunneled_requests.pop
    assert_include inside, "GET /inside HTTP/1.1\r\n"
    assert_not_include inside, 'Proxy-Authorization'
    assert_not_include inside, basic('tunnel-user', 'tunnel-pass')
    assert_not_include inside, basic('leak', 'leak')
  end

  def test_static_credentials_produce_the_unchanged_connect_request
    observed = Queue.new

    port, thread, errors = start_server do |server|
      conn = server.accept
      observed << read_request(conn)
      conn.write("HTTP/1.1 200 Connection established\r\nContent-Length: 0\r\n\r\n")
      conn.close
    end

    proxy = Net::HTTP.Proxy('127.0.0.1', port, 'user', 'password')
    http = proxy.new('foo.example.org', 8000)
    http.use_ssl = true
    http.verify_mode = OpenSSL::SSL::VERIFY_NONE
    assert_nil http.proxy_credential_store
    assert_equal({published_generation: nil, in_use_generation: nil},
                 http.proxy_credential_diagnostics)
    start_ssl_client(http)
    join_server(thread, errors)

    assert_equal(
      "CONNECT foo.example.org:8000 HTTP/1.1\r\n" \
      "Host: foo.example.org:8000\r\n" \
      "Proxy-Authorization: Basic dXNlcjpwYXNzd29yZA==\r\n" \
      "\r\n",
      observed.pop)
    assert_nil http.proxy_credential_generation
  end

  def test_empty_store_falls_back_to_static_credentials
    observed = Queue.new

    port, thread, errors = start_server do |server|
      conn = server.accept
      observed << read_request(conn)
      conn.write("HTTP/1.1 200 Connection established\r\nContent-Length: 0\r\n\r\n")
      conn.close
    end

    http = Net::HTTP.new('origin.example', 443, '127.0.0.1', port, 'user', 'password')
    http.use_ssl = true
    http.verify_mode = OpenSSL::SSL::VERIFY_NONE
    http.proxy_credential_store = Net::HTTP::ProxyCredentialStore.new
    assert_nil http.proxy_credential_store.published_generation
    start_ssl_client(http)
    join_server(thread, errors)

    connect = observed.pop
    assert_include connect, "Proxy-Authorization: #{basic('user', 'password')}\r\n"
    # Static credentials have no generation of their own.
    assert_nil http.proxy_credential_generation
    assert_equal({published_generation: nil, in_use_generation: nil},
                 http.proxy_credential_diagnostics)
  end

  def test_407_whose_connection_is_closed_is_not_retried
    store = Net::HTTP::ProxyCredentialStore.new('user1', 'pass1')
    observed = Queue.new

    port, thread, errors = start_server do |server|
      conn = server.accept
      observed << read_request(conn)
      conn.write(
        "HTTP/1.1 407 Proxy Authentication Required\r\n" \
        "Proxy-Authenticate: Basic realm=\"controlled\"\r\n" \
        "Connection: close\r\nContent-Length: 0\r\n\r\n")
      conn.close
    end

    http = ssl_proxy_client(store, port)
    error = assert_raise(Net::HTTP::ProxyAuthenticationError) { http.start }
    assert_equal 1, error.generation
    assert_equal '407', error.response.code
    join_server(thread, errors)

    assert_equal 1, observed.size
    # A rejected handshake still reports which generation was tried.
    assert_equal 1, http.proxy_credential_generation
    assert_equal({published_generation: 1, in_use_generation: 1},
                 http.proxy_credential_diagnostics)
  end

  def test_store_without_installation_keeps_static_behaviour
    store = Net::HTTP::ProxyCredentialStore.new('user1', 'pass1')
    # A connection object that never saw the store behaves exactly as
    # before: no generation is reported and no store is visible.
    http = Net::HTTP.new('origin.example', 443, '127.0.0.1', 1)
    http.use_ssl = true
    assert_nil http.proxy_credential_store
    assert_equal({published_generation: nil, in_use_generation: nil},
                 http.proxy_credential_diagnostics)
    assert_equal 1, store.published_generation
  end

  def test_static_credentials_keep_the_legacy_request_behaviour
    # Without a credential store, the static attributes behave exactly as
    # before (including on a connection with no proxy); the rotation
    # machinery must not change that path.
    http = Net::HTTP.new('origin.example', 80, nil)
    http.proxy_user = 'user1'
    http.proxy_pass = 'pass1'
    refute_predicate http, :proxy?

    request = Net::HTTP::Get.new('/direct')
    http.send(:apply_proxy_authentication, request)
    assert_equal basic('user1', 'pass1'), request['Proxy-Authorization']
    assert_nil http.proxy_credential_generation
    assert_equal({published_generation: nil, in_use_generation: nil},
                 http.proxy_credential_diagnostics)
  end
end