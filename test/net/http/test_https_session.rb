# frozen_string_literal: false
require "test/unit"
require_relative "utils"
begin
  require 'net/https'
rescue LoadError
  # should skip this test
end

return unless defined?(OpenSSL::SSL)

class TestNetHTTPSessionReuse < Test::Unit::TestCase
  include TestNetHTTPUtils

  def self.read_fixture(key)
    File.read(File.expand_path("../fixtures/#{key}", __dir__))
  end

  HOST = 'localhost'
  HOST_IP = '127.0.0.1'
  CA_CERT = OpenSSL::X509::Certificate.new(read_fixture("cacert.pem"))
  SERVER_KEY = OpenSSL::PKey.read(read_fixture("server.key"))
  SERVER_CERT = OpenSSL::X509::Certificate.new(read_fixture("server.crt"))
  TEST_STORE = OpenSSL::X509::Store.new.tap {|s| s.add_cert(CA_CERT) }

  CONFIG = {
    'host' => HOST,
    'proxy_host' => nil,
    'proxy_port' => nil,
    'ssl_enable' => true,
    'ssl_certificate' => SERVER_CERT,
    'ssl_private_key' => SERVER_KEY,
  }

  # A recording stand-in for OpenSSL::SSL::SSLSocket so the handshake
  # parameters (offered session, SNI hostname, context) can be asserted on
  # without performing real TLS.
  class FakeSSLSocket
    attr_accessor :sync_close, :hostname, :session
    attr_reader :context, :offered_sessions, :connect_count

    def initialize(io, context)
      @io = io
      @context = context
      @sync_close = false
      @hostname = nil
      @session = FakeSession.new
      @offered_sessions = []
      @connect_count = 0
      @closed = false
    end

    def connect
      connect_nonblock(exception: true)
    end

    def connect_nonblock(exception: true)
      @connect_count += 1
      :connected
    end

    def to_io
      @io
    end

    def session_reused?
      !@offered_sessions.empty?
    end

    def session=(session)
      @session = session
      @offered_sessions << session
    end

    def post_connection_check(hostname); end

    def peer_cert; end

    def ssl_version
      "TLSv1.3"
    end

    def cipher
      ["TLS_AES_256_GCM_SHA384"]
    end

    def close
      @closed = true
    end

    def closed?
      @closed
    end

    def eof?
      true
    end
  end

  class FakeSession
    attr_reader :time, :timeout

    def initialize(timeout: 300)
      @time = Time.now
      @timeout = timeout
    end
  end

  # Replaces the SSL socket factory on one Net::HTTP instance and returns the
  # array into which every created fake socket is recorded.
  def install_fake_ssl_socket(http)
    sockets = []
    http.define_singleton_method(:new_ssl_socket) do |io, context|
      FakeSSLSocket.new(io, context).tap {|sock| sockets << sock }
    end
    sockets
  end

  # Opens one local TCP server on a stable port (the port is part of the
  # session identity) and yields it.  Repeated handshakes against the same
  # server therefore share a cache identity.
  def with_fake_tls_server
    server = TCPServer.open(HOST_IP, 0)
    port = server.addr[1]
    thread = Thread.new do
      loop do
        client = server.accept
        client.close
      end
    rescue IOError
      # server closed during teardown
    end
    yield port
  ensure
    server&.close
    thread&.join
  end

  # Performs one handshake against +port+ using a recording SSL socket and
  # returns the sockets created for that handshake.
  def handshake_once(http, port)
    http.ipaddr = HOST_IP
    http.instance_variable_set(:@port, port)
    sockets = install_fake_ssl_socket(http)
    http.start
    sockets
  ensure
    http.finish if http&.started?
  end

  def build_http(address: HOST, port: 0)
    http = Net::HTTP.new(address, port)
    http.use_ssl = true
    http.verify_mode = OpenSSL::SSL::VERIFY_NONE
    http
  end

  def test_first_handshake_has_no_session_and_reports_no_session_reason
    http = build_http
    with_fake_tls_server do |port|
      sockets = handshake_once(http, port)

      assert_equal 1, sockets.length
      assert_equal [], sockets.first.offered_sessions
      assert_false http.ssl_session_reused?
      assert_equal :no_session, http.ssl_session_invalidation_reason
    end
  end

  def test_second_handshake_offers_cached_session_and_reports_hit
    http = build_http
    with_fake_tls_server do |port|
      sockets = handshake_once(http, port)
      first_session = sockets.first.session

      sockets = handshake_once(http, port)
      assert_equal [first_session], sockets.first.offered_sessions
      assert_true http.ssl_session_reused?
      assert_nil http.ssl_session_invalidation_reason
    end
  end

  def test_changing_verify_mode_invalidates_cached_session
    http = build_http
    with_fake_tls_server do |port|
      handshake_once(http, port)

      http.verify_mode = OpenSSL::SSL::VERIFY_PEER
      sockets = handshake_once(http, port)

      assert_equal [], sockets.first.offered_sessions
      assert_false http.ssl_session_reused?
      assert_equal :identity_changed, http.ssl_session_invalidation_reason
    end
  end

  def test_changing_verify_callback_invalidates_cached_session
    http = build_http
    with_fake_tls_server do |port|
      handshake_once(http, port)

      http.verify_callback = proc {|preverify, _ctx| preverify }
      sockets = handshake_once(http, port)

      assert_equal [], sockets.first.offered_sessions
      assert_equal :identity_changed, http.ssl_session_invalidation_reason
    end
  end

  def test_changing_client_certificate_invalidates_cached_session
    http = build_http
    with_fake_tls_server do |port|
      handshake_once(http, port)

      http.cert = SERVER_CERT
      http.key = SERVER_KEY
      sockets = handshake_once(http, port)

      assert_equal [], sockets.first.offered_sessions
      assert_equal :identity_changed, http.ssl_session_invalidation_reason
    end
  end

  def test_identical_client_certificate_objects_keep_identity
    http = build_http
    with_fake_tls_server do |port|
      handshake_once(http, port)

      http.cert = SERVER_CERT
      http.key = SERVER_KEY
      # first connection with the client cert: identity differs from before
      handshake_once(http, port)
      assert_equal :identity_changed, http.ssl_session_invalidation_reason

      # same objects again: identity is stable, session must be reused
      sockets = handshake_once(http, port)
      assert_true http.ssl_session_reused?
      assert_nil http.ssl_session_invalidation_reason
      assert_equal 1, sockets.first.offered_sessions.length
    end
  end

  def test_session_expired_reports_expired_reason
    http = build_http
    with_fake_tls_server do |port|
      handshake_once(http, port)

      cache = http.instance_variable_get(:@ssl_sessions)
      key = http.instance_variable_get(:@ssl_session_server_key)
      identity = http.instance_variable_get(:@ssl_session_tls_identity)
      cache.store(key, identity, FakeSession.new(timeout: 0))
      sleep 0.01

      sockets = handshake_once(http, port)
      assert_equal [], sockets.first.offered_sessions
      assert_false http.ssl_session_reused?
      assert_equal :session_expired, http.ssl_session_invalidation_reason
    end
  end

  def test_different_origin_does_not_inherit_session
    with_fake_tls_server do |port|
      first_http = build_http
      first_sockets = handshake_once(first_http, port)
      first_session = first_sockets.first.session

      other = build_http(address: "other.example")
      other_sockets = handshake_once(other, port)

      assert_equal [], other_sockets.first.offered_sessions
      assert_equal :no_session, other.ssl_session_invalidation_reason
      assert_same first_session, first_sockets.first.session
    end
  end

  def test_same_object_redirected_to_other_origin_does_not_reuse_session
    http = build_http
    with_fake_tls_server do |port|
      handshake_once(http, port)

      # Simulate following a redirect to a different origin on the same
      # object.  ipaddr keeps the TCP connect local while @address drives
      # SNI and the session cache identity.
      http.instance_variable_set(:@address, "redirected.example")
      sockets = handshake_once(http, port)

      assert_equal [], sockets.first.offered_sessions
      assert_equal :no_session, http.ssl_session_invalidation_reason
      assert_equal "redirected.example", sockets.first.hostname
    end
  end

  def test_proxy_is_part_of_session_identity
    http = build_http
    with_fake_tls_server do |port|
      handshake_once(http, port)

      # A proxy that completes the CONNECT tunnel and keeps the client side
      # open while the (fake) origin handshake runs through it.
      proxy_server = TCPServer.open(HOST_IP, 0)
      proxy_port = proxy_server.addr[1]
      proxy_thread = Thread.new do
        proxy_client = proxy_server.accept
        # read until the end of the CONNECT request headers
        proxy_client.gets("\r\n\r\n")
        proxy_client.write("HTTP/1.1 200 Connection established\r\n\r\n")
        # hold the tunnel open; closed during teardown
        sleep 0.2
      end

      http.instance_variable_set(:@proxy_from_env, false)
      http.instance_variable_set(:@proxy_address, HOST_IP)
      http.instance_variable_set(:@proxy_port, proxy_port)
      http.ipaddr = HOST_IP
      http.instance_variable_set(:@port, port)
      sockets = install_fake_ssl_socket(http)

      http.start
      proxy_thread.join

      assert_equal [], sockets.first.offered_sessions
      assert_equal :no_session, http.ssl_session_invalidation_reason
      assert_equal HOST, sockets.first.hostname
    ensure
      http.finish if http&.started?
      proxy_server&.close
    end
  end

  def test_failed_handshake_does_not_publish_session
    http = build_http
    TCPServer.open(HOST_IP, 0) do |server|
      port = server.addr[1]
      http.ipaddr = HOST_IP
      http.instance_variable_set(:@port, port)

      http.define_singleton_method(:new_ssl_socket) do |io, context|
        FakeSSLSocket.new(io, context).tap do |sock|
          sock.define_singleton_method(:connect_nonblock) do |exception: true|
            raise OpenSSL::SSL::SSLError, "handshake boom"
          end
        end
      end

      thread = Thread.new { server.accept.close }
      assert_raise(OpenSSL::SSL::SSLError) { http.start }
      thread.join

      cache = http.instance_variable_get(:@ssl_sessions)
      assert_empty cache.instance_variable_get(:@sessions)
      assert_false http.ssl_session_reused?
      assert_equal :handshake_failed, http.ssl_session_invalidation_reason
    end
  end

  def test_private_key_content_is_not_in_session_identity
    http = build_http
    http.key = SERVER_KEY
    identity = http.send(:build_ssl_session_identity)
    rendered = identity.inspect

    assert_not_match(/BEGIN/, rendered)
    assert_not_match(Regexp.new(Regexp.escape(SERVER_KEY.to_pem[0, 40])), rendered)
  end

  def test_debug_output_reports_reason_but_never_key_material
    require "stringio"
    http = build_http
    http.key = SERVER_KEY
    log = StringIO.new
    http.set_debug_output(log)

    with_fake_tls_server do |port|
      handshake_once(http, port)
      http.verify_mode = OpenSSL::SSL::VERIFY_PEER
      handshake_once(http, port)
    end

    output = log.string
    assert_match(/TLS session not reused.*identity_changed/, output)
    assert_not_match(/PRIVATE KEY/, output)
    assert_not_match(Regexp.new(Regexp.escape(SERVER_KEY.to_pem[0, 40])), output)
  end

  def test_ssl_socket_factory_records_handshake_parameters
    http = build_http
    with_fake_tls_server do |port|
      sockets = handshake_once(http, port)

      assert_kind_of FakeSSLSocket, sockets.first
      assert_equal HOST, sockets.first.hostname
      assert_kind_of OpenSSL::SSL::SSLContext, sockets.first.context
    end
  end

  def test_real_tls_session_reuse_reports_hit_and_expiry
    http = Net::HTTP.new(HOST, config("port"))
    http.use_ssl = true
    http.cert_store = TEST_STORE
    if OpenSSL::OPENSSL_LIBRARY_VERSION =~ /LibreSSL (\d+\.\d+)/ && $1.to_f > 3.19
      http.ssl_version = :TLSv1_2
    end

    http.start
    assert_false http.ssl_session_reused? unless http.ssl_session_reused?.nil?
    assert_equal :no_session, http.ssl_session_invalidation_reason
    http.get("/")
    http.finish

    http.start
    reused = http.instance_variable_get(:@socket).io.session_reused?
    unless reused.nil? # JRuby cannot detect reuse
      if reused
        assert_true http.ssl_session_reused?
        assert_nil http.ssl_session_invalidation_reason
      else
        assert_false http.ssl_session_reused?
        assert_not_nil http.ssl_session_invalidation_reason
      end
    end
    http.finish
  end
end
