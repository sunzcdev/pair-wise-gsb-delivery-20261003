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
end
