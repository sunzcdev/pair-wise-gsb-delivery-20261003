# frozen_string_literal: true

# Copyright (C) Bob Aman
#
#    Licensed under the Apache License, Version 2.0 (the "License");
#    you may not use this file except in compliance with the License.
#    You may obtain a copy of the License at
#
#        http://www.apache.org/licenses/LICENSE-2.0
#
#    Unless required by applicable law or agreed to in writing, software
#    distributed under the License is distributed on an "AS IS" BASIS,
#    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#    See the License for the specific language governing permissions and
#    limitations under the License.


require "spec_helper"

require "addressable/uri"

describe Addressable::URI, ".constrained_join" do
  let(:base) { "http://example.com/docs/index.html" }
  let(:origin) { "http://example.com" }
  let(:boundary) { "/docs" }

  def resolve(reference, **options)
    defaults = {
      allowed_origins: [origin], path_boundary: boundary
    }
    Addressable::URI.constrained_join(
      base, reference, **defaults.merge(options)
    )
  end

  describe "allowed resolutions" do
    it "resolves a same-directory relative reference" do
      result = resolve("page2.html")
      expect(result).to be_allowed
      expect(result.uri).to eq(
        Addressable::URI.parse("http://example.com/docs/page2.html")
      )
      expect(result.to_s).to eq(
        "http://example.com/docs/page2.html"
      )
      expect(result.reason).to eq(nil)
      expect(result.message).to eq(nil)
    end

    it "resolves a subdirectory reference" do
      result = resolve("sub/page.html")
      expect(result).to be_allowed
      expect(result.uri.to_s).to eq(
        "http://example.com/docs/sub/page.html"
      )
    end

    it "resolves an absolute path inside the boundary" do
      result = resolve("/docs/sub/page.html")
      expect(result).to be_allowed
      expect(result.uri.to_s).to eq(
        "http://example.com/docs/sub/page.html"
      )
    end

    it "accepts the boundary path itself" do
      result = resolve("/docs")
      expect(result).to be_allowed
      expect(result.uri.path).to eq("/docs")
    end

    it "accepts an in-directory self-reference via dot segments" do
      result = resolve("/docs/./page.html")
      expect(result).to be_allowed
      expect(result.uri.path).to eq("/docs/page.html")
    end

    it "accepts a harmless parent segment that stays inside the boundary" do
      result = resolve("/docs/sub/../page.html")
      expect(result).to be_allowed
      expect(result.uri.path).to eq("/docs/page.html")
    end

    it "treats an empty reference as the base itself" do
      result = resolve("")
      expect(result).to be_allowed
      expect(result.uri.to_s).to eq(
        "http://example.com/docs/index.html"
      )
    end

    it "treats a query-only reference as the base with a new query" do
      result = resolve("?q=1")
      expect(result).to be_allowed
      expect(result.uri.to_s).to eq(
        "http://example.com/docs/index.html?q=1"
      )
    end

    it "treats a fragment-only reference as the base with a new fragment" do
      result = resolve("#section")
      expect(result).to be_allowed
      expect(result.uri.to_s).to eq(
        "http://example.com/docs/index.html#section"
      )
    end

    it "treats an encoded slash as data, not a path separator" do
      # `a%2F..%2Fb` is a single path segment; it never splits, and
      # therefore cannot traverse upward.
      result = resolve("/docs/a%2F..%2Fb")
      expect(result).to be_allowed
      expect(result.uri.path).to eq("/docs/a%2F..%2Fb")
    end

    it "folds the default port when matching origins" do
      result = resolve("page.html",
        allowed_origins: ["http://example.com:80"])
      expect(result).to be_allowed
    end

    it "matches hosts case-insensitively" do
      result = resolve("http://EXAMPLE.COM/docs/page.html")
      expect(result).to be_allowed
    end

    it "accepts an explicit origin with a non-default port" do
      result = Addressable::URI.constrained_join(
        "http://example.com:8080/docs/index.html", "page.html",
        allowed_origins: ["http://example.com:8080"],
        path_boundary: "/docs"
      )
      expect(result).to be_allowed
      expect(result.uri.origin).to eq("http://example.com:8080")
    end
  end

  describe "rejected resolutions" do
    it "rejects a plain dot-segment escape from a nested subdirectory" do
      result = resolve("sub/../../secret")
      expect(result).to be_rejected
      expect(result.reason).to eq(:path_outside_boundary)
      expect(result.uri.to_s).to eq("http://example.com/secret")
      expect(result.message).to include("/docs")
    end

    it "allows a dot segment that merely returns to the boundary" do
      result = resolve("sub/../secret")
      expect(result).to be_allowed
      expect(result.uri.to_s).to eq("http://example.com/docs/secret")
    end

    it "rejects an absolute dot-segment escape" do
      result = resolve("/docs/../secret")
      expect(result).to be_rejected
      expect(result.reason).to eq(:path_outside_boundary)
      expect(result.uri.path).to eq("/secret")
    end

    it "rejects a percent-encoded dot-segment escape" do
      result = resolve("/docs/%2e%2e/secret")
      expect(result).to be_rejected
      expect(result.reason).to eq(:path_outside_boundary)
      # The judgment is made on the resolved URI, reported verbatim.
      expect(result.uri.path).to eq("/secret")
    end

    it "rejects a percent-encoded relative dot-segment escape" do
      result = resolve("%2e%2e/secret")
      expect(result).to be_rejected
      expect(result.reason).to eq(:path_outside_boundary)
      expect(result.uri.path).to eq("/secret")
    end

    it "rejects mixed-case percent-encoded dot segments" do
      result = resolve("/docs/%2E%2e/%2E%2E/secret")
      expect(result).to be_rejected
      expect(result.reason).to eq(:path_outside_boundary)
      expect(result.uri.path).to eq("/secret")
    end

    it "rejects a sibling directory despite the textual prefix match" do
      # "/docs-old" starts with "/docs" as a string but is not inside
      # the "/docs" directory.
      result = resolve("/docs-old/secret")
      expect(result).to be_rejected
      expect(result.reason).to eq(:path_outside_boundary)
      expect(result.uri.path).to eq("/docs-old/secret")
    end

    it "rejects a relative reference resolving into a sibling directory" do
      result = resolve("../docs-old/secret")
      expect(result).to be_rejected
      expect(result.reason).to eq(:path_outside_boundary)
      expect(result.uri.path).to eq("/docs-old/secret")
    end

    it "rejects a network-path reference that changes the host" do
      result = resolve("//evil.example/docs/page.html")
      expect(result).to be_rejected
      expect(result.reason).to eq(:disallowed_origin)
      expect(result.uri.host).to eq("evil.example")
    end

    it "rejects an absolute reference to a different host" do
      result = resolve("http://evil.example/docs/page.html")
      expect(result).to be_rejected
      expect(result.reason).to eq(:disallowed_origin)
    end

    it "rejects a different scheme even for the same host" do
      result = resolve("https://example.com/docs/page.html")
      expect(result).to be_rejected
      expect(result.reason).to eq(:disallowed_origin)
    end

    it "rejects a non-default port that differs from the allowed origin" do
      result = resolve("http://example.com:9000/docs/page.html")
      expect(result).to be_rejected
      expect(result.reason).to eq(:disallowed_origin)
      expect(result.uri.port).to eq(9000)
    end

    it "does not treat the default port as differing from no port" do
      result = resolve("http://example.com:80/docs/page.html",
        allowed_origins: ["http://example.com"])
      expect(result).to be_allowed
    end

    it "rejects a path escape even when the origin matches" do
      result = resolve("http://example.com/secret")
      expect(result).to be_rejected
      expect(result.reason).to eq(:path_outside_boundary)
      expect(result.uri.to_s).to eq("http://example.com/secret")
    end
  end

  describe "base URI validation" do
    it "rejects a relative base URI" do
      result = Addressable::URI.constrained_join(
        "docs/index.html", "page.html",
        allowed_origins: ["http://example.com"],
        path_boundary: "/docs"
      )
      expect(result).to be_rejected
      expect(result.reason).to eq(:invalid_base)
    end
  end

  describe "boundary parameter validation" do
    it "requires an absolute boundary path" do
      expect {
        Addressable::URI.constrained_join(
          base, "page.html",
          allowed_origins: [origin],
          path_boundary: "docs"
        )
      }.to raise_error(ArgumentError, /absolute/)
    end
  end

  describe "return value identity" do
    it "reports the exact URI used for the verdict (rejection)" do
      result = resolve("/docs/../%65vil")
      expect(result).to be_rejected
      expect(result.uri.to_s).to eq("http://example.com/evil")
    end

    it "reports the exact URI used for the verdict (allowance)" do
      result = resolve("/docs/./%70age.html")
      expect(result).to be_allowed
      expect(result.uri.to_s).to eq("http://example.com/docs/page.html")
    end
  end

  describe "instance method" do
    it "behaves identically to the class method" do
      instance = Addressable::URI.parse(base)
      result = instance.constrained_join(
        "page.html",
        allowed_origins: [origin], path_boundary: boundary
      )
      expect(result).to be_allowed
      expect(result.uri.to_s).to eq(
        "http://example.com/docs/page.html"
      )
    end
  end
end

describe Addressable::URI::ConstrainedJoinResult do
  it "rejects unknown reasons" do
    uri = Addressable::URI.parse("http://example.com/")
    expect {
      described_class.new(uri, :bogus, "msg")
    }.to raise_error(ArgumentError, /Unknown reason/)
  end

  it "requires reason and message to agree" do
    uri = Addressable::URI.parse("http://example.com/")
    expect {
      described_class.new(uri, :invalid_base, nil)
    }.to raise_error(ArgumentError)
    expect {
      described_class.new(uri, nil, "msg")
    }.to raise_error(ArgumentError)
  end
end
