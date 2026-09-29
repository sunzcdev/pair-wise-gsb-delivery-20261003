# frozen_string_literal: true
require File.expand_path('helper', File.dirname(__FILE__))
require 'http/cookie_jar'

class TestHTTPCookieJarSameSite < Test::Unit::TestCase
  TARGET = 'https://www.example.com/'.freeze

  def setup
    @jar = HTTP::CookieJar.new
    @jar.parse('strict=1; SameSite=Strict; Secure', TARGET)
    @jar.parse('lax=1; SameSite=Lax', TARGET)
    @jar.parse('none=1; SameSite=None; Secure', TARGET)
    @jar.parse('none_insecure=1; SameSite=None', TARGET)
    @jar.parse('unset=1', TARGET)
  end

  # Table of [description, context, expected cookie names sent].
  # Cookies in the jar: strict, lax, none (Secure), none_insecure, unset.
  SEND_TABLE = [
    ['direct access (no initiator)',
     {},
     %w[strict lax none unset]],

    ['same-site top-level navigation',
     { initiator: 'https://www.example.com/home', top_level: true, method: 'GET' },
     %w[strict lax none unset]],

    ['same-site subrequest (POST)',
     { initiator: 'https://www.example.com/home', method: 'POST' },
     %w[strict lax none unset]],

    ['same-site subrequest from another host of the site',
     { initiator: 'https://static.example.com/', method: 'POST' },
     %w[strict lax none unset]],

    ['cross-site top-level GET navigation',
     { initiator: 'https://other.com/', top_level: true, method: 'GET' },
     %w[lax none unset]],

    ['cross-site top-level POST navigation',
     { initiator: 'https://other.com/', top_level: true, method: 'POST' },
     %w[none]],

    ['cross-site subrequest (GET)',
     { initiator: 'https://other.com/', method: 'GET' },
     %w[none]],

    ['cross-site subrequest (POST)',
     { initiator: 'https://other.com/', method: 'POST' },
     %w[none]],

    ['cross-site redirect back to the origin site (top-level GET)',
     { initiator: 'https://www.example.com/', top_level: true, method: 'GET',
       redirect_chain: ['https://other.com/hop'] },
     %w[lax none unset]],

    ['cross-site redirect back to the origin site (subrequest POST)',
     { initiator: 'https://www.example.com/', method: 'POST',
       redirect_chain: ['https://other.com/hop'] },
     %w[none]],

    ['same-site redirect chain stays same-site',
     { initiator: 'https://www.example.com/', method: 'POST',
       redirect_chain: ['https://www.example.com/hop1', 'https://static.example.com/hop2'] },
     %w[strict lax none unset]],

    ['scheme downgrade makes the request cross-site',
     { initiator: 'http://www.example.com/', method: 'GET' },
     %w[none]],
  ].freeze

  def test_send_set_table
    SEND_TABLE.each do |description, context, expected|
      context = HTTP::CookieJar::SameSiteContext.new(**context)
      assert_equal expected.sort, @jar.cookies(TARGET, context).map(&:name).sort,
        "send set mismatch: #{description}"
    end
  end

  # Table of [description, context, expected {cookie name => reason}].
  REASON_TABLE = [
    ['cross-site subrequest',
     { initiator: 'https://other.com/', method: 'POST' },
     { 'strict' => :samesite_strict, 'lax' => :samesite_lax,
       'none_insecure' => :samesite_none_secure, 'unset' => :samesite_lax }],

    ['cross-site top-level GET navigation',
     { initiator: 'https://other.com/', top_level: true, method: 'GET' },
     { 'strict' => :samesite_strict, 'none_insecure' => :samesite_none_secure }],

    ['cross-site redirect back to the origin site',
     { initiator: 'https://www.example.com/', top_level: true, method: 'GET',
       redirect_chain: ['https://other.com/hop'] },
     { 'strict' => :samesite_strict, 'none_insecure' => :samesite_none_secure }],
  ].freeze

  def test_exclude_reasons_table
    REASON_TABLE.each do |description, context, expected|
      reasons = {}
      @jar.cookies(TARGET, context, exclude_reasons: reasons)
      actual = reasons.map { |cookie, reason| [cookie.name, reason] }.to_h
      assert_equal expected, actual, "exclusion reasons mismatch: #{description}"
    end
  end

  def test_no_context_keeps_legacy_behavior
    # Without a context every valid cookie is sent, including
    # SameSite=None without Secure.
    assert_equal %w[lax none none_insecure strict unset],
      @jar.cookies(TARGET).map(&:name).sort
  end

  def test_context_accepts_plain_hash
    sent = @jar.cookies(TARGET, { initiator: 'https://other.com/', method: 'POST' })
    assert_equal %w[none], sent.map(&:name)
  end

  def test_context_without_url_is_ignored
    assert_equal @jar.cookies.map(&:name).sort,
      @jar.cookies(nil, { initiator: 'https://other.com/' }).map(&:name).sort
  end

  def test_none_requires_secure_transport
    http_jar = HTTP::CookieJar.new
    http_jar.parse('none=1; SameSite=None; Secure', 'https://www.example.com/')
    # The Secure flag alone keeps it off plain HTTP.
    assert_equal [], http_jar.cookies('http://www.example.com/', {})
  end

  def test_samesite_survives_yaml_roundtrip
    require 'stringio'
    jar = HTTP::CookieJar.new
    jar.parse('a=1; SameSite=None; Secure; Max-Age=3600', TARGET)
    jar.parse('b=2; SameSite=Strict; Max-Age=3600', TARGET)
    jar.parse('c=3; Max-Age=3600', TARGET)

    io = StringIO.new
    jar.save(io)
    loaded = HTTP::CookieJar.new
    loaded.load(StringIO.new(io.string))

    assert_equal({ 'a' => :none, 'b' => :strict, 'c' => nil },
      loaded.cookies.map { |cookie| [cookie.name, cookie.samesite] }.to_h)
  end

  def test_samesite_context_wrap
    context = HTTP::CookieJar::SameSiteContext.new(initiator: 'https://other.com/')
    assert_same context, HTTP::CookieJar::SameSiteContext.wrap(context)
    assert_nil HTTP::CookieJar::SameSiteContext.wrap(nil)
    wrapped = HTTP::CookieJar::SameSiteContext.wrap('initiator' => 'https://other.com/', 'method' => 'post')
    assert_equal 'POST', wrapped.method
    assert_equal URI('https://other.com/'), wrapped.initiator
    assert_raise(TypeError) { HTTP::CookieJar::SameSiteContext.wrap(42) }
  end
end
