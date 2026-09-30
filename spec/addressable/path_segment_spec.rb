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

describe Addressable::URI::PathSegment do
  it "should parse a segment name without parameters" do
    segment = Addressable::URI::PathSegment.parse("books")
    expect(segment.name).to eq("books")
    expect(segment.params).to eq([])
  end

  it "should parse matrix parameters in order" do
    segment = Addressable::URI::PathSegment.parse("books;lang=zh;rev=3")
    expect(segment.name).to eq("books")
    expect(segment.params).to eq([["lang", "zh"], ["rev", "3"]])
    expect(segment["lang"]).to eq("zh")
    expect(segment["rev"]).to eq("3")
  end

  it "should preserve repeated parameter names in order" do
    segment = Addressable::URI::PathSegment.parse("item;tag=a;tag=b;tag=c")
    expect(segment.values("tag")).to eq(["a", "b", "c"])
    expect(segment["tag"]).to eq("a")
    expect(segment.to_s).to eq("item;tag=a;tag=b;tag=c")
  end

  it "should distinguish a missing equals sign from an empty value" do
    segment = Addressable::URI::PathSegment.parse("x;flag;empty=")
    expect(segment.key?("flag")).to eq(true)
    expect(segment["flag"]).to be_nil
    expect(segment.key?("empty")).to eq(true)
    expect(segment["empty"]).to eq("")
    expect(segment.key?("missing")).to eq(false)
    expect(segment["missing"]).to be_nil
    expect(segment.to_s).to eq("x;flag;empty=")
  end

  it "should treat encoded semicolons and slashes as data" do
    segment = Addressable::URI::PathSegment.parse("a%3Bb%2Fc;x=%3B%2F%3D")
    expect(segment.name).to eq("a;b/c")
    expect(segment.params).to eq([["x", ";/="]])
    expect(segment.to_s).to eq("a%3Bb%2Fc;x=%3B%2F=")
  end

  it "should encode structural delimiters when serializing" do
    segment = Addressable::URI::PathSegment.new("a;b", [["k=1", "x;y"]])
    expect(segment.to_s).to eq("a%3Bb;k%3D1=x%3By")
    reparsed = Addressable::URI::PathSegment.parse(segment.to_s)
    expect(reparsed).to eq(segment)
  end

  it "should replace the first occurrence and drop duplicates on []=" do
    segment = Addressable::URI::PathSegment.parse("item;tag=a;keep=1;tag=b")
    segment["tag"] = "z"
    expect(segment.params).to eq([["tag", "z"], ["keep", "1"]])
  end

  it "should append new parameters on []= and add_param" do
    segment = Addressable::URI::PathSegment.new("item")
    segment["lang"] = "zh"
    segment.add_param("lang", "en")
    segment.add_param("archived")
    expect(segment.params).to eq(
      [["lang", "zh"], ["lang", "en"], ["archived", nil]]
    )
    expect(segment.to_s).to eq("item;lang=zh;lang=en;archived")
  end

  it "should delete all parameters with a given name" do
    segment = Addressable::URI::PathSegment.parse("item;tag=a;keep=1;tag=b")
    segment.delete_param("tag")
    expect(segment.params).to eq([["keep", "1"]])
  end

  it "should not share parameter state between copies" do
    segment = Addressable::URI::PathSegment.parse("item;tag=a")
    copy = segment.dup
    copy["tag"] = "b"
    expect(segment["tag"]).to eq("a")
    expect(copy["tag"]).to eq("b")
  end
end

describe Addressable::URI, "path segments" do
  it "should expose structured segments for /books;lang=zh/42;rev=3" do
    uri = Addressable::URI.parse("http://example.com/books;lang=zh/42;rev=3")
    segments = uri.path_segments
    expect(segments.map(&:name)).to eq(["", "books", "42"])
    expect(segments[1].params).to eq([["lang", "zh"]])
    expect(segments[2].params).to eq([["rev", "3"]])
  end

  it "should round-trip segments back to the identical path" do
    path = "/books;lang=zh;lang=en/42;rev=3;flag;empty="
    uri = Addressable::URI.parse("http://example.com#{path}")
    uri.path_segments = uri.path_segments
    expect(uri.path).to eq(path)
  end

  it "should not split segments on encoded slashes or semicolons" do
    uri = Addressable::URI.parse("http://example.com/a%3Bb/c%2Fd;e=%3B")
    segments = uri.path_segments
    expect(segments.length).to eq(3)
    expect(segments[1].name).to eq("a;b")
    expect(segments[1].params).to eq([])
    expect(segments[2].name).to eq("c/d")
    expect(segments[2]["e"]).to eq(";")
  end

  it "should modify one segment without affecting other segments " \
     "that use the same parameter name" do
    uri = Addressable::URI.parse(
      "http://example.com/books;lang=zh/42;lang=en;rev=3"
    )
    segments = uri.path_segments
    segments[1]["lang"] = "fr"
    uri.path_segments = segments
    expect(uri.path).to eq("/books;lang=fr/42;lang=en;rev=3")
    # The parameter with the same name on the other segment, and its
    # ownership, are unchanged.
    expect(uri.path_segments[2].params).to eq([["lang", "en"], ["rev", "3"]])
    expect(uri.path_segments[2].name).to eq("42")
  end

  it "should not modify the URI until segments are assigned back" do
    uri = Addressable::URI.parse("http://example.com/books;lang=zh/42")
    segments = uri.path_segments
    segments[1]["lang"] = "fr"
    expect(uri.path).to eq("/books;lang=zh/42")
  end

  it "should parse segments of relative references" do
    uri = Addressable::URI.parse("books;lang=zh/42;rev=3")
    segments = uri.path_segments
    expect(segments.map(&:name)).to eq(["books", "42"])
    expect(segments[0]["lang"]).to eq("zh")
    expect(segments[1]["rev"]).to eq("3")
  end

  it "should keep matrix parameters attached to their segments " \
     "when merging relative references" do
    base = Addressable::URI.parse(
      "http://example.com/books;lang=zh/42;rev=3"
    )
    joined = base.join("43;rev=4")
    expect(joined.path).to eq("/books;lang=zh/43;rev=4")
    segments = joined.path_segments
    expect(segments[1].params).to eq([["lang", "zh"]])
    expect(segments[2].params).to eq([["rev", "4"]])
  end

  it "should remove a segment's parameters along with the segment " \
     "when a relative reference traverses upwards" do
    base = Addressable::URI.parse(
      "http://example.com/books;lang=zh/42;rev=3"
    )
    joined = base.join("../43;rev=4")
    expect(joined.path).to eq("/43;rev=4")
  end

  it "should keep matrix parameters attached to their segments " \
     "during dot-segment normalization" do
    uri = Addressable::URI.parse(
      "http://example.com/a;p=1/./b;x=1/../c;lang=zh"
    )
    uri.normalize!
    expect(uri.path).to eq("/a;p=1/c;lang=zh")
    segments = uri.path_segments
    expect(segments[1].params).to eq([["p", "1"]])
    expect(segments[2].params).to eq([["lang", "zh"]])
  end

  it "should not treat '..;p=1' as a dot-segment during normalization" do
    uri = Addressable::URI.parse("http://example.com/a/..;p=1/b")
    uri.normalize!
    expect(uri.path).to eq("/a/..;p=1/b")
  end

  it "should keep parameters with their segments when normalizing " \
     "joined paths" do
    base = Addressable::URI.parse("http://example.com/a;p=1/b/../c;lang=zh")
    joined = base.join("x;rev=9")
    expect(joined.path).to eq("/a;p=1/x;rev=9")
  end
end
