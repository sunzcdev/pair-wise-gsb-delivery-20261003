require_relative "../spec_helper"

describe "match hook plugin" do
  it "matches verbs" do
    matches = []
    app(:bare) do
      plugin :match_hook
      match_hook do
        matches << [request.matched_path, request.remaining_path]
      end
      route do |r|
        r.on "foo" do
          r.on "bar" do
            r.get "baz" do
              "fbb"
            end
            "fb"
          end
          "f"
        end

        r.get "bar" do
          "b"
        end

        r.root do
          "r"
        end

        "n"
      end
    end

    body("/foo").must_equal 'f'
    matches.must_equal [["/foo", ""]]

    matches.clear
    body("/foo/bar").must_equal 'fb'
    matches.must_equal [["/foo", "/bar"], ["/foo/bar", ""]]

    matches.clear
    body("/foo/bar/baz").must_equal 'fbb'
    matches.must_equal [["/foo", "/bar/baz"], ["/foo/bar", "/baz"], ["/foo/bar/baz", ""]]

    matches.clear
    body("/bar").must_equal 'b'
    matches.must_equal [["/bar", ""]]

    matches.clear
    body.must_equal 'r'
    matches.must_equal [["", "/"]]

    matches.clear
    body('/x').must_equal 'n'
    matches.must_be_empty

    matches.clear
    body("/foo/baz").must_equal 'f'
    matches.must_equal [["/foo", "/baz"]]

    matches.clear
    body("/foo/bar/bar").must_equal 'fb'
    matches.must_equal [["/foo", "/bar/bar"], ["/foo/bar", "/bar"]]

    app.match_hook{matches << :x }

    matches.clear
    body("/foo/bar/baz").must_equal 'fbb'
    matches.must_equal [["/foo", "/bar/baz"], :x, ["/foo/bar", "/baz"], :x, ["/foo/bar/baz", ""], :x]

    app.freeze

    matches.clear
    body("/foo/bar/baz").must_equal 'fbb'
    matches.must_equal [["/foo", "/bar/baz"], :x, ["/foo/bar", "/baz"], :x, ["/foo/bar/baz", ""], :x]
  end

  it "does not record a route trace unless the trace option is used" do
    enabled = nil
    trace = nil
    app(:bare) do
      plugin :match_hook
      match_hook do
        enabled = request.route_trace_enabled?
        trace = request.route_trace
      end
      route do |r|
        r.on "a" do
          "a"
        end
      end
    end

    body("/a").must_equal 'a'
    enabled.must_equal false
    trace.must_equal []
  end

  it "records a route trace with the trace option" do
    trace = nil
    app(:bare) do
      plugin :match_hook, trace: true
      match_hook do
        trace = request.route_trace
      end
      route do |r|
        r.on "foo" do
          r.on "bar" do
            "foobar"
          end
        end
      end
    end

    body("/foo/bar").must_equal 'foobar'
    trace.map(&:marker).must_equal [%w"foo", %w"bar"]
    trace.map(&:status).must_equal [:committed, :committed]
    trace.map(&:final?).must_equal [false, true]
  end

  it "distinguishes abandoned candidates from the final handler with the pass plugin" do
    trace = nil
    app(:bare) do
      plugin :match_hook, trace: true
      plugin :pass
      match_hook do
        trace = request.route_trace
      end
      route do |r|
        r.on "foo" do
          r.pass
        end

        r.on "foo" do
          r.is "bar" do
            "foobar"
          end
        end
      end
    end

    body("/foo/bar").must_equal 'foobar'
    trace.map(&:marker).must_equal [%w"foo", %w"foo", ["bar", app::RodaRequest::TERM]]
    trace.map(&:status).must_equal [:abandoned, :committed, :committed]
    trace.map(&:final?).must_equal [false, false, true]
    trace.first.must_be :abandoned?
  end

  it "distinguishes abandoned candidates from the final handler with the break plugin" do
    trace = nil
    app(:bare) do
      plugin :match_hook, trace: true
      plugin :break
      match_hook do
        trace = request.route_trace
      end
      route do |r|
        r.on "foo" do
          break
        end

        r.on "foo" do
          r.is "bar" do
            "foobar"
          end
        end
      end
    end

    body("/foo/bar").must_equal 'foobar'
    trace.map(&:marker).must_equal [%w"foo", %w"foo", ["bar", app::RodaRequest::TERM]]
    trace.map(&:status).must_equal [:abandoned, :committed, :committed]
    trace.map(&:final?).must_equal [false, false, true]
  end
end
