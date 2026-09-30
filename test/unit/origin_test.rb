# frozen_string_literal: true

require 'rubygems'
require 'minitest/autorun'
require 'rack/cors'

describe Rack::Cors::Origin do
  describe 'exact host rules' do
    let(:rule) { Rack::Cors::Origin.new('https://example.com') }

    it 'should match the exact origin' do
      _(rule.matches?('https://example.com')).must_equal true
    end

    it 'should match the default port written out explicitly' do
      _(rule.matches?('https://example.com:443')).must_equal true
    end

    it 'should not match a different scheme' do
      _(rule.matches?('http://example.com')).must_equal false
    end

    it 'should not match a non-default port' do
      _(rule.matches?('https://example.com:8443')).must_equal false
    end

    it 'should not match a subdomain' do
      _(rule.matches?('https://sub.example.com')).must_equal false
    end

    it 'should not match a host with a matching prefix' do
      _(rule.matches?('https://evil-example.com')).must_equal false
    end

    it 'should not match a host with a matching suffix' do
      _(rule.matches?('https://example.com.evil.test')).must_equal false
    end

    it 'should match hosts case insensitively' do
      _(rule.matches?('https://EXAMPLE.com')).must_equal true
    end
  end

  describe 'explicit ports' do
    it 'should match only the configured port' do
      rule = Rack::Cors::Origin.new('http://example.com:8080')
      _(rule.matches?('http://example.com:8080')).must_equal true
      _(rule.matches?('http://example.com')).must_equal false
      _(rule.matches?('http://example.com:80')).must_equal false
    end

    it 'should treat an explicit default port like the implicit one' do
      rule = Rack::Cors::Origin.new('http://example.com:80')
      _(rule.matches?('http://example.com')).must_equal true
      _(rule.matches?('http://example.com:80')).must_equal true
    end

    it 'should support IPv6 hosts' do
      rule = Rack::Cors::Origin.new('http://[::1]:3000')
      _(rule.matches?('http://[::1]:3000')).must_equal true
      _(rule.matches?('http://[::1]')).must_equal false
      _(rule.matches?('http://[::2]:3000')).must_equal false
    end
  end

  describe 'subdomain rules' do
    let(:rule) { Rack::Cors::Origin.new('https://*.example.com') }

    it 'should match a direct subdomain' do
      _(rule.matches?('https://sub.example.com')).must_equal true
    end

    it 'should match a nested subdomain' do
      _(rule.matches?('https://a.b.example.com')).must_equal true
    end

    it 'should match the default port written out explicitly' do
      _(rule.matches?('https://sub.example.com:443')).must_equal true
    end

    it 'should not match the root domain' do
      _(rule.matches?('https://example.com')).must_equal false
    end

    it 'should not match a host with a matching prefix' do
      _(rule.matches?('https://evil-example.com')).must_equal false
    end

    it 'should not match a host with a matching suffix' do
      _(rule.matches?('https://example.com.evil.test')).must_equal false
      _(rule.matches?('https://sub.example.com.evil.test')).must_equal false
    end
  end

  describe 'malformed origin values' do
    let(:rule) { Rack::Cors::Origin.new('https://example.com') }

    it 'should not match an origin with a path' do
      _(rule.matches?('https://example.com/path')).must_equal false
    end

    it 'should not match an origin with userinfo' do
      _(rule.matches?('https://user@example.com')).must_equal false
      _(rule.matches?('https://example.com@evil.test')).must_equal false
    end

    it 'should not match an origin with a query or fragment' do
      _(rule.matches?('https://example.com?q=1')).must_equal false
      _(rule.matches?('https://example.com#frag')).must_equal false
    end

    it 'should not match a list of origins' do
      _(rule.matches?('https://example.com https://evil.test')).must_equal false
    end

    it 'should not match a trailing slash' do
      _(rule.matches?('https://example.com/')).must_equal false
    end

    it 'should not match nil or non-string values' do
      _(rule.matches?(nil)).must_equal false
      _(rule.matches?(123)).must_equal false
    end

    it 'should not match a non-numeric port' do
      _(rule.matches?('https://example.com:abc')).must_equal false
    end
  end

  describe 'null origin' do
    let(:rule) { Rack::Cors::Origin.null }

    it 'should match the null origin' do
      _(rule.matches?('null')).must_equal true
    end

    it 'should not match anything else' do
      _(rule.matches?('https://example.com')).must_equal false
      _(rule.matches?('')).must_equal false
    end

    it 'should not be matched by host rules' do
      _(Rack::Cors::Origin.new('https://example.com').matches?('null')).must_equal false
    end
  end

  describe 'invalid rules' do
    it 'should raise on a rule with a path' do
      _(proc { Rack::Cors::Origin.new('https://example.com/path') })
        .must_raise Rack::Cors::Resource::CorsMisconfigurationError
    end

    it 'should raise on a rule with userinfo' do
      _(proc { Rack::Cors::Origin.new('https://user@example.com') })
        .must_raise Rack::Cors::Resource::CorsMisconfigurationError
    end

    it 'should raise on a rule without a host' do
      _(proc { Rack::Cors::Origin.new('https://') })
        .must_raise Rack::Cors::Resource::CorsMisconfigurationError
    end

    it 'should raise on a wildcard-only host' do
      _(proc { Rack::Cors::Origin.new('https://*.') })
        .must_raise Rack::Cors::Resource::CorsMisconfigurationError
    end

    it 'should raise on a rule with a non-numeric port' do
      _(proc { Rack::Cors::Origin.new('https://example.com:abc') })
        .must_raise Rack::Cors::Resource::CorsMisconfigurationError
    end
  end
end
