# frozen_string_literal: true

require_relative "test_helper"

class ConnectionCoalescingTest < Minitest::Test
  include HTTPX

  # two local origins which, in a real deployment, would resolve to the same reachable IP.
  ORIGIN_A = "https://internal-a.local"
  ORIGIN_B = "https://internal-b.local"

  # pre-recorded peer addresses, so that no DNS resolution nor socket I/O is required.
  SHARED_ADDRESSES = [IPAddr.new("127.0.0.1")].freeze
  OTHER_ADDRESSES = [IPAddr.new("10.0.0.2")].freeze

  def test_request_headers_only_change_does_not_defeat_coalescing
    existing = recorded_connection(ORIGIN_A, Options.new, SHARED_ADDRESSES)
    candidate = recorded_connection(ORIGIN_B, Options.new(headers: { "x-trace-id" => "abc" }), SHARED_ADDRESSES)

    assert existing.mergeable?(candidate),
           "connections differing only by request headers should coalesce"
    assert existing.options.connection_options_match?(candidate.options)
  end

  def test_request_level_options_do_not_defeat_coalescing
    existing = recorded_connection(ORIGIN_A, Options.new, SHARED_ADDRESSES)

    request_scoped_options = [
      { headers: { "x-trace-id" => "abc" } },
      { timeout: { read_timeout: 3, write_timeout: 5 } },
      { base_path: "/v2" },
      { origin: "https://other-origin.local" },
      { decompress_response_body: false },
      { compress_request_body: false },
      { max_response_body_size: 1024 },
      { max_response_headers: 10 },
      { body_threshold_size: 1 },
      { persistent: true },
      { debug: StringIO.new, debug_level: 2 },
    ]

    request_scoped_options.each do |option_overrides|
      candidate = recorded_connection(ORIGIN_B, Options.new(option_overrides), SHARED_ADDRESSES)

      assert existing.mergeable?(candidate),
             "expected coalescing to be preserved for request-level option #{option_overrides.keys.inspect}"
      assert existing.options.connection_options_match?(candidate.options),
             "connection_options_match? should ignore request-level option #{option_overrides.keys.inspect}"
    end
  end

  def test_tls_identity_difference_requires_a_separate_connection
    existing = recorded_connection(ORIGIN_A, Options.new, SHARED_ADDRESSES)

    tls_scoped_options = [
      { ssl: { verify_mode: OpenSSL::SSL::VERIFY_NONE } },
      { ssl: { alpn_protocols: %w[http/1.1] } },
    ]

    tls_scoped_options.each do |option_overrides|
      candidate = recorded_connection(ORIGIN_B, Options.new(option_overrides), SHARED_ADDRESSES)

      refute existing.mergeable?(candidate),
             "a different TLS identity (#{option_overrides.keys.inspect}) must not be coalesced"
      refute existing.options.connection_options_match?(candidate.options)
    end
  end

  def test_routing_difference_requires_a_separate_connection
    existing = recorded_connection(ORIGIN_A, Options.new, SHARED_ADDRESSES)

    # same connection-level options, but the peer the candidate would dial is a different one.
    routed_elsewhere = recorded_connection(ORIGIN_B, Options.new, OTHER_ADDRESSES)

    refute existing.mergeable?(routed_elsewhere),
           "connections pointing to different peer addresses must not coalesce"

    # an explicit routing/transport convention also prevents reuse of an available connection.
    routed_via_options = [
      { transport: "unix" },
      { addresses: OTHER_ADDRESSES.map(&:to_s) },
      { ip_families: [Socket::AF_INET] },
      { resolver_class: :system },
    ]

    routed_via_options.each do |option_overrides|
      candidate = recorded_connection(ORIGIN_B, Options.new(option_overrides), SHARED_ADDRESSES)

      refute existing.mergeable?(candidate),
             "routing option #{option_overrides.keys.inspect} must prevent coalescing"
      refute existing.options.connection_options_match?(candidate.options)
    end
  end

  def test_wire_level_conventions_require_a_separate_connection
    existing = recorded_connection(ORIGIN_A, Options.new, SHARED_ADDRESSES)

    wire_scoped_options = [
      { http2_settings: { max_concurrent_streams: 2 } },
      { fallback_protocol: "h2" },
      { max_requests: 10 },
      { window_size: 4096 },
      { buffer_size: 1024 },
    ]

    wire_scoped_options.each do |option_overrides|
      candidate = recorded_connection(ORIGIN_B, Options.new(option_overrides), SHARED_ADDRESSES)

      refute existing.mergeable?(candidate),
             "wire-level option #{option_overrides.keys.inspect} must prevent coalescing"
      refute existing.options.connection_options_match?(candidate.options)
    end
  end

  def test_match_reuses_available_connection_for_request_level_changes
    existing = recorded_connection(ORIGIN_A, Options.new, SHARED_ADDRESSES)
    request_uri = URI("#{ORIGIN_A}/internal")

    assert existing.match?(request_uri, Options.new(headers: { "x-trace-id" => "abc" })),
           "a new request with extra headers should reuse the available connection"
    assert existing.match?(request_uri, Options.new(timeout: { read_timeout: 3 })),
           "a new request with per-request timeouts should reuse the available connection"
    refute existing.match?(request_uri, Options.new(ssl: { alpn_protocols: %w[http/1.1] })),
           "a request requiring a different TLS identity must not reuse the available connection"
  end

  private

  # builds a recorded connection double: a real HTTPX::Connection whose peer addresses are
  # pre-populated, and which looks open and connected, without performing any DNS lookup or
  # opening an actual socket. This is the object the pool/selector evaluate through
  # Connection#match? and Connection#mergeable? when deciding between coalescing and reconnecting.
  def recorded_connection(origin, options, addresses)
    connection = Connection.new(URI(origin), options)
    connection.addresses = addresses.map { |address| Resolver::Entry.new(address) }
    connection.instance_variable_set(:@state, :open)
    connection.io.instance_variable_set(:@state, :connected)
    connection
  end
end
