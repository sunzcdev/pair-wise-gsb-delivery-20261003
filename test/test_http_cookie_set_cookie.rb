# frozen_string_literal: true
require File.expand_path('helper', File.dirname(__FILE__))

class TestHTTPCookieSetCookie < Test::Unit::TestCase
  def test_round_trip_preserves_bytes
    raw = 'session=abc123;  PATH=/old ;Expires=Wed, 21 Oct 2015 07:28:00 GMT; X-Custom="a;b"; Secure'
    set_cookie = HTTP::Cookie::SetCookie.new(raw)
    assert_equal raw, set_cookie.to_s
  end

  def test_semantic_readers
    set_cookie = HTTP::Cookie::SetCookie.new(
      'uid=42; Path=/app; Expires=Wed, 21 Oct 2015 07:28:00 GMT; Max-Age=3600; Secure; HttpOnly; X-Ext=1'
    )
    assert_equal 'uid', set_cookie.name
    assert_equal '42', set_cookie.value
    assert_equal '/app', set_cookie.path
    assert_equal Time.utc(2015, 10, 21, 7, 28, 0), set_cookie.expires
    assert_equal 3600, set_cookie.max_age
    assert_true set_cookie.secure?
    assert_true set_cookie.httponly?
    assert_equal '1', set_cookie['x-ext']
    assert_equal %w[path expires max-age secure httponly x-ext], set_cookie.attribute_names
  end

  def test_to_cookie_with_origin
    set_cookie = HTTP::Cookie::SetCookie.new(
      'uid=42; Domain=.example.org; Path=/app; Max-Age=3600; Secure'
    )
    cookie = set_cookie.to_cookie('http://www.example.org/app/page')
    assert_kind_of HTTP::Cookie, cookie
    assert_equal 'uid', cookie.name
    assert_equal '42', cookie.value
    assert_equal 'example.org', cookie.domain
    assert_true cookie.for_domain?
    assert_equal '/app', cookie.path
    assert_equal 3600, cookie.max_age
    assert_true cookie.secure?
  end

  def test_to_cookie_without_origin
    set_cookie = HTTP::Cookie::SetCookie.new('uid=1; Path=/app; Secure')
    cookie = set_cookie.to_cookie
    assert_kind_of HTTP::Cookie, cookie
    assert_equal 'uid', cookie.name
    assert_equal '/app', cookie.path
    assert_true cookie.secure?
  end

  def test_duplicate_expires_first_wins_on_read
    set_cookie = HTTP::Cookie::SetCookie.new(
      'a=b; Expires=Wed, 21 Oct 2015 07:28:00 GMT; expires=Thu, 01 Jan 1970 00:00:00 GMT'
    )
    assert_equal Time.utc(2015, 10, 21, 7, 28, 0), set_cookie.expires
  end

  def test_rewrite_expires_drops_duplicates
    set_cookie = HTTP::Cookie::SetCookie.new(
      'a=b; Expires=Wed, 21 Oct 2015 07:28:00 GMT; Path=/; expires=Thu, 01 Jan 1970 00:00:00 GMT'
    )
    set_cookie.expires = Time.utc(2030, 1, 2, 3, 4, 5)
    assert_equal 'a=b; Expires=Wed, 02 Jan 2030 03:04:05 GMT; Path=/', set_cookie.to_s
  end

  def test_quoted_value_preserved
    raw = 'a="b; c"; Path=/'
    set_cookie = HTTP::Cookie::SetCookie.new(raw)
    assert_equal 'b; c', set_cookie.value
    assert_equal raw, set_cookie.to_s
    set_cookie.path = '/x'
    assert_equal 'a="b; c"; Path=/x', set_cookie.to_s
  end

  def test_unknown_attribute_preserved
    raw = 'a=b; X-Weird = some thing ; Path=/'
    set_cookie = HTTP::Cookie::SetCookie.new(raw)
    assert_equal 'some thing', set_cookie['x-weird']
    set_cookie.path = '/new'
    assert_equal 'a=b; X-Weird = some thing ; Path=/new', set_cookie.to_s
  end

  def test_only_path_bytes_change
    raw = 'session=ABC;  pAtH=/old;Expires=Wed, 21 Oct 2015 07:28:00 GMT;X-Custom="q;u"; Secure;HttpOnly'
    set_cookie = HTTP::Cookie::SetCookie.new(raw)
    set_cookie.path = '/new'
    assert_equal 'session=ABC;  pAtH=/new;Expires=Wed, 21 Oct 2015 07:28:00 GMT;X-Custom="q;u"; Secure;HttpOnly',
                 set_cookie.to_s
  end

  def test_append_missing_attribute
    set_cookie = HTTP::Cookie::SetCookie.new('a=b; Secure')
    set_cookie.path = '/app'
    assert_equal 'a=b; Secure; Path=/app', set_cookie.to_s
    set_cookie.httponly = true
    assert_equal 'a=b; Secure; Path=/app; HttpOnly', set_cookie.to_s
  end

  def test_remove_attribute_with_nil
    set_cookie = HTTP::Cookie::SetCookie.new('a=b; Path=/; Path=/x; Secure')
    set_cookie.path = nil
    assert_equal 'a=b; Secure', set_cookie.to_s
  end

  def test_flag_rewrite
    set_cookie = HTTP::Cookie::SetCookie.new('a=b; Secure; secure')
    set_cookie.secure = true
    assert_equal 'a=b; Secure', set_cookie.to_s
    set_cookie.secure = false
    assert_equal 'a=b', set_cookie.to_s
  end

  def test_max_age_rewrite_drops_duplicates
    set_cookie = HTTP::Cookie::SetCookie.new('a=b; Max-Age=100; max-age=200')
    assert_equal 100, set_cookie.max_age
    set_cookie.max_age = 300
    assert_equal 'a=b; Max-Age=300', set_cookie.to_s
  end

  def test_crlf_rejected
    assert_raise(ArgumentError) {
      HTTP::Cookie::SetCookie.new("a=b; Path=/\r\nX-Bad: 1")
    }
    set_cookie = HTTP::Cookie::SetCookie.new('a=b; Path=/')
    assert_raise(ArgumentError) { set_cookie.path = "/\r\n" }
    assert_raise(ArgumentError) { set_cookie.domain = "example.org\r\n" }
    assert_raise(ArgumentError) { set_cookie.expires = "2030-01-01\r\n" }
    assert_no_match(/[\r\n]/, set_cookie.to_s)
  end

  def test_empty_name_rejected
    assert_raise(ArgumentError) { HTTP::Cookie::SetCookie.new('; Path=/') }
  end
end
