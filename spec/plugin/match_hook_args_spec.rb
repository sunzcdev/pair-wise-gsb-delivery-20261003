require_relative "../spec_helper"

describe "match_hook_args plugin" do
  it "yields matchers and block args to match hooks" do
    matches = []
    app(:bare) do
      plugin :match_hook_args
      add_match_hook do |matchers, block_args|
        matches << [matchers, block_args, request.matched_path, request.remaining_path]
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

        r.get "baz", Integer do |id|
          "b-#{id}"
        end

        r.root do
          "r"
        end

        "n"
      end
    end

    term = app::RodaRequest::TERM

    body("/foo").must_equal 'f'
    matches.must_equal [[%w"foo", [], "/foo", ""]]

    matches.clear
    body("/foo/bar").must_equal 'fb'
    matches.must_equal [[%w"foo", [], "/foo", "/bar"], [%w"bar", [], "/foo/bar", ""]]

    matches.clear
    body("/foo/bar/baz").must_equal 'fbb'
    matches.must_equal [[%w"foo", [], "/foo", "/bar/baz"], [%w"bar", [], "/foo/bar", "/baz"], [["baz", term], [], "/foo/bar/baz", ""]]

    matches.clear
    body("/bar").must_equal 'b'
    matches.must_equal [[["bar", term], [], "/bar", ""]]

    matches.clear
    body("/baz/1").must_equal 'b-1'
    matches.must_equal [[["baz", Integer, term], [1], "/baz/1", ""]]

    matches.clear
    body.must_equal 'r'
    matches.must_equal [[nil, nil, "", "/"]]

    matches.clear
    body('/x').must_equal 'n'
    matches.must_be_empty

    matches.clear
    body("/foo/baz").must_equal 'f'
    matches.must_equal [[%w"foo", [], "/foo", "/baz"]]

    matches.clear
    body("/foo/bar/bar").must_equal 'fb'
    matches.must_equal [[%w"foo", [], "/foo", "/bar/bar"], [%w"bar", [], "/foo/bar", "/bar"]]

    app.add_match_hook{|_,_|matches << :x }

    matches.clear
    body("/foo/bar/baz").must_equal 'fbb'
    matches.must_equal [[%w"foo", [], "/foo", "/bar/baz"], :x, [%w"bar", [], "/foo/bar", "/baz"], :x, [["baz", term], [], "/foo/bar/baz", ""], :x]

    app.freeze

    matches.clear
    body("/foo/bar/baz").must_equal 'fbb'
    matches.must_equal [[%w"foo", [], "/foo", "/bar/baz"], :x, [%w"bar", [], "/foo/bar", "/baz"], :x, [["baz", term], [], "/foo/bar/baz", ""], :x]

    app.opts[:match_hook_args].must_be :frozen?
  end

  it "does not record a route trace by default" do
    enabled = []
    trace = []
    app(:bare) do
      plugin :match_hook_args
      add_match_hook do |_, _|
        enabled << request.route_trace_enabled?
        trace << request.route_trace
      end
      route do |r|
        r.on "foo" do
          "foo"
        end
      end
    end

    body("/foo").must_equal 'foo'
    enabled.must_equal [false]
    trace.must_equal [[]]
    trace.first.must_be :frozen?
    app::RodaRequest.new(app.new({}), {}).route_trace.must_be :frozen?
  end

  it "records a route trace when the trace option is used" do
    trace = nil
    app(:bare) do
      plugin :match_hook_args, trace: true
      add_match_hook do |_, _|
        trace = request.route_trace
      end
      route do |r|
        r.on "foo" do
          r.on "bar" do
            r.get "baz" do
              "fbb"
            end
          end
          "fb"
        end
      end
    end

    body("/foo/bar/baz").must_equal 'fbb'

    trace.map(&:marker).must_equal [%w"foo", %w"bar", ["baz", app::RodaRequest::TERM]]
    trace.map(&:status).must_equal [:committed, :committed, :committed]
    trace.map(&:final?).must_equal [false, false, true]
    trace.map(&:committed?).must_equal [true, true, true]
    trace.map(&:abandoned?).must_equal [false, false, false]
  end

  it "records a nil marker for matches without matchers" do
    trace = nil
    app(:bare) do
      plugin :match_hook_args, trace: true
      add_match_hook do |_, _|
        trace = request.route_trace
      end
      route do |r|
        r.root do
          "root"
        end
      end
    end

    body.must_equal 'root'
    trace.map(&:marker).must_equal [nil]
    trace.map(&:final?).must_equal [true]
  end

  it "uses stable declaration markers without recording captured values" do
    trace = nil
    app(:bare) do
      plugin :match_hook_args, trace: true
      add_match_hook do |_, _|
        trace = request.route_trace
      end
      route do |r|
        r.on "users", Integer, ["a", "b"] do
          "matched"
        end
      end
    end

    body("/users/123/a").must_equal 'matched'
    trace.length.must_equal 1
    trace.first.marker.must_equal ["users", Integer, ["a", "b"]]
    trace.first.marker.must_be :frozen?
    trace.first.marker[2].must_be :frozen?
    trace.first.inspect.wont_include('123')
  end

  it "uses declared symbol and regexp matchers as stable markers" do
    trace = nil
    app(:bare) do
      plugin :match_hook_args, trace: true
      add_match_hook do |_, _|
        trace = request.route_trace
      end
      route do |r|
        r.on :secret_id do
          r.is(/\d+/) do
            "matched"
          end
        end
      end
    end

    body("/abc/123").must_equal 'matched'
    trace.map(&:marker).must_equal [[:secret_id], [/\d+/, app::RodaRequest::TERM]]
    trace.first.inspect.wont_include('abc')
    trace.last.inspect.wont_include('123')
  end

  it "does not execute any matcher more often when tracing" do
    run = lambda do |tracing|
      calls = 0
      app(:bare) do
        plugin :match_hook_args, **(tracing ? {trace: true} : {})
        route do |r|
          r.on proc{calls += 1; true} do
            "matched"
          end
          "not matched"
        end
      end
      [body, calls]
    end

    run.call(false).must_equal ['matched', 1]
    run.call(true).must_equal ['matched', 1]
  end

  it "does not execute any matcher more often when tracing is used with an array backtracking" do
    run = lambda do |tracing|
      calls = 0
      app(:bare) do
        plugin :match_hook_args, **(tracing ? {trace: true} : {})
        plugin :backtracking_array
        route do |r|
          r.is [proc{calls += 1; 'a'}, 'a/b'] do
            "matched"
          end
        end
      end
      [body('/a/b'), calls]
    end

    run.call(false).must_equal ['matched', 1]
    run.call(true).must_equal ['matched', 1]
  end

  it "marks matches abandoned when a match block is passed" do
    trace = nil
    app(:bare) do
      plugin :match_hook_args, trace: true
      plugin :pass
      add_match_hook do |_, _|
        trace = request.route_trace
      end
      route do |r|
        r.on "a" do
          r.pass
        end

        r.on "a" do
          "handled"
        end
      end
    end

    body("/a").must_equal 'handled'

    trace.map(&:marker).must_equal [%w"a", %w"a"]
    trace.map(&:status).must_equal [:abandoned, :committed]
    trace.map(&:final?).must_equal [false, true]
    trace.map(&:abandoned?).must_equal [true, false]
  end

  it "marks matches abandoned when a match block is broken out of" do
    trace = nil
    app(:bare) do
      plugin :match_hook_args, trace: true
      plugin :break
      add_match_hook do |_, _|
        trace = request.route_trace
      end
      route do |r|
        r.on "a" do
          break
        end

        r.on "a" do
          "handled"
        end
      end
    end

    body("/a").must_equal 'handled'

    trace.map(&:marker).must_equal [%w"a", %w"a"]
    trace.map(&:status).must_equal [:abandoned, :committed]
    trace.map(&:final?).must_equal [false, true]
  end

  it "marks matches abandoned when a match block raises an exception" do
    trace = nil
    app(:bare) do
      plugin :match_hook_args, trace: true
      add_match_hook do |_, _|
        trace = request.route_trace
      end
      route do |r|
        r.on "a" do
          raise "boom"
        end
      end
    end

    proc{body("/a")}.must_raise RuntimeError

    trace.map(&:marker).must_equal [%w"a"]
    trace.map(&:status).must_equal [:abandoned]
    trace.map(&:final?).must_equal [false]
  end

  it "keeps only the innermost committed match final when an outer match commits" do
    trace = nil
    app(:bare) do
      plugin :match_hook_args, trace: true
      add_match_hook do |_, _|
        trace = request.route_trace
      end
      route do |r|
        r.on "a" do
          r.on "b" do
            "ab"
          end
        end
      end
    end

    body("/a/b").must_equal 'ab'

    trace.map(&:marker).must_equal [%w"a", %w"b"]
    trace.map(&:status).must_equal [:committed, :committed]
    trace.map(&:final?).must_equal [false, true]
  end

  it "marks an earlier committed match non-final if a later dispatch supersedes it" do
    trace = nil
    app(:bare) do
      plugin :match_hook_args, trace: true
      plugin :class_level_routing
      add_match_hook do |_, _|
        trace = request.route_trace
      end

      on "a" do
        "class-a"
      end

      route do |r|
        r.on "a" do
          nil
        end
      end
    end

    body("/a").must_equal 'class-a'

    trace.map(&:marker).must_equal [%w"a", %w"a"]
    trace.map(&:status).must_equal [:abandoned, :committed]
    trace.map(&:final?).must_equal [false, true]
  end

  it "returns a live route trace from the request" do
    traces = []
    enabled = []
    app(:bare) do
      plugin :match_hook_args, trace: true
      add_match_hook do |_, _|
        traces << request.route_trace
        enabled << request.route_trace_enabled?
      end
      route do |r|
        r.on "a" do
          r.on "b" do
            "ab"
          end
        end
      end
    end

    body("/a/b").must_equal 'ab'
    enabled.must_equal [true, true]
    traces[0].must_be_same_as traces[1]
    traces[0].length.must_equal 2
  end

  it "keeps the existing match hook stage, with the trace resolved by the after hook" do
    hook_entries = []
    after_entries = nil
    app(:bare) do
      plugin :match_hook_args, trace: true
      plugin :hooks
      plugin :pass
      add_match_hook do |_, _|
        entry = request.route_trace.last
        hook_entries << [entry.marker, entry.status]
      end
      after do |_|
        after_entries = request.route_trace.map{|e| [e.marker, e.status, e.final?]}
      end
      route do |r|
        r.on "a" do
          r.pass
        end

        r.on "a" do
          r.is "b" do
            "ab"
          end
        end
      end
    end

    body("/a/b").must_equal 'ab'

    # The match hook still runs at the original stage, before the match has
    # been committed or abandoned, so only the last recorded entry is visible.
    hook_entries.must_equal [[%w"a", :tried], [%w"a", :tried], [["b", app::RodaRequest::TERM], :tried]]

    after_entries.must_equal [
      [%w"a", :abandoned, false],
      [%w"a", :committed, false],
      [["b", app::RodaRequest::TERM], :committed, true],
    ]
  end
end
