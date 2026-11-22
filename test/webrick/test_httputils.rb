# frozen_string_literal: false
require "test/unit"
require "webrick/httputils"

class TestWEBrickHTTPUtils < Test::Unit::TestCase
  include WEBrick::HTTPUtils

  def test_normilize_path
    assert_equal("/foo",       normalize_path("/foo"))
    assert_equal("/foo/bar/",  normalize_path("/foo/bar/"))

    assert_equal("/",          normalize_path("/foo/../"))
    assert_equal("/",          normalize_path("/foo/.."))
    assert_equal("/",          normalize_path("/foo/bar/../../"))
    assert_equal("/",          normalize_path("/foo/bar/../.."))
    assert_equal("/",          normalize_path("/foo/bar/../.."))
    assert_equal("/baz",       normalize_path("/foo/bar/../../baz"))
    assert_equal("/baz",       normalize_path("/foo/../bar/../baz"))
    assert_equal("/baz/",      normalize_path("/foo/../bar/../baz/"))
    assert_equal("/...",       normalize_path("/bar/../..."))
    assert_equal("/.../",      normalize_path("/bar/../.../"))

    assert_equal("/foo/",      normalize_path("/foo/./"))
    assert_equal("/foo/",      normalize_path("/foo/."))
    assert_equal("/foo/",      normalize_path("/foo/././"))
    assert_equal("/foo/",      normalize_path("/foo/./."))
    assert_equal("/foo/bar",   normalize_path("/foo/./bar"))
    assert_equal("/foo/bar/",  normalize_path("/foo/./bar/."))
    assert_equal("/foo/bar/",  normalize_path("/./././foo/./bar/."))

    assert_equal("/foo/bar/",  normalize_path("//foo///.//bar/.///.//"))
    assert_equal("/",          normalize_path("//foo///..///bar/.///..//.//"))

    assert_raise(RuntimeError){ normalize_path("foo/bar") }
    assert_raise(RuntimeError){ normalize_path("..") }
    assert_raise(RuntimeError){ normalize_path("/..") }
    assert_raise(RuntimeError){ normalize_path("/./..") }
    assert_raise(RuntimeError){ normalize_path("/./../") }
    assert_raise(RuntimeError){ normalize_path("/./../..") }
    assert_raise(RuntimeError){ normalize_path("/./../../") }
    assert_raise(RuntimeError){ normalize_path("/./../") }
    assert_raise(RuntimeError){ normalize_path("/../..") }
    assert_raise(RuntimeError){ normalize_path("/../../") }
    assert_raise(RuntimeError){ normalize_path("/../../..") }
    assert_raise(RuntimeError){ normalize_path("/../../../") }
    assert_raise(RuntimeError){ normalize_path("/../foo/../") }
    assert_raise(RuntimeError){ normalize_path("/../foo/../../") }
    assert_raise(RuntimeError){ normalize_path("/foo/bar/../../../../") }
    assert_raise(RuntimeError){ normalize_path("/foo/../bar/../../") }
    assert_raise(RuntimeError){ normalize_path("/./../bar/") }
    assert_raise(RuntimeError){ normalize_path("/./../") }
  end

  def test_split_header_value
    assert_equal(['foo', 'bar'], split_header_value('foo, bar'))
    assert_equal(['"foo"', 'bar'], split_header_value('"foo", bar'))
    assert_equal(['foo', '"bar"'], split_header_value('foo, "bar"'))
    assert_equal(['*'], split_header_value('*'))
    assert_equal(['W/"xyzzy"', 'W/"r2d2xxxx"', 'W/"c3piozzzz"'],
                 split_header_value('W/"xyzzy", W/"r2d2xxxx", W/"c3piozzzz"'))
  end

  def test_escape
    assert_equal("/foo/bar", escape("/foo/bar"))
    assert_equal("/~foo/bar", escape("/~foo/bar"))
    assert_equal("/~foo%20bar", escape("/~foo bar"))
    assert_equal("/~foo%20bar", escape("/~foo bar"))
    assert_equal("/~foo%09bar", escape("/~foo\tbar"))
    assert_equal("/~foo+bar", escape("/~foo+bar"))
    bug8425 = '[Bug #8425] [ruby-core:55052]'
    assert_nothing_raised(ArgumentError, Encoding::CompatibilityError, bug8425) {
      assert_equal("%E3%83%AB%E3%83%93%E3%83%BC%E3%81%95%E3%82%93", escape("\u{30EB 30D3 30FC 3055 3093}"))
    }
  end

  def test_escape_form
    assert_equal("%2Ffoo%2Fbar", escape_form("/foo/bar"))
    assert_equal("%2F~foo%2Fbar", escape_form("/~foo/bar"))
    assert_equal("%2F~foo+bar", escape_form("/~foo bar"))
    assert_equal("%2F~foo+%2B+bar", escape_form("/~foo + bar"))
  end

  def test_unescape
    assert_equal("/foo/bar", unescape("%2ffoo%2fbar"))
    assert_equal("/~foo/bar", unescape("/%7efoo/bar"))
    assert_equal("/~foo/bar", unescape("%2f%7efoo%2fbar"))
    assert_equal("/~foo+bar", unescape("/%7efoo+bar"))
  end

  def test_unescape_form
    assert_equal("//foo/bar", unescape_form("/%2Ffoo/bar"))
    assert_equal("//foo/bar baz", unescape_form("/%2Ffoo/bar+baz"))
    assert_equal("/~foo/bar baz", unescape_form("/%7Efoo/bar+baz"))
  end

  def test_escape_path
    assert_equal("/foo/bar", escape_path("/foo/bar"))
    assert_equal("/foo/bar/", escape_path("/foo/bar/"))
    assert_equal("/%25foo/bar/", escape_path("/%foo/bar/"))
  end

  def test_mime_type_by_suffix
    tab = {
      "gz" => "application/gzip",
      "tar.gz" => "application/x-tar",
      "tgz" => "application/gzip",
      "txt" => "text/plain",
    }

    assert_equal("application/x-tar", mime_type_by_suffix("foo.tar.gz", tab))
    assert_equal("application/gzip", mime_type_by_suffix("foo.gz", tab))
    assert_equal("text/plain", mime_type_by_suffix("foo.TXT", tab))
    assert_equal("text/plain", mime_type_by_suffix("/dir/foo.txt", tab))
    assert_nil(mime_type_by_suffix("foo.unknown", tab))
    assert_nil(mime_type_by_suffix("foo", tab))
    # same as ::mime_type, a leading dot still counts as a suffix
    assert_equal("text/plain", mime_type_by_suffix(".txt", tab))
    assert_nil(mime_type_by_suffix("foo.", tab))
  end

  def test_mime_type_by_suffix_most_specific_wins
    tab = { "gz" => "application/gzip", "tar.gz" => "application/x-tar" }

    assert_equal("application/x-tar", mime_type_by_suffix("a.b.tar.gz", tab))
    assert_equal("application/gzip", mime_type_by_suffix("a.b.gz", tab))
    # no entry for any suffix of the file
    assert_nil(mime_type_by_suffix("a.b.tar.bz2", tab))
  end

  def test_mime_type_by_suffix_language_component
    tab = { "html" => "text/html", "en" => "text/x-english" }

    # a compound suffix ends the name, so it wins over the language
    assert_equal("text/x-english",
                 mime_type_by_suffix("index.html.en", tab))
    assert_equal("text/x-english",
                 mime_type_by_suffix("index.en", tab))
    # the component before the last one is tried, like ::mime_type does
    assert_equal("text/html",
                 mime_type_by_suffix("index.html.de", tab))
  end

  def test_freeze_mime_types
    source = { ".TAR.GZ" => "application/x-tar", "GZ" => "application/gzip" }
    frozen = freeze_mime_types(source)

    assert_equal("application/x-tar", frozen["tar.gz"])
    assert_equal("application/gzip", frozen["gz"])
    assert_predicate(frozen, :frozen?)
    assert_nil(freeze_mime_types(nil))

    # the snapshot is independent of the source mapping
    source["tar.gz"] = "application/octet-stream"
    assert_equal("application/x-tar", frozen["tar.gz"])

    # a frozen table is normalized as well when its keys are not already
    assert_equal("application/x-tar",
                 freeze_mime_types({ ".TAR.GZ" => "application/x-tar" }.freeze)["tar.gz"])

    # an already normalized snapshot is returned as is, so a mount's snapshot
    # can be passed to each request without being copied again
    assert_same(frozen, freeze_mime_types(frozen))
  end
end
