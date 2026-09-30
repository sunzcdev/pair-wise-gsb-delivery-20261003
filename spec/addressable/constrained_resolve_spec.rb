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

describe Addressable::URI, ".constrained_resolve" do
  let(:base) { "http://example.com/docs/index.html" }
  let(:origin) { "http://example.com" }
  let(:docs) { "/docs" }

  def resolve(reference, allowed_origins: origin, directory: docs, base_uri: base)
    Addressable::URI.constrained_resolve(
      base_uri, reference, allowed_origins, directory
    )
  end

  it "allows an ordinary relative reference below the directory" do
    result = resolve("install.html")
    expect(result).to be_allowed
    expect(result.uri).to be_a(Addressable::URI)
    expect(result.uri.to_s).to eq("http://example.com/docs/install.html")
  end

  it "allows a subdirectory reference" do
    result = resolve("guide/install.html")
    expect(result).to be_allowed
    expect(result.uri.to_s).to eq("http://example.com/docs/guide/install.html")
  end

  it "allows an absolute-path reference that stays in the directory" do
    result = resolve("/docs/guide/install.html")
    expect(result).to be_allowed
    expect(result.uri.to_s).to eq("http://example.com/docs/guide/install.html")
  end

  it "treats /docs and /docs-old as distinct directory segments" do
    result = resolve("../docs-old/x")
    expect(result).to be_rejected
    expect(result.reason).to eq(:path_outside_directory)
    # The reported URI is the one actually judged, not a string-prefix match.
    expect(result.resolved_uri.to_s).to eq("http://example.com/docs-old/x")
    expect(result.message).to include("directory boundary")
  end

  it "rejects an absolute path jumping to the sibling directory" do
    result = resolve("/docs-old/x")
    expect(result).to be_rejected
    expect(result.reason).to eq(:path_outside_directory)
    expect(result.resolved_uri.path).to eq("/docs-old/x")
  end

  it "rejects literal dot-segment traversal out of the directory" do
    result = resolve("../../etc/passwd")
    expect(result).to be_rejected
    expect(result.reason).to eq(:path_outside_directory)
    expect(result.resolved_uri.path).to eq("/etc/passwd")
  end

  it "rejects encoded dot segments that resolve out of the directory" do
    # %2E decodes to an unreserved "." at the component level, so after
    # resolution this is the dot segment "../etc" and escapes the boundary.
    result = resolve("%2e%2e/etc")
    expect(result).to be_rejected
    expect(result.reason).to eq(:path_outside_directory)
    expect(result.resolved_uri.path).to eq("/etc")
  end

  it "rejects uppercase encoded dot segments as well" do
    # Real slashes separate the segments; the encoded dots still decode to
    # dot segments that escape the boundary.
    result = resolve("%2E%2E/%2E%2E/etc")
    expect(result).to be_rejected
    expect(result.reason).to eq(:path_outside_directory)
    expect(result.resolved_uri.path).to eq("/etc")
  end

  it "does not treat an encoded slash as a path separator" do
    # "%2F" stays encoded inside a single segment that contains "..", so
    # nothing is resolved across a boundary and the reference stays put.
    result = resolve("a%2f..%2fetc")
    expect(result).to be_allowed
    expect(result.uri.path).to eq("/docs/a%2F..%2Fetc")
  end

  it "keeps an encoded slash inside the traversal segment without escaping" do
    result = resolve("%2e%2e%2fetc")
    expect(result).to be_allowed
    expect(result.uri.path).to eq("/docs/..%2Fetc")
  end

  it "allows the empty reference, resolving to the base URI" do
    result = resolve("")
    expect(result).to be_allowed
    expect(result.uri.to_s).to eq("http://example.com/docs/index.html")
  end

  it "allows a query-only reference to replace the base query" do
    result = resolve("?page=2")
    expect(result).to be_allowed
    expect(result.uri.to_s).to eq(
      "http://example.com/docs/index.html?page=2"
    )
  end

  it "allows a fragment-only reference" do
    result = resolve("#section")
    expect(result).to be_allowed
    expect(result.uri.to_s).to eq(
      "http://example.com/docs/index.html#section"
    )
  end

  it "rejects a //host network-path reference targeting another host" do
    result = resolve("//evil.example/x")
    expect(result).to be_rejected
    expect(result.reason).to eq(:origin_not_allowed)
    expect(result.resolved_uri.host).to eq("evil.example")
    expect(result.message).to include("allowed origins")
  end

  it "rejects an absolute http reference targeting another host" do
    result = resolve("http://evil.example/docs/x")
    expect(result).to be_rejected
    expect(result.reason).to eq(:origin_not_allowed)
  end

  it "rejects a different scheme even with the same host" do
    result = resolve("https://example.com/docs/x")
    expect(result).to be_rejected
    expect(result.reason).to eq(:origin_not_allowed)
  end

  it "rejects a non-default port even with the same host" do
    result = resolve("http://example.com:8080/docs/x")
    expect(result).to be_rejected
    expect(result.reason).to eq(:origin_not_allowed)
    expect(result.message).to include("example.com:8080")
  end

  it "allows an explicit default port as the same origin" do
    result = resolve("//example.com:80/docs/x")
    expect(result).to be_allowed
    expect(result.uri.to_s).to eq("http://example.com/docs/x")
  end

  it "allows an origin written with its default port" do
    result = resolve(
      "http://example.com/docs/x",
      allowed_origins: "http://example.com:80"
    )
    expect(result).to be_allowed
  end

  it "normalizes host case when comparing origins" do
    result = resolve("http://EXAMPLE.COM/docs/x")
    expect(result).to be_allowed
    expect(result.uri.host).to eq("example.com")
  end

  it "matches an IDNA origin against an ASCII allowed origin" do
    result = resolve("http://www.xn--8ws00zhy3a.com/docs/x")
    expect(result).to be_rejected
    unicode_result = resolve(
      "http://www.詹姆斯.com/docs/x",
      allowed_origins: "http://www.xn--8ws00zhy3a.com"
    )
    expect(unicode_result).to be_allowed
  end

  it "accepts multiple allowed origins" do
    result = resolve(
      "https://cdn.example.com/docs/x",
      allowed_origins: [
        "http://example.com", "https://cdn.example.com"
      ]
    )
    expect(result).to be_allowed
  end

  it "allows a reference that returns into the directory with dot segments" do
    result = resolve("../docs/guide/x")
    expect(result).to be_allowed
    expect(result.uri.path).to eq("/docs/guide/x")
  end

  it "allows exact equality with the directory boundary" do
    result = resolve("/docs")
    expect(result).to be_allowed
    expect(result.uri.path).to eq("/docs")
  end

  it "accepts a URI object as the directory boundary" do
    result = resolve("x", directory: Addressable::URI.parse("/docs/"))
    expect(result).to be_allowed
    expect(result.uri.path).to eq("/docs/x")
  end

  it "returns a frozen resolved URI" do
    expect(resolve("x").uri).to be_frozen
  end

  it "never has two interpretations for the reported value" do
    rejected = resolve("../docs-old/x")
    expect(rejected.to_s).to eq(rejected.resolved_uri.to_s)
    allowed = resolve("x")
    expect(allowed.to_s).to eq(allowed.uri.to_s)
  end

  it "inspects without impersonating a permitted URI" do
    rejected = resolve("../docs-old/x")
    expect(rejected.inspect).to include("allowed:false")
  end

  it "raises ArgumentError when the base URI is not absolute" do
    expect { resolve("x", base_uri: "/docs/index.html") }.to raise_error(
      ArgumentError, /absolute/
    )
  end

  it "raises ArgumentError when the directory is not absolute" do
    expect { resolve("x", directory: "docs") }.to raise_error(
      ArgumentError, /absolute path/
    )
  end

  it "raises TypeError when given nil arguments" do
    expect {
      Addressable::URI.constrained_resolve(nil, "x", origin, docs)
    }.to raise_error(TypeError)
  end

  it "accepts objects that respond to to_str" do
    base_struct = Struct.new(:s).new(base)
    expect(resolve(base_struct.s)).to be_allowed
  end
end

describe Addressable::URI, ".constrained_resolve!" do
  let(:base) { "http://example.com/docs/index.html" }

  it "returns the resolved URI when allowed" do
    uri = Addressable::URI.constrained_resolve!(
      base, "install.html", "http://example.com", "/docs"
    )
    expect(uri).to be_a(Addressable::URI)
    expect(uri.to_s).to eq("http://example.com/docs/install.html")
  end

  it "raises ConstraintViolation for a foreign origin" do
    expect {
      Addressable::URI.constrained_resolve!(
        base, "//evil.example/x", "http://example.com", "/docs"
      )
    }.to raise_error(
      Addressable::URI::ConstraintViolation, /origin/
    ) { |error|
      expect(error.reason).to eq(:origin_not_allowed)
      expect(error.resolved_uri.host).to eq("evil.example")
    }
  end

  it "raises ConstraintViolation for directory escape" do
    expect {
      Addressable::URI.constrained_resolve!(
        base, "../docs-old/x", "http://example.com", "/docs"
      )
    }.to raise_error(
      Addressable::URI::ConstraintViolation, /directory boundary/
    ) { |error|
      expect(error.reason).to eq(:path_outside_directory)
      expect(error.resolved_uri.path).to eq("/docs-old/x")
    }
  end

  it "is a subclass of InvalidURIError" do
    expect(Addressable::URI::ConstraintViolation.ancestors).to include(
      Addressable::URI::InvalidURIError
    )
  end
end
