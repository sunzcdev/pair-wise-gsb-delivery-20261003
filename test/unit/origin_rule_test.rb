# frozen_string_literal: true

require 'minitest/autorun'
require 'rack/cors'

describe Rack::Cors::OriginRule do
  describe '.exact' do
    let(:rule) { Rack::Cors::OriginRule.exact('example.com', schemes: %w[https]) }

    it 'matches the exact host' do
      _(rule.match?('https://example.com')).must_equal true
    end

    it 'matches the host case insensitively' do
      _(rule.match?('https://EXAMPLE.com')).must_equal true
    end

    it 'matches when the default port is written explicitly' do
      _(rule.match?('https://example.com:443')).must_equal true
    end

    it 'does not match a non-default port' do
      _(rule.match?('https://example.com:8443')).must_equal false
    end

    it 'does not match another scheme' do
      _(rule.match?('http://example.com')).must_equal false
    end

    it 'does not match subdomains' do
      _(rule.match?('https://app.example.com')).must_equal false
    end

    it 'does not match look-alike domains' do
      _(rule.match?('https://evil-example.com')).must_equal false
      _(rule.match?('https://example.com.evil')).must_equal false
      _(rule.match?('https://example.com.evil.io')).must_equal false
    end

    it 'does not match the null origin' do
      _(rule.match?('null')).must_equal false
    end
  end

  describe '.subdomains' do
    let(:rule) { Rack::Cors::OriginRule.subdomains('example.com', schemes: %w[https]) }

    it 'matches direct and nested subdomains' do
      _(rule.match?('https://app.example.com')).must_equal true
      _(rule.match?('https://a.b.example.com')).must_equal true
    end

    it 'matches subdomains with the default port written explicitly' do
      _(rule.match?('https://app.example.com:443')).must_equal true
    end

    it 'does not match the root domain' do
      _(rule.match?('https://example.com')).must_equal false
    end

    it 'does not match partial labels or look-alike domains' do
      _(rule.match?('https://evil-example.com')).must_equal false
      _(rule.match?('https://evilexample.com')).must_equal false
      _(rule.match?('https://example.com.evil')).must_equal false
    end

    it 'matches the root domain when include_root is true' do
      rule = Rack::Cors::OriginRule.subdomains('example.com', schemes: %w[https], include_root: true)
      _(rule.match?('https://example.com')).must_equal true
      _(rule.match?('https://app.example.com')).must_equal true
    end
  end

  describe 'ports' do
    it 'matches the default port of each allowed scheme' do
      rule = Rack::Cors::OriginRule.exact('example.com')
      _(rule.match?('http://example.com')).must_equal true
      _(rule.match?('http://example.com:80')).must_equal true
      _(rule.match?('https://example.com')).must_equal true
      _(rule.match?('https://example.com:443')).must_equal true
      _(rule.match?('http://example.com:8080')).must_equal false
      _(rule.match?('https://example.com:80')).must_equal false
    end

    it 'matches custom ports when configured' do
      rule = Rack::Cors::OriginRule.exact('example.com', schemes: %w[https], ports: [8443])
      _(rule.match?('https://example.com:8443')).must_equal true
      _(rule.match?('https://example.com')).must_equal false
      _(rule.match?('https://example.com:443')).must_equal false
    end
  end

  describe 'strict origin parsing' do
    let(:rule) { Rack::Cors::OriginRule.exact('example.com', schemes: %w[https]) }

    it 'does not match an origin with a path' do
      _(rule.match?('https://example.com/path')).must_equal false
      _(rule.match?('https://example.com/')).must_equal false
    end

    it 'does not match an origin with userinfo' do
      _(rule.match?('https://user@example.com')).must_equal false
      _(rule.match?('https://user:secret@example.com')).must_equal false
    end

    it 'does not match an origin with a query or fragment' do
      _(rule.match?('https://example.com?query=1')).must_equal false
      _(rule.match?('https://example.com#fragment')).must_equal false
    end

    it 'does not match multiple origins' do
      _(rule.match?('https://example.com https://evil.com')).must_equal false
    end

    it 'does not match a bare host or invalid values' do
      _(rule.match?('example.com')).must_equal false
      _(rule.match?('')).must_equal false
      _(rule.match?('http://')).must_equal false
      _(rule.match?(nil)).must_equal false
    end
  end

  describe '.null' do
    let(:rule) { Rack::Cors::OriginRule.null }

    it 'matches only the null origin' do
      _(rule.match?('null')).must_equal true
      _(rule.match?('https://example.com')).must_equal false
      _(rule.match?('NULL')).must_equal false
      _(rule.match?('null null')).must_equal false
    end
  end

  describe 'rule host validation' do
    it 'rejects hosts that are not plain host names' do
      _(proc { Rack::Cors::OriginRule.exact('https://example.com') }).must_raise ArgumentError
      _(proc { Rack::Cors::OriginRule.exact('example.com/path') }).must_raise ArgumentError
      _(proc { Rack::Cors::OriginRule.exact('user@example.com') }).must_raise ArgumentError
      _(proc { Rack::Cors::OriginRule.exact('') }).must_raise ArgumentError
    end
  end
end
