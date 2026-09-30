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

require "addressable"

describe Addressable::URI::MountMap do
  def build_map
    described_class.new do |mounts|
      mounts.mount "http://service-a.internal:8080/",
                   "https://gateway.example.com/service-a/"
      mounts.mount "http://service-b.internal/",
                   "https://gateway.example.com/service-b/"
    end
  end

  describe "#mount" do
    it "should accept a block at construction" do
      map = build_map
      expect(map.size).to eq(2)
    end

    it "should return the registered base URIs" do
      map = build_map
      expect(map.internal_bases.map(&:to_s)).to eq(
        ["http://service-a.internal:8080/", "http://service-b.internal/"]
      )
      expect(map.public_bases.map(&:to_s)).to eq(
        [
          "https://gateway.example.com/service-a/",
          "https://gateway.example.com/service-b/"
        ]
      )
    end

    it "should enumerate the pairs in registration order" do
      pairs = build_map.map { |internal, public_uri|
        [internal.to_s, public_uri.to_s]
      }
      expect(pairs.length).to eq(2)
      expect(pairs.first).to eq(
        ["http://service-a.internal:8080/",
         "https://gateway.example.com/service-a/"]
      )
    end

    it "should accept URI objects as well as strings" do
      map = described_class.new
      map.mount(
        Addressable::URI.parse("http://a.internal/"),
        Addressable::URI.parse("https://g.example.com/a/")
      )
      expect(
        map.externalize("http://a.internal/x").to_s
      ).to eq("https://g.example.com/a/x")
    end

    it "should reject relative mount bases" do
      map = described_class.new
      expect {
        map.mount("/relative/", "https://g.example.com/r/")
      }.to raise_error(described_class::InvalidMountError)
      expect {
        map.mount("http://a.internal/", "/relative/")
      }.to raise_error(described_class::InvalidMountError)
    end

    it "should reject mount bases with a query or fragment" do
      map = described_class.new
      expect {
        map.mount("http://a.internal/?q=1", "https://g.example.com/a/")
      }.to raise_error(described_class::InvalidMountError)
      expect {
        map.mount("http://a.internal/#f", "https://g.example.com/a/")
      }.to raise_error(described_class::InvalidMountError)
      expect {
        map.mount("http://a.internal/", "https://g.example.com/a/?q=1")
      }.to raise_error(described_class::InvalidMountError)
    end

    it "should reject mount bases containing userinfo" do
      map = described_class.new
      expect {
        map.mount("http://bob@a.internal/", "https://g.example.com/a/")
      }.to raise_error(described_class::InvalidMountError)
    end
  end

  describe "#externalize" do
    it "should rewrite scheme, host, port and path prefix" do
      uri = build_map.externalize(
        "http://service-a.internal:8080/widgets/1"
      )
      expect(uri.to_s).to eq(
        "https://gateway.example.com/service-a/widgets/1"
      )
    end

    it "should preserve the query and fragment" do
      uri = build_map.externalize(
        "http://service-a.internal:8080/widgets/1?x=1&y=2#frag"
      )
      expect(uri.to_s).to eq(
        "https://gateway.example.com/service-a/widgets/1?x=1&y=2#frag"
      )
    end

    it "should match the base itself" do
      expect(
        build_map.externalize("http://service-a.internal:8080/").to_s
      ).to eq("https://gateway.example.com/service-a/")
    end

    it "should strip a default port on the internal side" do
      map = described_class.new do |mounts|
        mounts.mount "http://a.internal:80/", "https://g.example.com/a/"
      end
      expect(
        map.externalize("http://a.internal:80/x").to_s
      ).to eq("https://g.example.com/a/x")
    end

    it "should preserve an explicit non-default port" do
      map = described_class.new do |mounts|
        mounts.mount "http://a.internal:3000/",
                     "https://g.example.com/a/"
      end
      expect(
        map.externalize("http://a.internal:3000/x").to_s
      ).to eq("https://g.example.com/a/x")
      expect(
        map.internalize("https://g.example.com/a/x").to_s
      ).to eq("http://a.internal:3000/x")
    end

    it "should canonicalize percent encoding" do
      uri = build_map.externalize(
        "http://service-b.internal/foo%7ebar"
      )
      expect(uri.to_s).to eq(
        "https://gateway.example.com/service-b/foo~bar"
      )
    end

    it "should preserve encoded slash characters within a segment" do
      uri = build_map.externalize(
        "http://service-b.internal/a%2Fb/c"
      )
      expect(uri.path).to eq("/service-b/a%2Fb/c")
    end

    it "should normalize IDN hosts to ASCII" do
      map = described_class.new do |mounts|
        mounts.mount "http://xn--bcher-kva.example/",
                     "https://g.example.com/b/"
      end
      expect(
        map.externalize("http://bücher.example/x").to_s
      ).to eq("https://g.example.com/b/x")
    end

    it "should preserve a trailing slash on a directory" do
      expect(
        build_map.externalize("http://service-a.internal:8080/dir/").to_s
      ).to eq("https://gateway.example.com/service-a/dir/")
    end

    it "should raise MountNotFoundError when nothing matches" do
      expect {
        build_map.externalize("http://other.internal/x")
      }.to raise_error(described_class::MountNotFoundError)
    end

    it "should not match a different scheme or port on the same host" do
      expect {
        build_map.externalize("https://service-a.internal:8080/x")
      }.to raise_error(described_class::MountNotFoundError)
      expect {
        build_map.externalize("http://service-a.internal:9000/x")
      }.to raise_error(described_class::MountNotFoundError)
    end

    it "should refuse to carry userinfo across" do
      expect {
        build_map.externalize("http://bob@service-a.internal:8080/x")
      }.to raise_error(described_class::UserinfoError)
    end

    it "should return an Addressable::URI" do
      expect(
        build_map.externalize("http://service-a.internal:8080/")
      ).to be_kind_of(Addressable::URI)
    end
  end

  describe "#internalize" do
    it "should rewrite scheme, host, port and path prefix" do
      uri = build_map.internalize(
        "https://gateway.example.com/service-a/widgets/1"
      )
      expect(uri.to_s).to eq(
        "http://service-a.internal:8080/widgets/1"
      )
    end

    it "should preserve the query and fragment" do
      uri = build_map.internalize(
        "https://gateway.example.com/service-a/w?a=1#top"
      )
      expect(uri.to_s).to eq(
        "http://service-a.internal:8080/w?a=1#top"
      )
    end

    it "should canonicalize encoding in the query and fragment" do
      uri = build_map.internalize(
        "https://gateway.example.com/service-a/w?z=%7E#f%7E"
      )
      expect(uri.to_s).to eq(
        "http://service-a.internal:8080/w?z=~#f~"
      )
    end

    it "should match the base itself" do
      expect(
        build_map.internalize(
          "https://gateway.example.com/service-a/"
        ).to_s
      ).to eq("http://service-a.internal:8080/")
    end

    it "should not cross segment boundaries when matching a prefix" do
      # "/app" must not match the "/appliance" mount or vice versa; the
      # remainder has to begin at a segment boundary.
      map = described_class.new do |mounts|
        mounts.mount "http://a.internal/",
                     "https://gateway.example.com/app/"
        mounts.mount "http://b.internal/",
                     "https://gateway.example.com/appliance/"
      end
      expect(
        map.internalize("https://gateway.example.com/app/x").to_s
      ).to eq("http://a.internal/x")
      expect(
        map.internalize("https://gateway.example.com/appliance/y").to_s
      ).to eq("http://b.internal/y")
      expect {
        map.internalize("https://gateway.example.com/appliance")
      }.to raise_error(described_class::MountNotFoundError)
      expect {
        map.internalize("https://gateway.example.com/app")
      }.to raise_error(described_class::MountNotFoundError)
    end

    it "should treat a base without a trailing slash as a file mount " +
        "that does not match paths beneath it" do
      map = described_class.new do |mounts|
        mounts.mount "http://a.internal/app",
                     "https://gateway.example.com/app"
      end
      expect(
        map.internalize("https://gateway.example.com/app").to_s
      ).to eq("http://a.internal/app")
      expect {
        map.internalize("https://gateway.example.com/app/x")
      }.to raise_error(described_class::MountNotFoundError)
    end

    it "should raise MountNotFoundError when nothing matches" do
      expect {
        build_map.internalize("https://gateway.example.com/nope/x")
      }.to raise_error(described_class::MountNotFoundError)
    end

    it "should refuse to carry userinfo across" do
      expect {
        build_map.internalize(
          "https://bob@gateway.example.com/service-a/x"
        )
      }.to raise_error(described_class::UserinfoError)
    end
  end

  describe "longest prefix matching" do
    let(:map) do
      described_class.new do |mounts|
        mounts.mount "http://h.internal/",
                     "https://g.example.com/"
        mounts.mount "http://h.internal/api/v1/",
                     "https://g.example.com/api/v1/"
        mounts.mount "http://h.internal/api/v2/",
                     "https://g.example.com/api/v2/"
      end
    end

    it "should prefer the most specific mount" do
      expect(
        map.externalize("http://h.internal/api/v2/widgets").to_s
      ).to eq("https://g.example.com/api/v2/widgets")
      expect(
        map.internalize("https://g.example.com/api/v2/widgets").to_s
      ).to eq("http://h.internal/api/v2/widgets")
    end

    it "should fall back to the outer mount outside the nested prefix" do
      expect(
        map.externalize("http://h.internal/home").to_s
      ).to eq("https://g.example.com/home")
    end

    it "should not treat a partial segment as a nested prefix" do
      # "/api/v2beta" is a single segment alongside "/api/v2", not a
      # path beneath it; it is served by the root mount instead.
      expect(
        map.externalize("http://h.internal/api/v2beta/x").to_s
      ).to eq("https://g.example.com/api/v2beta/x")
    end
  end

  describe "stable round trips" do
    let(:map) do
      described_class.new do |mounts|
        mounts.mount "http://service-a.internal:8080/",
                     "https://gateway.example.com/service-a/"
        mounts.mount "http://service-a.internal:8080/api/",
                     "https://gateway.example.com/service-a/api/"
        mounts.mount "http://service-b.internal/",
                     "https://gateway.example.com/service-b/"
      end
    end

    [
      "http://service-a.internal:8080/",
      "http://service-a.internal:8080/widgets/1",
      "http://service-a.internal:8080/widgets/1?x=1#f",
      "http://service-a.internal:8080/api/items",
      "http://service-a.internal:8080/dir/",
      "http://service-b.internal/x/y?z"
    ].each do |internal|
      it "round-trips #{internal}" do
        public_uri = map.externalize(internal)
        expect(map.internalize(public_uri).to_s).to eq(
          Addressable::URI.parse(internal).normalize.to_s
        )
      end
    end

    [
      "https://gateway.example.com/service-a/",
      "https://gateway.example.com/service-a/widgets/1",
      "https://gateway.example.com/service-a/api/items",
      "https://gateway.example.com/service-b/x?z"
    ].each do |public_uri|
      it "round-trips #{public_uri} in reverse" do
        internal = map.internalize(public_uri)
        expect(map.externalize(internal).to_s).to eq(
          Addressable::URI.parse(public_uri).normalize.to_s
        )
      end
    end

    it "should not round-trip links beneath no mounted prefix" do
      expect {
        map.internalize("https://gateway.example.com/elsewhere/x")
      }.to raise_error(described_class::MountNotFoundError)
    end
  end

  describe "ambiguous configuration" do
    it "should reject two internal addresses on the same public prefix" do
      map = described_class.new
      map.mount "http://a.internal/", "https://g.example.com/a/"
      expect {
        map.mount "http://b.internal/", "https://g.example.com/a/"
      }.to raise_error(described_class::AmbiguousMountError)
    end

    it "should reject mounting one internal prefix at two public prefixes" do
      map = described_class.new
      map.mount "http://a.internal/", "https://g.example.com/a/"
      expect {
        map.mount "http://a.internal/", "https://g.example.com/b/"
      }.to raise_error(described_class::AmbiguousMountError)
    end

    it "should treat the same mount written with a default port and " +
        "encoding differences as the same prefix" do
      map = described_class.new
      map.mount "http://a.internal:80/foo%7e/",
                "https://g.example.com/a/"
      expect {
        map.mount "http://a.internal/foo~/",
                  "https://g.example.com/b/"
      }.to raise_error(described_class::AmbiguousMountError)
    end

    it "should allow a file mount and a directory mount sharing a name " +
        "when both sides mirror the distinction" do
      map = described_class.new do |mounts|
        mounts.mount "http://a.internal/app",
                     "https://g.example.com/app"
        mounts.mount "http://a.internal/app/",
                     "https://g.example.com/app/"
      end
      expect(
        map.internalize("https://g.example.com/app").to_s
      ).to eq("http://a.internal/app")
      expect(
        map.internalize("https://g.example.com/app/").to_s
      ).to eq("http://a.internal/app/")
      expect(
        map.internalize("https://g.example.com/app/x").to_s
      ).to eq("http://a.internal/app/x")
    end

    it "should allow adjacent directory names on the same public origin" do
      map = described_class.new do |mounts|
        mounts.mount "http://a.internal/",
                     "https://g.example.com/app/"
        mounts.mount "http://b.internal/",
                     "https://g.example.com/appliance/"
      end
      expect(map.size).to eq(2)
      expect(
        map.internalize("https://g.example.com/app/1").to_s
      ).to eq("http://a.internal/1")
      expect(
        map.internalize("https://g.example.com/appliance/1").to_s
      ).to eq("http://b.internal/1")
    end

    it "should reject nested mounts whose public layout differs from " +
        "the internal layout" do
      map = described_class.new
      map.mount "http://h.internal/", "https://g.example.com/a/"
      expect {
        map.mount "http://h.internal/api/",
                  "https://g.example.com/a/b/"
      }.to raise_error(described_class::AmbiguousMountError)
    end

    it "should accept nested mounts whose layout mirrors each side" do
      map = described_class.new do |mounts|
        mounts.mount "http://h.internal/",
                     "https://g.example.com/"
        mounts.mount "http://h.internal/api/",
                     "https://g.example.com/api/"
      end
      expect(
        map.externalize("http://h.internal/api/x").to_s
      ).to eq("https://g.example.com/api/x")
      expect(
        map.internalize("https://g.example.com/api/x").to_s
      ).to eq("http://h.internal/api/x")
    end

    it "should reject nested mounts that only nest on one side" do
      map = described_class.new
      map.mount "http://h.internal/api/",
                "https://g.example.com/a/"
      expect {
        map.mount "http://h.internal/",
                  "https://g.example.com/c/"
      }.to raise_error(described_class::AmbiguousMountError)
    end

    it "should reject mounts nested on the public side at different " +
        "internal origins" do
      map = described_class.new
      map.mount "http://a.internal/",
                "https://g.example.com/x/"
      expect {
        map.mount "http://b.internal/y/",
                  "https://g.example.com/x/y/"
      }.to raise_error(described_class::AmbiguousMountError)
    end

    it "should reject a public subtree split across two unrelated " +
        "internal origins" do
      map = described_class.new
      map.mount "http://a.internal/",
                "https://g.example.com/x/"
      expect {
        map.mount "http://b.internal/",
                  "https://g.example.com/x/y/"
      }.to raise_error(described_class::AmbiguousMountError)
    end
  end

  describe "#externalizable? and #internalizable?" do
    let(:map) { build_map }

    it "should report matching prefixes" do
      expect(
        map.externalizable?("http://service-a.internal:8080/x")
      ).to be(true)
      expect(
        map.internalizable?("https://gateway.example.com/service-a/x")
      ).to be(true)
    end

    it "should report non-matching prefixes" do
      expect(map.externalizable?("http://other.internal/x")).to be(false)
      expect(
        map.internalizable?("https://gateway.example.com/nope/x")
      ).to be(false)
    end

    it "should report false for URIs with userinfo" do
      expect(
        map.externalizable?("http://bob@service-a.internal:8080/x")
      ).to be(false)
    end
  end
end
