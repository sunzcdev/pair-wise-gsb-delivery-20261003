# frozen_string_literal: true
require 'helper'

class Test_HTTP_Cookie_SetCookie < Test::Unit::TestCase
  def parse(str)
    HTTP::Cookie::SetCookie.parse(str)
  end

  def test_roundtrip_is_byte_identical
    str = 'session=abc123; Expires=Wed, 21 Oct 2015 07:28:00 GMT; Path=/app; SameSite=Lax; HttpOnly'
    assert_equal str, parse(str).to_s
  end

  def test_roundtrip_preserves_spelling_and_spacing
    str = 'a=1;expires=Wed, 21 Oct 2015 07:28:00 GMT;  PATH =/x ;Secure; X-Custom=foo=bar'
    assert_equal str, parse(str).to_s
  end

  def test_name_and_value
    sc = parse('uid=xyz; Path=/')
    assert_equal 'uid', sc.name
    assert_equal 'xyz', sc.value
  end

  def test_quoted_value_with_semicolons
    str = 'a="v;with;semis"; Path=/x'
    sc = parse(str)
    assert_equal 'a', sc.name
    assert_equal '"v;with;semis"', sc.value
    assert_equal str, sc.to_s
    assert_equal 'v;with;semis', sc.to_cookie('http://example.com/').value
  end

  def test_quoted_value_with_escaped_quote
    str = 'a="v\\";still"; Path=/x'
    sc = parse(str)
    assert_equal str, sc.to_s
    assert_equal '/x', sc.path
  end

  def test_unknown_attributes_are_listed_and_preserved
    sc = parse('a=1; Priority=High; SameSite=None; Secure')
    assert_equal [['Priority', 'High'], ['SameSite', 'None']], sc.unknown_attributes
    sc.path = '/new'
    assert_equal 'a=1; Priority=High; SameSite=None; Secure; Path=/new', sc.to_s
  end

  def test_duplicate_expires_read_last_valid_wins
    sc = parse('a=1; Expires=Wed, 21 Oct 2015 07:28:00 GMT; Expires=Thu, 22 Oct 2015 07:28:00 GMT')
    assert_equal Time.utc(2015, 10, 22, 7, 28, 0), sc.expires
  end

  def test_duplicate_expires_invalid_occurrences_ignored
    valid = 'Wed, 21 Oct 2015 07:28:00 GMT'
    assert_equal Time.utc(2015, 10, 21, 7, 28, 0),
      parse("a=1; Expires=#{valid}; Expires=garbage").expires
    assert_equal Time.utc(2015, 10, 21, 7, 28, 0),
      parse("a=1; Expires=garbage; Expires=#{valid}").expires
  end

  def test_duplicate_expires_write_replaces_first_and_removes_rest
    sc = parse('a=1; Expires=Wed, 21 Oct 2015 07:28:00 GMT; Path=/p; Expires=Thu, 22 Oct 2015 07:28:00 GMT')
    sc.expires = Time.utc(2030, 1, 2, 3, 4, 5)
    assert_equal 'a=1; Expires=Wed, 02 Jan 2030 03:04:05 GMT; Path=/p', sc.to_s
    assert_equal Time.utc(2030, 1, 2, 3, 4, 5), sc.expires
  end

  def test_path_edit_leaves_other_bytes_untouched
    str = 'a=1;expires=Wed, 21 Oct 2015 07:28:00 GMT;  Path=/old ; Secure;X-Weird = q'
    sc = parse(str)
    sc.path = '/new'
    prefix = 'a=1;expires=Wed, 21 Oct 2015 07:28:00 GMT;'
    suffix = '; Secure;X-Weird = q'
    assert_equal 'a=1;expires=Wed, 21 Oct 2015 07:28:00 GMT;  Path=/new; Secure;X-Weird = q', sc.to_s
    assert sc.to_s.start_with?(prefix), 'segments before Path must be byte-identical'
    assert sc.to_s.end_with?(suffix), 'segments after Path must be byte-identical'
  end

  def test_flags
    sc = parse('a=1; Path=/')
    assert_equal false, sc.secure?
    assert_equal false, sc.httponly?
    sc.secure = true
    sc.httponly = true
    assert_equal 'a=1; Path=/; Secure; HttpOnly', sc.to_s
    assert_equal true, sc.secure?
    assert_equal true, sc.httponly?
    sc.secure = false
    assert_equal 'a=1; Path=/; HttpOnly', sc.to_s
  end

  def test_max_age_and_domain
    sc = parse('a=1')
    sc.max_age = 3600
    sc.domain = 'example.com'
    assert_equal 'a=1; Max-Age=3600; Domain=example.com', sc.to_s
    assert_equal 3600, sc.max_age
    assert_equal 'example.com', sc.domain
  end

  def test_nil_removes_attribute
    sc = parse('a=1; Path=/x; Expires=Wed, 21 Oct 2015 07:28:00 GMT')
    sc.expires = nil
    assert_equal 'a=1; Path=/x', sc.to_s
    assert_nil sc.expires
    sc.path = nil
    assert_equal 'a=1', sc.to_s
  end

  def test_crlf_rejected_at_parse_time
    assert_raise(ArgumentError) { parse("a=1; Path=/\r\nInjected: x") }
    assert_raise(ArgumentError) { parse("a=1\n") }
  end

  def test_crlf_rejected_by_writers
    sc = parse('a=1')
    assert_raise(ArgumentError) { sc.path = "/x\r\nEvil: 1" }
    assert_raise(ArgumentError) { sc.domain = "example.com\nX: y" }
    assert_raise(ArgumentError) { sc.path = '/x;Injected=1' }
    assert_equal 'a=1', sc.to_s
  end

  def test_invalid_max_age_rejected
    sc = parse('a=1')
    assert_raise(ArgumentError) { sc.max_age = 'ten' }
  end

  def test_semantic_view
    sc = parse('a=1; Expires=Mon, 21 Oct 2030 07:28:00 GMT; Path=/app; Secure; HttpOnly')
    cookie = sc.to_cookie('http://example.com/app/x')
    assert_kind_of HTTP::Cookie, cookie
    assert_equal 'a', cookie.name
    assert_equal '1', cookie.value
    assert_equal '/app', cookie.path
    assert_equal Time.utc(2030, 10, 21, 7, 28, 0), cookie.expires
    assert_equal true, cookie.secure?
    assert_equal true, cookie.httponly?
  end

  def test_semantic_view_reflects_edits
    sc = parse('a=1; Path=/old')
    sc.path = '/new'
    assert_equal '/new', sc.to_cookie('http://example.com/').path
  end
end
