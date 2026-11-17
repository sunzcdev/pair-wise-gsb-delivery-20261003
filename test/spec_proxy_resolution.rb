# frozen_string_literal: true

require_relative 'helper'

separate_testing do
  require_relative '../lib/rack/proxy_resolution'
  require_relative '../lib/rack/request'
  require_relative '../lib/rack/mock_request'
  require_relative '../lib/rack/lint'
  require_relative '../lib/rack/config'
  require_relative '../lib/rack/builder'
end

describe Rack::ProxyResolution do
  def resolve(env_overrides = {}, config: nil)
    env = Rack::MockRequest.env_for("/", env_overrides)
    env[Rack::RACK_REQUEST_CONFIG] = config if config
    Rack::Request.new(env).proxy_resolution
  end

  trust_10 = lambda { |ip| ip == '10.0.0.1' }
  trust_10_and_192 = lambda { |ip| ip == '10.0.0.1' || ip == '192.168.0.1' }

  it "requires an explicit trusted proxy policy and ignores forwarding headers without one" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_X_FORWARDED_FOR' => '1.2.3.4',
      'HTTP_X_FORWARDED_HOST' => 'example.org',
      'HTTP_HOST' => 'direct.example:81',
    })

    result.trusted?.must_equal false
    result.client_address.must_equal '10.0.0.1'
    result.origin.host.must_equal 'direct.example'
    result.origin.port.must_equal 81
    result.discards.map(&:reason).uniq.must_equal [:untrusted_peer]
    result.discards.map(&:header).must_include 'X-Forwarded-For'
    result.discards.map(&:header).must_include 'X-Forwarded-Host'
  end

  it "does not consult the process-global ip_filter default" do
    old_filter = Rack::Request.ip_filter
    Rack::Request.ip_filter = lambda { |ip| true }
    begin
      result = resolve({ 'REMOTE_ADDR' => '10.0.0.1', 'HTTP_X_FORWARDED_FOR' => '1.2.3.4' })
      result.trusted?.must_equal false
      result.client_address.must_equal '10.0.0.1'
    ensure
      Rack::Request.ip_filter = old_filter
    end
  end

  it "ignores forwarding headers from an untrusted peer" do
    result = resolve({
      'REMOTE_ADDR' => '8.8.8.8',
      'HTTP_FORWARDED' => 'for=1.2.3.4;host=example.org;proto=https',
      'HTTP_X_FORWARDED_FOR' => '1.2.3.4',
    }, config: { trusted_proxy: trust_10 })

    result.trusted?.must_equal false
    result.client_address.must_equal '8.8.8.8'
    result.hops.must_equal ['8.8.8.8']
    result.discards.map(&:reason).uniq.must_equal [:untrusted_peer]
  end

  it "adopts the client address by walking the trusted chain hop by hop" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_X_FORWARDED_FOR' => '1.2.3.4, 192.168.0.1, 10.0.0.1',
    }, config: { trusted_proxy: trust_10_and_192 })

    result.trusted?.must_equal true
    result.client_address.must_equal '1.2.3.4'
    result.hops.must_equal ['10.0.0.1', '192.168.0.1']
    result.discards.must_be_empty
  end

  it "discards client-supplied entries beyond the trusted boundary" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_X_FORWARDED_FOR' => '6.6.6.6, 1.2.3.4, 10.0.0.1',
    }, config: { trusted_proxy: trust_10 })

    result.client_address.must_equal '1.2.3.4'
    discard = result.discards.find { |d| d.reason == :unverified_hops }
    discard.wont_be_nil
    discard.value.must_equal '6.6.6.6'
  end

  it "parses quoted IPv6 addresses with ports in Forwarded per RFC 7239" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_FORWARDED' => 'for="[2001:db8:cafe::17]:47011";proto=https;host="example.org:9292"',
    }, config: { trusted_proxy: trust_10 })

    result.client_address.must_equal '2001:db8:cafe::17'
    result.origin.scheme.must_equal 'https'
    result.origin.host.must_equal 'example.org'
    result.origin.port.must_equal 9292
    result.origin.authority.must_equal 'example.org:9292'
    result.origin.to_s.must_equal 'https://example.org:9292'
  end

  it "parses multi-element Forwarded values hop by hop" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_FORWARDED' => 'for=1.2.3.4, for=192.168.0.1;proto=https, for=10.0.0.1',
    }, config: { trusted_proxy: trust_10_and_192 })

    result.client_address.must_equal '1.2.3.4'
    result.hops.must_equal ['10.0.0.1', '192.168.0.1']
    result.origin.scheme.must_equal 'https'
  end

  it "parses bracketed IPv6 with port in X-Forwarded-For" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_X_FORWARDED_FOR' => '[2001:db8:cafe::17]:47011, 10.0.0.1',
    }, config: { trusted_proxy: trust_10 })

    result.client_address.must_equal '2001:db8:cafe::17'
  end

  it "adopts origin from X-Forwarded-Host and X-Forwarded-Port" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_X_FORWARDED_HOST' => 'example.org',
      'HTTP_X_FORWARDED_PORT' => '8443',
      'HTTP_X_FORWARDED_PROTO' => 'https',
    }, config: { trusted_proxy: trust_10 })

    result.origin.scheme.must_equal 'https'
    result.origin.host.must_equal 'example.org'
    result.origin.port.must_equal 8443
    result.origin.to_s.must_equal 'https://example.org:8443'
  end

  it "adopts bracketed IPv6 X-Forwarded-Host" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_X_FORWARDED_HOST' => '[2001:db8:cafe::17]:47011',
    }, config: { trusted_proxy: trust_10 })

    result.origin.host.must_equal '2001:db8:cafe::17'
    result.origin.port.must_equal 47011
    result.origin.authority.must_equal '[2001:db8:cafe::17]:47011'
  end

  it "discards both header families when Forwarded and X-Forwarded-For conflict" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_FORWARDED' => 'for=1.2.3.4',
      'HTTP_X_FORWARDED_FOR' => '5.6.7.8',
    }, config: { trusted_proxy: trust_10 })

    result.conflict?.must_equal true
    result.client_address.must_equal '10.0.0.1'
    result.discards.map(&:reason).must_equal [:conflicting_headers, :conflicting_headers]
  end

  it "discards both header families when the forwarded schemes conflict" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_FORWARDED' => 'proto=https',
      'HTTP_X_FORWARDED_PROTO' => 'http',
    }, config: { trusted_proxy: trust_10 })

    result.conflict?.must_equal true
    result.origin.scheme.must_equal 'http' # direct connection scheme
  end

  it "discards both header families when the forwarded authorities conflict" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_FORWARDED' => 'host=example.org:9292',
      'HTTP_X_FORWARDED_HOST' => 'other.example',
    }, config: { trusted_proxy: trust_10 })

    result.conflict?.must_equal true
  end

  it "accepts agreeing Forwarded and X-Forwarded-* header families" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_FORWARDED' => 'for=1.2.3.4;host=example.org:9292;proto=https',
      'HTTP_X_FORWARDED_FOR' => '1.2.3.4',
      'HTTP_X_FORWARDED_HOST' => 'example.org',
      'HTTP_X_FORWARDED_PORT' => '9292',
      'HTTP_X_FORWARDED_PROTO' => 'https',
    }, config: { trusted_proxy: trust_10 })

    result.conflict?.must_equal false
    result.client_address.must_equal '1.2.3.4'
    result.origin.scheme.must_equal 'https'
    result.origin.host.must_equal 'example.org'
    result.origin.port.must_equal 9292
    result.discards.must_be_empty
  end

  it "discards an unparsable Forwarded header without affecting interpretation" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_FORWARDED' => 'for="unterminated',
      'HTTP_HOST' => 'direct.example',
    }, config: { trusted_proxy: trust_10 })

    result.client_address.must_equal '10.0.0.1'
    result.origin.host.must_equal 'direct.example'
    result.discards.map(&:reason).must_equal [:invalid_syntax]
  end

  it "rejects disallowed forwarded schemes" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_FORWARDED' => 'proto=gopher',
      'rack.url_scheme' => 'http',
    }, config: { trusted_proxy: trust_10 })

    result.origin.scheme.must_equal 'http'
    result.discards.map(&:reason).must_equal [:disallowed_scheme]
  end

  it "does not perform DNS queries on forwarded hostnames" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_X_FORWARDED_FOR' => 'client.example, 10.0.0.1',
    }, config: { trusted_proxy: trust_10 })

    # The hostname is adopted verbatim, never resolved:
    result.client_address.must_equal 'client.example'
  end

  it "supports boolean trusted proxy policies" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_X_FORWARDED_FOR' => '1.2.3.4',
    }, config: { trusted_proxy: false })

    result.trusted?.must_equal false
    result.client_address.must_equal '10.0.0.1'

    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_X_FORWARDED_FOR' => '1.2.3.4',
    }, config: { trusted_proxy: true })

    result.trusted?.must_equal true
    result.client_address.must_equal '1.2.3.4'
  end

  it "adopts the leftmost entry when every hop is trusted" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_X_FORWARDED_FOR' => '192.168.0.1, 10.0.0.1',
    }, config: { trusted_proxy: trust_10_and_192 })

    result.client_address.must_equal '192.168.0.1'
    result.hops.must_equal ['10.0.0.1']
  end

  it "falls back to direct connection values when no forwarding headers are present" do
    result = resolve({
      'REMOTE_ADDR' => '10.0.0.1',
      'HTTP_HOST' => 'direct.example:81',
      'rack.url_scheme' => 'http',
    }, config: { trusted_proxy: trust_10 })

    result.trusted?.must_equal true
    result.client_address.must_equal '10.0.0.1'
    result.origin.scheme.must_equal 'http'
    result.origin.host.must_equal 'direct.example'
    result.origin.port.must_equal 81
    result.discards.must_be_empty
  end

  it "is configured per application instance via rack.request.config" do
    inner = lambda do |env|
      result = Rack::Request.new(env).proxy_resolution
      [200, { 'content-type' => 'text/plain' }, [result.client_address.to_s]]
    end

    configure = lambda do |trusted_proxy|
      Rack::Config.new(inner) do |env|
        env[Rack::RACK_REQUEST_CONFIG] = { trusted_proxy: trusted_proxy }
      end
    end

    Rack::MockRequest.new(configure.call(trust_10)).get(
      '/', 'REMOTE_ADDR' => '10.0.0.1', 'HTTP_X_FORWARDED_FOR' => '1.2.3.4'
    ).body.must_equal '1.2.3.4'

    # A second instance without the policy is unaffected:
    Rack::MockRequest.new(inner).get(
      '/', 'REMOTE_ADDR' => '10.0.0.1', 'HTTP_X_FORWARDED_FOR' => '1.2.3.4'
    ).body.must_equal '10.0.0.1'
  end
end
