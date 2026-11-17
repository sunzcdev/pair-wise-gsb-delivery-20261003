# frozen_string_literal: true

require_relative 'helper'

separate_testing do
  require_relative '../lib/rack/urlmap'
  require_relative '../lib/rack/lint'
  require_relative '../lib/rack/mock_request'
end

describe Rack::URLMap do
  it "dispatches paths correctly" do
    app = lambda { |env|
      [200, {
        'x-scriptname' => env['SCRIPT_NAME'],
        'x-pathinfo' => env['PATH_INFO'],
        'content-type' => 'text/plain'
      }, [""]]
    }
    map = Rack::Lint.new(Rack::URLMap.new({
      'http://foo.org/bar' => app,
      '/foo' => app,
      '/foo/bar' => app
    }))

    res = Rack::MockRequest.new(map).get("/")
    res.must_be :not_found?

    res = Rack::MockRequest.new(map).get("/qux")
    res.must_be :not_found?

    res = Rack::MockRequest.new(map).get("/foo")
    res.must_be :ok?
    res["x-scriptname"].must_equal "/foo"
    res["x-pathinfo"].must_equal ""

    res = Rack::MockRequest.new(map).get("/foo/")
    res.must_be :ok?
    res["x-scriptname"].must_equal "/foo"
    res["x-pathinfo"].must_equal "/"

    res = Rack::MockRequest.new(map).get("/foo/bar")
    res.must_be :ok?
    res["x-scriptname"].must_equal "/foo/bar"
    res["x-pathinfo"].must_equal ""

    res = Rack::MockRequest.new(map).get("/foo/bard")
    res.must_be :ok?
    res["x-scriptname"].must_equal "/foo"
    res["x-pathinfo"].must_equal "/bard"

    res = Rack::MockRequest.new(map).get("/foo/bar/")
    res.must_be :ok?
    res["x-scriptname"].must_equal "/foo/bar"
    res["x-pathinfo"].must_equal "/"

    res = Rack::MockRequest.new(map).get("/foo///bar//quux")
    res.status.must_equal 200
    res.must_be :ok?
    res["x-scriptname"].must_equal "/foo/bar"
    res["x-pathinfo"].must_equal "//quux"

    res = Rack::MockRequest.new(map).get("/foo/quux", "SCRIPT_NAME" => "/bleh")
    res.must_be :ok?
    res["x-scriptname"].must_equal "/bleh/foo"
    res["x-pathinfo"].must_equal "/quux"

    res = Rack::MockRequest.new(map).get("/bar", 'HTTP_HOST' => 'foo.org')
    res.must_be :ok?
    res["x-scriptname"].must_equal "/bar"
    res["x-pathinfo"].must_be :empty?

    res = Rack::MockRequest.new(map).get("/bar/", 'HTTP_HOST' => 'foo.org')
    res.must_be :ok?
    res["x-scriptname"].must_equal "/bar"
    res["x-pathinfo"].must_equal '/'
  end


  it "dispatches hosts correctly" do
    map = Rack::Lint.new(Rack::URLMap.new("http://foo.org/" => lambda { |env|
                             [200,
                              { "content-type" => "text/plain",
                                "x-position" => "foo.org",
                                "x-host" => env["HTTP_HOST"] || env["SERVER_NAME"],
                              }, [""]]},
                           "http://subdomain.foo.org/" => lambda { |env|
                             [200,
                              { "content-type" => "text/plain",
                                "x-position" => "subdomain.foo.org",
                                "x-host" => env["HTTP_HOST"] || env["SERVER_NAME"],
                              }, [""]]},
                           "http://bar.org/" => lambda { |env|
                             [200,
                              { "content-type" => "text/plain",
                                "x-position" => "bar.org",
                                "x-host" => env["HTTP_HOST"] || env["SERVER_NAME"],
                              }, [""]]},
                           "/" => lambda { |env|
                             [200,
                              { "content-type" => "text/plain",
                                "x-position" => "default.org",
                                "x-host" => env["HTTP_HOST"] || env["SERVER_NAME"],
                              }, [""]]}
                           ))

    res = Rack::MockRequest.new(map).get("/")
    res.must_be :ok?
    res["x-position"].must_equal "default.org"

    res = Rack::MockRequest.new(map).get("/", "HTTP_HOST" => "bar.org")
    res.must_be :ok?
    res["x-position"].must_equal "bar.org"

    res = Rack::MockRequest.new(map).get("/", "HTTP_HOST" => "foo.org")
    res.must_be :ok?
    res["x-position"].must_equal "foo.org"

    res = Rack::MockRequest.new(map).get("/", "HTTP_HOST" => "subdomain.foo.org", "SERVER_NAME" => "foo.org")
    res.must_be :ok?
    res["x-position"].must_equal "subdomain.foo.org"

    res = Rack::MockRequest.new(map).get("http://foo.org/")
    res.must_be :ok?
    res["x-position"].must_equal "foo.org"

    res = Rack::MockRequest.new(map).get("/", "HTTP_HOST" => "example.org")
    res.must_be :ok?
    res["x-position"].must_equal "default.org"

    res = Rack::MockRequest.new(map).get("/", "HTTP_HOST" => "any-host.org")
    res.must_be :ok?
    res["x-position"].must_equal "default.org"

    res = Rack::MockRequest.new(map).get("/", "HTTP_HOST" => "any-host.org", "HTTP_X_FORWARDED_HOST" => "any-host.org")
    res.must_be :ok?
    res["x-position"].must_equal "default.org"

    res = Rack::MockRequest.new(map).get("/",
                                         "HTTP_HOST" => "example.org:9292",
                                         "SERVER_PORT" => "9292")
    res.must_be :ok?
    res["x-position"].must_equal "default.org"
  end

  it "be nestable" do
    map = Rack::Lint.new(Rack::URLMap.new("/foo" =>
      Rack::URLMap.new("/bar" =>
        Rack::URLMap.new("/quux" => lambda { |env|
                           [200,
                            { "content-type" => "text/plain",
                              "x-position" => "/foo/bar/quux",
                              "x-pathinfo" => env["PATH_INFO"],
                              "x-scriptname" => env["SCRIPT_NAME"],
                            }, [""]]}
                         ))))

    res = Rack::MockRequest.new(map).get("/foo/bar")
    res.must_be :not_found?

    res = Rack::MockRequest.new(map).get("/foo/bar/quux")
    res.must_be :ok?
    res["x-position"].must_equal "/foo/bar/quux"
    res["x-pathinfo"].must_equal ""
    res["x-scriptname"].must_equal "/foo/bar/quux"
  end

  it "route root apps correctly" do
    map = Rack::Lint.new(Rack::URLMap.new("/" => lambda { |env|
                             [200,
                              { "content-type" => "text/plain",
                                "x-position" => "root",
                                "x-pathinfo" => env["PATH_INFO"],
                                "x-scriptname" => env["SCRIPT_NAME"]
                              }, [""]]},
                           "/foo" => lambda { |env|
                             [200,
                              { "content-type" => "text/plain",
                                "x-position" => "foo",
                                "x-pathinfo" => env["PATH_INFO"],
                                "x-scriptname" => env["SCRIPT_NAME"]
                              }, [""]]}
                           ))

    res = Rack::MockRequest.new(map).get("/foo/bar")
    res.must_be :ok?
    res["x-position"].must_equal "foo"
    res["x-pathinfo"].must_equal "/bar"
    res["x-scriptname"].must_equal "/foo"

    res = Rack::MockRequest.new(map).get("/foo")
    res.must_be :ok?
    res["x-position"].must_equal "foo"
    res["x-pathinfo"].must_equal ""
    res["x-scriptname"].must_equal "/foo"

    res = Rack::MockRequest.new(map).get("/bar")
    res.must_be :ok?
    res["x-position"].must_equal "root"
    res["x-pathinfo"].must_equal "/bar"
    res["x-scriptname"].must_equal ""

    res = Rack::MockRequest.new(map).get("")
    res.must_be :ok?
    res["x-position"].must_equal "root"
    res["x-pathinfo"].must_equal "/"
    res["x-scriptname"].must_equal ""
  end

  it "not squeeze slashes" do
    map = Rack::Lint.new(Rack::URLMap.new("/" => lambda { |env|
                             [200,
                              { "content-type" => "text/plain",
                                "x-position" => "root",
                                "x-pathinfo" => env["PATH_INFO"],
                                "x-scriptname" => env["SCRIPT_NAME"]
                              }, [""]]},
                           "/foo" => lambda { |env|
                             [200,
                              { "content-type" => "text/plain",
                                "x-position" => "foo",
                                "x-pathinfo" => env["PATH_INFO"],
                                "x-scriptname" => env["SCRIPT_NAME"]
                              }, [""]]}
                           ))

    res = Rack::MockRequest.new(map).get("/http://example.org/bar")
    res.must_be :ok?
    res["x-position"].must_equal "root"
    res["x-pathinfo"].must_equal "/http://example.org/bar"
    res["x-scriptname"].must_equal ""
  end

  it "not be case sensitive with hosts" do
    map = Rack::Lint.new(Rack::URLMap.new("http://example.org/" => lambda { |env|
                             [200,
                              { "content-type" => "text/plain",
                                "x-position" => "root",
                                "x-pathinfo" => env["PATH_INFO"],
                                "x-scriptname" => env["SCRIPT_NAME"]
                              }, [""]]}
                           ))

    res = Rack::MockRequest.new(map).get("http://example.org/")
    res.must_be :ok?
    res["x-position"].must_equal "root"
    res["x-pathinfo"].must_equal "/"
    res["x-scriptname"].must_equal ""

    res = Rack::MockRequest.new(map).get("http://EXAMPLE.ORG/")
    res.must_be :ok?
    res["x-position"].must_equal "root"
    res["x-pathinfo"].must_equal "/"
    res["x-scriptname"].must_equal ""
  end

  it "not allow locations unless they start with /" do
    lambda do
      Rack::URLMap.new("a/" => lambda { |env| })
    end.must_raise ArgumentError
  end

  it "keep the old mapping when remap fails validation" do
    app = lambda { |env| [200, {'content-type' => 'text/plain'}, ["ok"]] }
    urlmap = Rack::URLMap.new("/foo" => app)

    # A valid request works before the failed remap.
    res = Rack::MockRequest.new(urlmap).get("/foo")
    res.must_be :ok?

    lambda do
      urlmap.remap("invalid-location" => app)
    end.must_raise ArgumentError

    # The old mapping is still fully functional.
    res = Rack::MockRequest.new(urlmap).get("/foo")
    res.must_be :ok?

    # The new, invalid mapping was not installed.
    res = Rack::MockRequest.new(urlmap).get("/")
    res.must_be :not_found?
  end

  it "swap the whole routing table atomically with remap" do
    old_app = lambda { |env| [200, {'content-type' => 'text/plain', 'x-app' => 'old'}, [""]] }
    new_app = lambda { |env| [200, {'content-type' => 'text/plain', 'x-app' => 'new'}, [""]] }

    urlmap = Rack::URLMap.new("/old" => old_app)

    res = Rack::MockRequest.new(urlmap).get("/old")
    res.must_be :ok?
    res["x-app"].must_equal "old"

    urlmap.remap("/new" => new_app)

    res = Rack::MockRequest.new(urlmap).get("/old")
    res.must_be :not_found?

    res = Rack::MockRequest.new(urlmap).get("/new")
    res.must_be :ok?
    res["x-app"].must_equal "new"
  end

  it "dispatch a request against the mapping snapshot it started with" do
    outer = nil
    swapped = false
    swapping_inner_app = lambda { |env|
      unless swapped
        # Remap the outer URLMap while an outer request is still in
        # flight (its ensure/restore has not run yet).
        outer.remap("/" => lambda { |inner_env|
          [200, {'content-type' => 'text/plain', 'x-app' => 'new'}, [""]]
        })
        swapped = true
      end
      [200, {'content-type' => 'text/plain', 'x-app' => 'old'}, [""]]
    }

    inner = Rack::URLMap.new("/bar" => swapping_inner_app)
    outer = Rack::URLMap.new("/foo" => inner)

    # The in-flight outer request completes against the snapshot it
    # started matching with, even though #remap ran during the dispatch.
    res = Rack::MockRequest.new(outer).get("/foo/bar")
    res.must_be :ok?
    res["x-app"].must_equal "old"

    # The next request sees the new table: it is routed to the new root
    # app instead of the old nested /foo/bar route.
    res = Rack::MockRequest.new(outer).get("/foo/bar")
    res.must_be :ok?
    res["x-app"].must_equal "new"

    res = Rack::MockRequest.new(outer).get("/")
    res.must_be :ok?
    res["x-app"].must_equal "new"
  end

  it "match hosts case-insensitively for the known-hosts fallback as well" do
    app = lambda { |env|
      [200, {
        "content-type" => "text/plain",
        "x-host" => env["HTTP_HOST"] || env["SERVER_NAME"]
      }, [""]]
    }
    map = Rack::Lint.new(Rack::URLMap.new(
      "http://example.org/foo" => app,
      "/" => app
    ))

    # An uppercase variant of a known host must not be treated as
    # unknown, otherwise it would wrongly fall back to the hostless app.
    res = Rack::MockRequest.new(map).get("/foo", "HTTP_HOST" => "EXAMPLE.ORG")
    res.must_be :ok?
    res["x-host"].must_equal "EXAMPLE.ORG"
  end

  it "ignore default ports when matching hosts" do
    app = lambda { |env|
      [200, {
        "content-type" => "text/plain",
        "x-position" => "example",
        "x-host" => env["HTTP_HOST"] || env["SERVER_NAME"]
      }, [""]]
    }
    fallback = lambda { |env|
      [200, { "content-type" => "text/plain", "x-position" => "fallback" }, [""]]
    }
    map = Rack::Lint.new(Rack::URLMap.new(
      "http://example.org/" => app,
      "https://secure.example.org/" => app,
      "/" => fallback
    ))

    res = Rack::MockRequest.new(map).get("/", "HTTP_HOST" => "example.org:80")
    res.must_be :ok?
    res["x-position"].must_equal "example"

    res = Rack::MockRequest.new(map).get("/", "HTTP_HOST" => "EXAMPLE.ORG:80")
    res.must_be :ok?
    res["x-position"].must_equal "example"

    res = Rack::MockRequest.new(map).get("https://secure.example.org:443/")
    res.must_be :ok?
    res["x-position"].must_equal "example"

    # A non-default port is still part of the host identity, so it does
    # not match the portless mapping.  The host is then treated as
    # unknown and falls back to the hostless mapping.
    res = Rack::MockRequest.new(map).get("/", "HTTP_HOST" => "example.org:9292", "SERVER_PORT" => "9292")
    res.must_be :ok?
    res["x-position"].must_equal "fallback"
  end

  it "try longest paths first regardless of insertion order" do
    long_app  = lambda { |env| [200, { "content-type" => "text/plain", "x-position" => "long" }, [""]] }
    short_app = lambda { |env| [200, { "content-type" => "text/plain", "x-position" => "short" }, [""]] }

    # Insert the shorter path first; the longer one must still win.
    map = Rack::Lint.new(Rack::URLMap.new(
      "/foo" => short_app,
      "/foo/bar" => long_app
    ))

    res = Rack::MockRequest.new(map).get("/foo/bar")
    res.must_be :ok?
    res["x-position"].must_equal "long"

    res = Rack::MockRequest.new(map).get("/foo/baz")
    res.must_be :ok?
    res["x-position"].must_equal "short"
  end

  it "fall back to hostless mappings only when no mapped host matches" do
    hosted    = lambda { |env| [200, { "content-type" => "text/plain", "x-position" => "hosted" }, [""]] }
    hostless  = lambda { |env| [200, { "content-type" => "text/plain", "x-position" => "hostless" }, [""]] }

    map = Rack::Lint.new(Rack::URLMap.new(
      "http://example.org/" => hosted,
      "/" => hostless
    ))

    res = Rack::MockRequest.new(map).get("/", "HTTP_HOST" => "example.org")
    res.must_be :ok?
    res["x-position"].must_equal "hosted"

    res = Rack::MockRequest.new(map).get("/", "HTTP_HOST" => "unknown.example.com")
    res.must_be :ok?
    res["x-position"].must_equal "hostless"
  end

  it "restore SCRIPT_NAME and PATH_INFO when an app raises" do
    env = Rack::MockRequest.env_for(
      "/foo/bar/baz",
      "SCRIPT_NAME" => "/bleh"
    )
    original_path_info   = env["PATH_INFO"].dup
    original_script_name = env["SCRIPT_NAME"].dup

    raising_app = lambda { |inner_env| raise "boom" }
    urlmap = Rack::URLMap.new("/foo/bar" => raising_app)

    lambda { urlmap.call(env) }.must_raise RuntimeError

    env["PATH_INFO"].must_equal original_path_info
    env["SCRIPT_NAME"].must_equal original_script_name
  end

  it "restore SCRIPT_NAME and PATH_INFO after a nested dispatch and reuse the env safely" do
    inner = Rack::URLMap.new("/bar" => lambda { |inner_env|
      [200,
       {
         "content-type" => "text/plain",
         "x-pathinfo" => inner_env["PATH_INFO"],
         "x-scriptname" => inner_env["SCRIPT_NAME"]
       }, [""]]
    })
    outer = Rack::Lint.new(Rack::URLMap.new("/foo" => inner))

    res = Rack::MockRequest.new(outer).get("/foo/bar/quux")
    res.must_be :ok?
    res["x-pathinfo"].must_equal "/quux"
    res["x-scriptname"].must_equal "/foo/bar"

    # The second request on a fresh environment must not inherit any
    # leftover path pieces from the first dispatch.
    res = Rack::MockRequest.new(outer).get("/foo/bar")
    res.must_be :ok?
    res["x-scriptname"].must_equal "/foo/bar"
    res["x-pathinfo"].must_equal ""
  end
end
