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
end
