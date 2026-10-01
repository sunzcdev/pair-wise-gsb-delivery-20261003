require_relative 'test_helper'
require 'rack/cache/cache_control'
require 'rack/cache/meta_store'

describe Rack::Cache::CacheControl do
  it 'takes no args and initializes with an empty set of values' do
    cache_control = Rack::Cache::CacheControl.new
    assert cache_control.empty?
    cache_control.to_s.must_equal ''
  end

  it 'takes a String and parses it into a Hash when created' do
    cache_control = Rack::Cache::CacheControl.new('max-age=600, foo')
    cache_control['max-age'].must_equal '600'
    cache_control['foo'].must_equal true
  end

  it 'takes a String with a single name=value pair' do
    cache_control = Rack::Cache::CacheControl.new('max-age=600')
    cache_control['max-age'].must_equal '600'
  end

  it 'takes a String with multiple name=value pairs' do
    cache_control = Rack::Cache::CacheControl.new('max-age=600, max-stale=300, min-fresh=570')
    cache_control['max-age'].must_equal '600'
    cache_control['max-stale'].must_equal '300'
    cache_control['min-fresh'].must_equal '570'
  end

  it 'takes a String with a single flag value' do
    cache_control = Rack::Cache::CacheControl.new('no-cache')
    cache_control.must_include 'no-cache'
    cache_control['no-cache'].must_equal true
  end

  it 'takes a String with a bunch of all kinds of stuff' do
    cache_control =
      Rack::Cache::CacheControl.new('max-age=600,must-revalidate,min-fresh=3000,foo=bar,baz')
    cache_control['max-age'].must_equal '600'
    cache_control['must-revalidate'].must_equal true
    cache_control['min-fresh'].must_equal '3000'
    cache_control['foo'].must_equal 'bar'
    cache_control['baz'].must_equal true
  end

  it 'strips leading and trailing spaces from header value' do
    cache_control = Rack::Cache::CacheControl.new('   public,   max-age =   600  ')
    cache_control.must_include 'public'
    cache_control.must_include 'max-age'
    cache_control['max-age'].must_equal '600'
  end

  it 'strips blank segments' do
    cache_control = Rack::Cache::CacheControl.new('max-age=600,,max-stale=300')
    cache_control['max-age'].must_equal '600'
    cache_control['max-stale'].must_equal '300'
  end

  it 'removes all directives with #clear' do
    cache_control = Rack::Cache::CacheControl.new('max-age=600, must-revalidate')
    cache_control.clear
    assert cache_control.empty?
  end

  it 'converts self into header String with #to_s' do
    cache_control = Rack::Cache::CacheControl.new
    cache_control['public'] = true
    cache_control['max-age'] = '600'
    cache_control.to_s.split(', ').sort.must_equal ['max-age=600', 'public']
  end

  it 'sorts alphabetically with boolean directives before value directives' do
    cache_control = Rack::Cache::CacheControl.new('foo=bar, z, x, y, bling=baz, zoom=zib, b, a')
    cache_control.to_s.must_equal 'a, b, x, y, z, bling=baz, foo=bar, zoom=zib'
  end

  it 'responds to #max_age with an integer when max-age directive present' do
    cache_control = Rack::Cache::CacheControl.new('public, max-age=600')
    cache_control.max_age.must_equal 600
  end

  it 'responds to #max_age with nil when no max-age directive present' do
    cache_control = Rack::Cache::CacheControl.new('public')
    cache_control.max_age.must_be_nil
  end

  it 'responds to #shared_max_age with an integer when s-maxage directive present' do
    cache_control = Rack::Cache::CacheControl.new('public, s-maxage=600')
    cache_control.shared_max_age.must_equal 600
  end

  it 'responds to #shared_max_age with nil when no s-maxage directive present' do
    cache_control = Rack::Cache::CacheControl.new('public')
    cache_control.shared_max_age.must_be_nil
  end

  it 'responds to #reverse_max_age with an integer when r-maxage directive present' do
    cache_control = Rack::Cache::CacheControl.new('public, r-maxage=600')
    cache_control.reverse_max_age.must_equal 600
  end

  it 'responds to #reverse_max_age with nil when no r-maxage directive present' do
    cache_control = Rack::Cache::CacheControl.new('public')
    cache_control.reverse_max_age.must_be_nil
  end

  it 'responds to #public? truthfully when public directive present' do
    cache_control = Rack::Cache::CacheControl.new('public')
    assert cache_control.public?
  end

  it 'responds to #public? non-truthfully when no public directive present' do
    cache_control = Rack::Cache::CacheControl.new('private')
    refute cache_control.public?
  end

  it 'responds to #private? truthfully when private directive present' do
    cache_control = Rack::Cache::CacheControl.new('private')
    assert cache_control.private?
  end

  it 'responds to #private? non-truthfully when no private directive present' do
    cache_control = Rack::Cache::CacheControl.new('public')
    refute cache_control.private?
  end

  it 'responds to #no_cache? truthfully when no-cache directive present' do
    cache_control = Rack::Cache::CacheControl.new('no-cache')
    assert cache_control.no_cache?
  end

  it 'responds to #no_cache? non-truthfully when no no-cache directive present' do
    cache_control = Rack::Cache::CacheControl.new('max-age=600')
    refute cache_control.no_cache?
  end

  it 'responds to #must_revalidate? truthfully when must-revalidate directive present' do
    cache_control = Rack::Cache::CacheControl.new('must-revalidate')
    assert cache_control.must_revalidate?
  end

  it 'responds to #must_revalidate? non-truthfully when no must-revalidate directive present' do
    cache_control = Rack::Cache::CacheControl.new('max-age=600')
    refute cache_control.no_cache?
  end

  it 'responds to #proxy_revalidate? truthfully when proxy-revalidate directive present' do
    cache_control = Rack::Cache::CacheControl.new('proxy-revalidate')
    assert cache_control.proxy_revalidate?
  end

  it 'responds to #proxy_revalidate? non-truthfully when no proxy-revalidate directive present' do
    cache_control = Rack::Cache::CacheControl.new('max-age=600')
    refute cache_control.no_cache?
  end

  describe 'field-name lists (quoted-string)' do
    it 'parses a single field name in the private directive' do
      cc = Rack::Cache::CacheControl.new('max-age=100, private="Set-Cookie"')
      cc.private_fields.must_equal ['Set-Cookie']
      assert cc.private?
    end

    it 'parses several field names in the private directive' do
      cc = Rack::Cache::CacheControl.new('private="Set-Cookie, X-Secret ,WWW-Authenticate"')
      cc.private_fields.must_equal ['Set-Cookie', 'X-Secret', 'WWW-Authenticate']
    end

    it 'parses field names in the no-cache directive' do
      cc = Rack::Cache::CacheControl.new('no-cache="set-cookie, location"')
      cc.no_cache_fields.must_equal ['set-cookie', 'location']
      assert cc.no_cache?
    end

    it 'returns :all for a parameterless private directive' do
      Rack::Cache::CacheControl.new('private').private_fields.must_equal :all
    end

    it 'returns :all for a parameterless no-cache directive' do
      Rack::Cache::CacheControl.new('no-cache').no_cache_fields.must_equal :all
    end

    it 'returns nil when the directive is absent' do
      cc = Rack::Cache::CacheControl.new('max-age=100')
      cc.private_fields.must_be_nil
      cc.no_cache_fields.must_be_nil
    end

    it 'conservatively treats an unquoted field list as :all' do
      cc = Rack::Cache::CacheControl.new('private=set-cookie')
      cc.private_fields.must_equal :all
    end

    it 'conservatively treats an unterminated quoted string as :all' do
      cc = Rack::Cache::CacheControl.new('max-age=100, no-cache="set-cookie')
      cc.no_cache_fields.must_equal :all
      cc['max-age'].must_equal '100'
    end

    it 'conservatively treats a malformed field name as :all' do
      cc = Rack::Cache::CacheControl.new('no-cache="set cookie"')
      cc.no_cache_fields.must_equal :all
    end

    it 'conservatively treats an empty field-name list as :all' do
      Rack::Cache::CacheControl.new('private=""').private_fields.must_equal :all
    end

    it 'parses both directives with independent field lists' do
      cc = Rack::Cache::CacheControl.new(
        'private="set-cookie", no-cache="location"')
      cc.private_fields.must_equal ['set-cookie']
      cc.no_cache_fields.must_equal ['location']
    end

    it 'recognizes field-scoped directives independent of directive name case' do
      cc = Rack::Cache::CacheControl.new('PRIVATE="Set-Cookie", NO-CACHE="Location"')
      cc.private_fields.must_equal ['Set-Cookie']
      cc.no_cache_fields.must_equal ['Location']
    end

    it 'still parses other directives around a quoted field list' do
      cc = Rack::Cache::CacheControl.new(
        'public, no-cache="set-cookie", max-age=60, must-revalidate')
      cc['max-age'].must_equal '60'
      assert cc.public?
      assert cc.must_revalidate?
      cc.no_cache_fields.must_equal ['set-cookie']
    end

    it 'does not split field names on commas inside quoted strings' do
      cc = Rack::Cache::CacheControl.new(
        'no-cache="set-cookie, x-one", private="x-two"')
      cc.no_cache_fields.must_equal ['set-cookie', 'x-one']
      cc.private_fields.must_equal ['x-two']
    end

    it 'round trips a field-list directive through #to_s' do
      cc = Rack::Cache::CacheControl.new('max-age=600')
      cc['private'] = '"set-cookie"'
      regenerated = Rack::Cache::CacheControl.new(cc.to_s)
      regenerated.private_fields.must_equal ['set-cookie']
    end
  end
end
