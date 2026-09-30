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

describe Addressable::URI::MountMap do
  def mount_map(mappings, options = {})
    described_class.new(mappings, options)
  end

  def round_trip(map, internal_uri)
    public_uri = map.apply(internal_uri)
    back = map.unapply(public_uri)
    [public_uri, back]
  end

  let(:gateway) do
    mount_map(
      "http://srv-a.internal/"      => "https://gateway.example/api/",
      "http://srv-a.internal/admin" => "https://gateway.example/api/admin",
      "http://srv-b.internal/v1"    => "https://gateway.example/beta"
    )
  end

  describe "construction" do
    it "is empty by default" do
      expect(described_class.new.mounts).to eq([])
    end

    it "accepts a hash of internal => public mount points" do
      map = mount_map("http://a.internal/" => "https://gw.example/api/")
      expect(map.mounts.size).to eq(1)
      expect(map.mounts.first.internal.to_s)
        .to eq("http://a.internal/")
      expect(map.mounts.first.external.to_s)
        .to eq("https://gw.example/api")
    end

    it "accepts the inverse direction" do
      map = mount_map(
        { "https://gw.example/api/" => "http://a.internal/" },
        :direction => :public
      )
      expect(map.apply("http://a.internal/x").to_s)
        .to eq("https://gw.example/api/x")
    end

    it "rejects an invalid direction" do
      expect { described_class.new({}, :direction => :sideways) }
        .to raise_error(ArgumentError, /Invalid direction/)
    end

    it "supports incremental registration with #mount" do
      map = described_class.new
      map.mount("http://a.internal/", "https://gw.example/api/")
      expect(map.apply("http://a.internal/x").to_s)
        .to eq("https://gw.example/api/x")
    end

    it "ignores exact duplicate pairings" do
      map = described_class.new
      map.mount("http://a.internal/x", "https://gw.example/p")
      map.mount("http://a.internal/x/", "https://gw.example/p/")
      expect(map.mounts.size).to eq(1)
    end
  end

  describe "mount point validation" do
    it "rejects relative URIs" do
      expect { described_class.new("a/x" => "https://gw.example/") }
        .to raise_error(described_class::InvalidMountError, /absolute/)
    end

    it "rejects URIs without a host" do
      expect { described_class.new("http:///" => "https://gw.example/") }
        .to raise_error(described_class::InvalidMountError, /host-based/)
    end

    it "rejects user information on mount points" do
      expect do
        described_class.new("http://bob@a.internal/" => "https://gw.example/")
      end.to raise_error(described_class::InvalidMountError, /user information/)
    end

    it "rejects passwords on mount points" do
      expect do
        described_class.new(
          "http://bob:secret@a.internal/" => "https://gw.example/"
        )
      end.to raise_error(described_class::InvalidMountError, /user information/)
    end

    it "rejects queries on mount points" do
      expect do
        described_class.new("http://a.internal/?x=1" => "https://gw.example/")
      end.to raise_error(described_class::InvalidMountError, /query/)
    end

    it "rejects fragments on mount points" do
      expect do
        described_class.new("http://a.internal/#frag" => "https://gw.example/")
      end.to raise_error(described_class::InvalidMountError, /fragment/)
    end

    it "rejects dot segments in mount paths" do
      expect do
        described_class.new("http://a.internal/./x" => "https://gw.example/")
      end.to raise_error(described_class::InvalidMountError, /dot or empty/)
      expect do
        described_class.new("http://a.internal/x/../y" => "https://gw.example/")
      end.to raise_error(described_class::InvalidMountError, /dot or empty/)
    end

    it "rejects double slashes in mount paths" do
      expect do
        described_class.new("http://a.internal//x" => "https://gw.example/")
      end.to raise_error(described_class::InvalidMountError, /dot or empty/)
    end
  end

  describe "#apply" do
    it "replaces scheme, host, port and mount path prefix" do
      expect(gateway.apply("http://srv-a.internal/widgets/42").to_s)
        .to eq("https://gateway.example/api/widgets/42")
    end

    it "preserves the remainder of the path" do
      expect(gateway.apply("http://srv-b.internal/v1/deep/path/to/thing").to_s)
        .to eq("https://gateway.example/beta/deep/path/to/thing")
    end

    it "preserves query and fragment" do
      uri = "http://srv-a.internal/widgets/42?a=1&b=2#section-3"
      expect(gateway.apply(uri).to_s)
        .to eq("https://gateway.example/api/widgets/42?a=1&b=2#section-3")
    end

    it "maps the mount point itself" do
      expect(gateway.apply("http://srv-b.internal/v1").to_s)
        .to eq("https://gateway.example/beta")
    end

    it "maps a root mount point" do
      expect(gateway.apply("http://srv-a.internal/").to_s)
        .to eq("https://gateway.example/api")
    end

    it "returns an Addressable::URI" do
      result = gateway.apply("http://srv-a.internal/x")
      expect(result).to be_kind_of(Addressable::URI)
    end

    it "accepts an Addressable::URI" do
      result = gateway.apply(Addressable::URI.parse("http://srv-a.internal/x"))
      expect(result.to_s).to eq("https://gateway.example/api/x")
    end

    it "raises when nothing matches" do
      expect { gateway.apply("http://unknown.internal/x") }
        .to raise_error(described_class::NoMountError, /No mount point/)
    end

    it "rejects relative URIs" do
      expect { gateway.apply("/x") }
        .to raise_error(described_class::InvalidMappingURLError, /relative/)
    end

    it "rejects non-URI objects" do
      expect { gateway.apply(123) }
        .to raise_error(described_class::InvalidMappingURLError)
    end
  end

  describe "longest-prefix matching on complete components" do
    it "prefers the longer nested mount point" do
      expect(gateway.apply("http://srv-a.internal/admin/users").to_s)
        .to eq("https://gateway.example/api/admin/users")
    end

    it "does not match a directory prefix across a segment boundary" do
      # "/admin" is nested, but "/administration" only matches the root mount.
      expect(gateway.apply("http://srv-a.internal/administration").to_s)
        .to eq("https://gateway.example/api/administration")
    end

    it "does not match a directory prefix when no parent mount exists" do
      map = mount_map("http://a.internal/admin" => "https://gw.example/admin")
      expect { map.apply("http://a.internal/administration") }
        .to raise_error(described_class::NoMountError)
    end

    it "respects segment boundaries for adjacent directories" do
      map = mount_map(
        "http://a.internal/app"   => "https://gw.example/api",
        "http://a.internal/appv2" => "https://gw.example/apiv2"
      )
      expect(map.apply("http://a.internal/app/x").to_s)
        .to eq("https://gw.example/api/x")
      expect(map.apply("http://a.internal/appv2/x").to_s)
        .to eq("https://gw.example/apiv2/x")
      expect(map.apply("http://a.internal/app").to_s)
        .to eq("https://gw.example/api")
      expect { map.apply("http://a.internal/apple") }
        .to raise_error(described_class::NoMountError)
      expect { map.apply("http://a.internal/appv2x") }
        .to raise_error(described_class::NoMountError)
    end

    it "does not match a different host even with the same path" do
      map = mount_map("http://a.internal/x" => "https://gw.example/p")
      expect { map.apply("http://b.internal/x/y") }
        .to raise_error(described_class::NoMountError)
    end

    it "does not match a different scheme" do
      map = mount_map("http://a.internal/x" => "https://gw.example/p")
      expect { map.apply("https://a.internal/x/y") }
        .to raise_error(described_class::NoMountError)
    end
  end

  describe "default ports and encoding normalization" do
    let(:map) do
      mount_map("http://a.internal:80/" => "https://gw.example:443/root/")
    end

    it "matches the default port explicitly or omitted" do
      expect(map.apply("http://a.internal:80/x").to_s)
        .to eq("https://gw.example/root/x")
      expect(map.apply("http://a.internal/x").to_s)
        .to eq("https://gw.example/root/x")
    end

    it "does not match a non-default port" do
      expect { map.apply("http://a.internal:8080/x") }
        .to raise_error(described_class::NoMountError)
    end

    it "omits default ports in the result" do
      expect(map.apply("http://a.internal/x").to_s)
        .to eq("https://gw.example/root/x")
    end

    it "reverses default ports too" do
      expect(map.unapply("https://gw.example:443/root/x").to_s)
        .to eq("http://a.internal/x")
    end

    it "unifies equivalent percent-encoding forms" do
      expect(map.apply("http://a.internal/a%2Eb?b=%32").to_s)
        .to eq("https://gw.example/root/a.b?b=2")
    end

    it "normalizes encoded slashes consistently per component" do
      # A slash stays encoded inside a path segment, while an encoded slash in
      # a query is a sub-delimiter-safe character and gets unencoded by
      # Addressable's standard normalization.
      expect(map.apply("http://a.internal/a%2Fb?q=%2F").to_s)
        .to eq("https://gw.example/root/a%2Fb?q=/")
    end

    it "round-trips through encoding normalization" do
      public_uri, back = round_trip(map, "http://a.internal/a%7Eb?b=%32")
      expect(public_uri.to_s).to eq("https://gw.example/root/a~b?b=2")
      expect(back.to_s).to eq("http://a.internal/a~b?b=2")
    end

    it "matches on normalized IDN hosts" do
      map = mount_map("http://bücher.example/x" => "https://gw.example/b")
      result = map.apply("http://b%C3%BCcher.example/x/page")
      expect(result.to_s).to eq("https://gw.example/b/page")
    end
  end

  describe "user information isolation" do
    let(:map) { mount_map("http://a.internal/" => "https://gw.example/api/") }

    it "refuses to map an internal URI with a user" do
      expect { map.apply("http://bob@a.internal/x") }
        .to raise_error(described_class::InvalidMappingURLError, /user information/)
    end

    it "refuses to map an internal URI with a password" do
      expect { map.apply("http://bob:hunter2@a.internal/x") }
        .to raise_error(described_class::InvalidMappingURLError, /user information/)
    end

    it "refuses to map a public URI with credentials" do
      expect { map.unapply("https://bob@gw.example/api/x") }
        .to raise_error(described_class::InvalidMappingURLError, /user information/)
    end

    it "never emits user information" do
      result = map.apply("http://a.internal/x")
      expect(result.user).to be_nil
      expect(result.password).to be_nil
    end
  end

  describe "#unapply" do
    it "reverses a simple mapping" do
      expect(gateway.unapply("https://gateway.example/api/widgets").to_s)
        .to eq("http://srv-a.internal/widgets")
    end

    it "reverses a non-root mount preserving the suffix" do
      expect(gateway.unapply("https://gateway.example/beta/a/b/c").to_s)
        .to eq("http://srv-b.internal/v1/a/b/c")
    end

    it "preserves query and fragment" do
      expect(
        gateway.unapply("https://gateway.example/api/x?z=9#top").to_s
      ).to eq("http://srv-a.internal/x?z=9#top")
    end

    it "raises when nothing matches" do
      expect { gateway.unapply("https://elsewhere.example/x") }
        .to raise_error(described_class::NoMountError)
    end
  end

  describe "round-trip stability" do
    def expect_stable(map, internal_uri)
      public_uri = map.apply(internal_uri)
      back = map.unapply(public_uri)
      expect(back.to_s).to eq(Addressable::URI.parse(internal_uri).normalize.to_s),
        "expected #{internal_uri.inspect} to round-trip via #{public_uri}"
      again = map.apply(back)
      expect(again.to_s).to eq(public_uri.to_s)
    end

    it "round-trips links under a root mount" do
      map = mount_map("http://a.internal/" => "https://gw.example/api/")
      %w[
        http://a.internal/
        http://a.internal/widgets
        http://a.internal/widgets/42
        http://a.internal/a/b/c/d?x=1&y=2#frag
      ].each { |uri| expect_stable(map, uri) }
    end

    it "round-trips links under nested mounts" do
      %w[
        http://srv-a.internal/
        http://srv-a.internal/widgets/42
        http://srv-a.internal/admin
        http://srv-a.internal/admin/users/7
        http://srv-b.internal/v1
        http://srv-b.internal/v1/orders/9/items
      ].each { |uri| expect_stable(gateway, uri) }
    end

    it "round-trips adjacent directory mounts without bleeding across" do
      map = mount_map(
        "http://a.internal/app"   => "https://gw.example/api",
        "http://a.internal/appv2" => "https://gw.example/apiv2",
        "http://a.internal/other" => "https://gw.example/other"
      )
      %w[
        http://a.internal/app
        http://a.internal/app/login
        http://a.internal/appv2
        http://a.internal/appv2/deploy/3
        http://a.internal/other/thing
      ].each { |uri| expect_stable(map, uri) }
    end

    it "round-trips deeply nested, consistently ordered mounts" do
      map = mount_map(
        "http://a.internal/"      => "https://gw.example/",
        "http://a.internal/a"     => "https://gw.example/a",
        "http://a.internal/a/b"   => "https://gw.example/a/b",
        "http://a.internal/a/b/c" => "https://gw.example/a/b/c"
      )
      %w[
        http://a.internal/x
        http://a.internal/a/x
        http://a.internal/a/b/x
        http://a.internal/a/b/c/x
        http://a.internal/a/b/c
      ].each { |uri| expect_stable(map, uri) }
    end

    it "round-trips trailing-slash directory links" do
      map = mount_map("http://a.internal/svc" => "https://gw.example/api")
      expect_stable(map, "http://a.internal/svc/")
      expect_stable(map, "http://a.internal/svc/dir/")
    end
  end

  describe "reverse ambiguity" do
    it "rejects two internal addresses sharing one public prefix at config time" do
      expect do
        mount_map(
          "http://a.internal/svc1" => "https://gw.example/shared",
          "http://b.internal/svc2" => "https://gw.example/shared"
        )
      end.to raise_error(
        described_class::AmbiguousMountError, /share the external prefix/
      )
    end

    it "rejects two public prefixes pointing at one internal prefix" do
      # Use #mount directly since a Hash cannot hold duplicate keys.
      expect do
        map = described_class.new
        map.mount("http://a.internal/shared", "https://gw.example/p1")
        map.mount("http://a.internal/shared", "https://gw.example/p2")
        map.validate!
      end.to raise_error(
        described_class::AmbiguousMountError, /share the internal prefix/
      )
    end

    it "treats trailing slashes as the same prefix when detecting ambiguity" do
      expect do
        mount_map(
          "http://a.internal/svc1" => "https://gw.example/shared",
          "http://b.internal/svc2" => "https://gw.example/shared/"
        )
      end.to raise_error(described_class::AmbiguousMountError)
    end

    it "rejects nested mounts that nest inconsistently on the other side" do
      expect do
        mount_map(
          "http://a.internal/"      => "https://gw.example/api/",
          "http://a.internal/admin" => "https://gw.example/other"
        )
      end.to raise_error(
        described_class::AmbiguousMountError, /Inconsistent mount nesting/
      )
    end

    it "rejects the reverse case of inconsistently nested mounts" do
      expect do
        mount_map(
          "http://a.internal/other" => "https://gw.example/",
          "http://a.internal/"      => "https://gw.example/admin"
        )
      end.to raise_error(
        described_class::AmbiguousMountError, /Inconsistent mount nesting/
      )
    end

    it "allows disjoint mount trees" do
      expect do
        mount_map(
          "http://a.internal/a" => "https://gw.example/x",
          "http://a.internal/b" => "https://gw.example/y"
        )
      end.not_to raise_error
    end

    it "names both mounts in the error message" do
      error = begin
        mount_map(
          "http://a.internal/svc1" => "https://gw.example/shared",
          "http://b.internal/svc2" => "https://gw.example/shared"
        )
        nil
      rescue described_class::AmbiguousMountError => e
        e
      end
      expect(error.message).to include("http://a.internal/svc1")
      expect(error.message).to include("http://b.internal/svc2")
    end

    it "raises at reverse time if ambiguity somehow remains" do
      # Two mounts with distinct internal prefixes that nevertheless share a
      # public prefix are blocked at construction; construct the object and
      # tamper the registry to prove mapping never just picks the first.
      map = described_class.new
      map.mounts << described_class::Mount.new(
        Addressable::URI.parse("http://a.internal/x").normalize,
        Addressable::URI.parse("https://gw.example/p").normalize
      )
      map.mounts << described_class::Mount.new(
        Addressable::URI.parse("http://b.internal/x").normalize,
        Addressable::URI.parse("https://gw.example/p").normalize
      )
      expect { map.unapply("https://gw.example/p/y") }
        .to raise_error(described_class::AmbiguousMountError, /Multiple mount/)
    end
  end
end
