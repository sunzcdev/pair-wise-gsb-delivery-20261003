# frozen_string_literal: true
require File.expand_path('helper', File.dirname(__FILE__))
require 'yaml'

module TestHTTPCookieJar
  class TestSameSiteAttribute < Test::Unit::TestCase
    def test_samesite_default_is_nil
      assert_nil HTTP::Cookie.new('name', 'value').samesite
    end

    def test_samesite_normalization
      {
        :strict   => :strict,
        'Strict'  => :strict,
        'STRICT'  => :strict,
        :lax      => :lax,
        'Lax'     => :lax,
        :none     => :none,
        'None'    => :none,
        nil       => nil,
      }.each { |value, expected|
        cookie = HTTP::Cookie.new('name', 'value')
        cookie.samesite = value
        assert_equal expected, cookie.samesite, "samesite=#{value.inspect}"
      }
    end

    def test_samesite_invalid_value
      assert_raise(ArgumentError) {
        HTTP::Cookie.new('name', 'value', :samesite => :invalid)
      }
      assert_raise(ArgumentError) {
        HTTP::Cookie.new('name', 'value', :samesite => 'strictest')
      }
      assert_raise(TypeError) {
        HTTP::Cookie.new('name', 'value', :samesite => 1)
      }
    end

    def test_parse_samesite
      cookies = HTTP::Cookie.parse('a=1; SameSite=Lax, b=2; SameSite=None; Secure, c=3',
                                   'https://example.com/')
      assert_equal [:lax, :none, nil], cookies.map(&:samesite)
    end

    def test_set_cookie_value
      cookie = HTTP::Cookie.new('a', '1', :domain => 'example.com',
                                :path => '/', :samesite => :strict)
      assert_include cookie.set_cookie_value, '; SameSite=Strict'
      cookie.samesite = nil
      assert_not_include cookie.set_cookie_value, 'SameSite'
    end

    def test_samesite_persisted_in_yaml
      cookie = HTTP::Cookie.new('a', '1', :domain => 'example.com',
                                :path => '/', :samesite => :lax)
      assert_equal :lax, YAML.unsafe_load(YAML.dump(cookie)).samesite
    end
  end

  class TestSameSiteFiltering < Test::Unit::TestCase
    URL = 'https://shop.example.com/cart'

    def setup
      @jar = HTTP::CookieJar.new
      @jar.parse('strict=1; SameSite=Strict', URL)
      @jar.parse('lax=1; SameSite=Lax', URL)
      @jar.parse('none=1; SameSite=None; Secure', URL)
      @jar.parse('nonsecure_none=1; SameSite=None', URL)
      @jar.parse('legacy=1', URL)
    end

    def sent_names(context)
      @jar.cookies(URL, samesite: context).map(&:name).sort
    end

    def exclusion_reasons(context)
      @jar.samesite_exclusions(URL, samesite: context).map { |cookie, reason|
        [cookie.name, reason]
      }.to_h
    end

    NONE_NOT_SECURE = { 'nonsecure_none' => :samesite_none_not_secure }.freeze
    STRICT_XSITE    = { 'strict' => :samesite_strict_cross_site }.freeze
    LAX_XSITE       = { 'lax' => :samesite_lax_cross_site }.freeze

    # [context, expected sent cookie names, expected exclusion reasons]
    CASES = {
      'direct visit (no initiating site given)' => [
        {},
        %w[lax legacy none strict],
        NONE_NOT_SECURE,
      ],
      'same-site subrequest' => [
        { :site => 'https://shop.example.com/', :top_level => false },
        %w[lax legacy none strict],
        NONE_NOT_SECURE,
      ],
      'same-site across subdomains' => [
        { :site => 'https://www.example.com/' },
        %w[lax legacy none strict],
        NONE_NOT_SECURE,
      ],
      'scheme mismatch is cross-site' => [
        { :site => 'http://shop.example.com/' },
        %w[legacy none],
        NONE_NOT_SECURE.merge(STRICT_XSITE).merge(LAX_XSITE),
      ],
      'cross-site subrequest' => [
        { :site => 'https://ads.example.net/' },
        %w[legacy none],
        NONE_NOT_SECURE.merge(STRICT_XSITE).merge(LAX_XSITE),
      ],
      'cross-site top-level GET navigation' => [
        { :site => 'https://ads.example.net/', :top_level => true, :method => 'GET' },
        %w[lax legacy none],
        NONE_NOT_SECURE.merge(STRICT_XSITE),
      ],
      'cross-site top-level navigation, method case-insensitive' => [
        { :site => 'https://ads.example.net/', :top_level => true, :method => 'head' },
        %w[lax legacy none],
        NONE_NOT_SECURE.merge(STRICT_XSITE),
      ],
      'cross-site top-level POST navigation' => [
        { :site => 'https://ads.example.net/', :top_level => true, :method => 'POST' },
        %w[legacy none],
        NONE_NOT_SECURE.merge(STRICT_XSITE).merge(LAX_XSITE),
      ],
      'cross-site POST subrequest' => [
        { :site => 'https://ads.example.net/', :method => 'POST' },
        %w[legacy none],
        NONE_NOT_SECURE.merge(STRICT_XSITE).merge(LAX_XSITE),
      ],
      'same-site redirect chain' => [
        { :site => 'https://shop.example.com/',
          :redirect_chain => ['https://shop.example.com/step1',
                              'https://www.example.com/step2'] },
        %w[lax legacy none strict],
        NONE_NOT_SECURE,
      ],
      'cross-site redirect back to origin site' => [
        { :site => 'https://shop.example.com/',
          :top_level => true, :method => 'GET',
          :redirect_chain => ['https://ads.example.net/hop'] },
        %w[legacy none],
        NONE_NOT_SECURE.merge(STRICT_XSITE).merge(LAX_XSITE),
      ],
      'cross-site top-level navigation redirected to destination' => [
        { :site => 'https://ads.example.net/',
          :top_level => true, :method => 'GET',
          :redirect_chain => ['https://ads.example.net/jump'] },
        %w[lax legacy none],
        NONE_NOT_SECURE.merge(STRICT_XSITE),
      ],
    }

    data(CASES)
    def test_send_set_and_exclusions((context, expected_names, expected_reasons))
      assert_equal expected_names, sent_names(context)
      assert_equal expected_reasons, exclusion_reasons(context)
    end

    def test_no_context_preserves_legacy_behavior
      assert_equal %w[lax legacy none nonsecure_none strict],
                   @jar.cookies(URL).map(&:name).sort
      assert_equal %w[lax legacy none nonsecure_none strict],
                   @jar.cookies(URL, samesite: nil).map(&:name).sort
    end

    def test_samesite_none_requires_secure_transport
      http_url = 'http://shop.example.com/cart'
      # The SameSite=None cookie has the Secure attribute, so it is
      # not sent over plain HTTP, and that exclusion is not a
      # SameSite exclusion.
      assert_equal %w[lax legacy strict],
                   @jar.cookies(http_url, samesite: {}).map(&:name).sort
      assert_equal NONE_NOT_SECURE, exclusion_reasons_for(http_url)
    end

    def exclusion_reasons_for(url)
      @jar.samesite_exclusions(url, samesite: {}).map { |cookie, reason|
        [cookie.name, reason]
      }.to_h
    end

    def test_each_with_samesite_context
      names = []
      @jar.each(URL, samesite: { :site => 'https://ads.example.net/' }) { |cookie|
        names << cookie.name
      }
      assert_equal %w[legacy none], names.sort
    end

    def test_each_with_samesite_context_object
      context = HTTP::CookieJar::SameSite::Context.new(
        URL, :site => 'https://ads.example.net/', :top_level => true)
      assert_equal %w[lax legacy none],
                   @jar.cookies(URL, samesite: context).map(&:name).sort
    end

    def test_each_with_samesite_requires_uri
      assert_raise(ArgumentError) {
        @jar.each(samesite: {}) { }
      }
    end

    def test_empty_with_samesite_context
      jar = HTTP::CookieJar.new
      jar.parse('strict=1; SameSite=Strict', URL)
      assert_equal false, jar.empty?(URL, samesite: {})
      assert_equal true, jar.empty?(URL, samesite: { :site => 'https://ads.example.net/' })
    end

    def test_invalid_context_type
      assert_raise(TypeError) {
        @jar.cookies(URL, samesite: 'https://shop.example.com/')
      }
    end
  end
end
