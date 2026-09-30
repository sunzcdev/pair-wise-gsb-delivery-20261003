# frozen_string_literal: true

require 'spec_helper'

require 'addressable/uri'

describe Addressable::URI::PathSegment do
  it "parses a bare segment name" do
    segment = described_class.parse("books")
    expect(segment.name).to eq("books")
    expect(segment.matrix_params).to eq([])
  end

  it "parses matrix parameters in order" do
    segment = described_class.parse("books;lang=zh;rev=3")
    expect(segment.name).to eq("books")
    expect(segment.matrix_params).to eq([["lang", "zh"], ["rev", "3"]])
  end

  it "preserves repeated parameter names" do
    segment = described_class.parse("a;x=1;x=2")
    expect(segment.matrix_params).to eq([["x", "1"], ["x", "2"]])
    expect(segment.values("x")).to eq(["1", "2"])
    expect(segment["x"]).to eq("1")
  end

  it "distinguishes a missing equals sign from an empty value" do
    segment = described_class.parse("a;flag;empty=")
    expect(segment.matrix_params).to eq([["flag", nil], ["empty", ""]])
    expect(segment["flag"]).to be_nil
    expect(segment["empty"]).to eq("")
    expect(segment.key?("flag")).to eq(true)
    expect(segment.to_s).to eq("a;flag;empty=")
  end

  it "keeps values containing equals signs intact" do
    segment = described_class.parse("a;key=b=c")
    expect(segment.matrix_params).to eq([["key", "b=c"]])
  end

  it "does not split on encoded semicolons or slashes" do
    segment = described_class.parse("a%3Bb%2Fc;x=%3B;y=%2F")
    expect(segment.name).to eq("a%3Bb%2Fc")
    expect(segment.matrix_params).to eq([["x", "%3B"], ["y", "%2F"]])
  end

  it "round-trips through to_s" do
    string = "name;a=1;flag;b=;a=2"
    expect(described_class.parse(string).to_s).to eq(string)
  end

  it "replaces the first occurrence and drops later duplicates on []=" do
    segment = described_class.parse("a;x=1;y=9;x=2")
    segment["x"] = "3"
    expect(segment.matrix_params).to eq([["x", "3"], ["y", "9"]])
  end

  it "appends with []= when the key is absent" do
    segment = described_class.parse("a;x=1")
    segment["z"] = "5"
    expect(segment.matrix_params).to eq([["x", "1"], ["z", "5"]])
  end

  it "appends duplicates with add_param" do
    segment = described_class.parse("a;x=1")
    segment.add_param("x", "2")
    segment.add_param("flag")
    expect(segment.matrix_params).to eq([["x", "1"], ["x", "2"], ["flag", nil]])
  end

  it "removes all occurrences with delete_param" do
    segment = described_class.parse("a;x=1;y=9;x=2")
    expect(segment.delete_param("x")).to eq(["1", "2"])
    expect(segment.matrix_params).to eq([["y", "9"]])
  end

  it "compares by name and parameters" do
    expect(described_class.parse("a;x=1")).to eq(described_class.parse("a;x=1"))
    expect(described_class.parse("a;x=1")).not_to eq(described_class.parse("a;x=2"))
    expect(described_class.parse("a;x")).not_to eq(described_class.parse("a;x="))
  end
end

describe Addressable::URI, "path segments" do
  it "exposes structured segments with matrix parameters" do
    uri = described_class.parse("/books;lang=zh/42;rev=3")
    segments = uri.path_segments
    expect(segments.map(&:name)).to eq(["", "books", "42"])
    expect(segments[1].matrix_params).to eq([["lang", "zh"]])
    expect(segments[2].matrix_params).to eq([["rev", "3"]])
  end

  it "round-trips the path through the segments" do
    path = "/books;lang=zh;flag/42;rev=3;rev=4/"
    uri = described_class.parse(path)
    expect(uri.path_segments.map(&:to_s).join("/")).to eq(path)
  end

  it "treats encoded semicolons and slashes as data, not delimiters" do
    uri = described_class.parse("/a%3Bb/c%2Fd;x=p%3Bq")
    segments = uri.path_segments
    expect(segments.map(&:name)).to eq(["", "a%3Bb", "c%2Fd"])
    expect(segments[2].matrix_params).to eq([["x", "p%3Bq"]])
    expect(uri.path_segments.map(&:to_s).join("/")).to eq("/a%3Bb/c%2Fd;x=p%3Bq")
  end

  it "updates the path when segments are assigned back" do
    uri = described_class.parse("/books;lang=zh/42;rev=3")
    segments = uri.path_segments
    segments[1]["lang"] = "en"
    segments[2].add_param("rev", "4")
    uri.path_segments = segments
    expect(uri.path).to eq("/books;lang=en/42;rev=3;rev=4")
  end

  it "only modifies the targeted segment when the same parameter name appears in two segments" do
    uri = described_class.parse("/books;lang=zh/42;lang=en")
    segments = uri.path_segments
    segments[1]["lang"] = "fr"
    uri.path_segments = segments
    expect(uri.path).to eq("/books;lang=fr/42;lang=en")
    expect(uri.path_segments[1].values("lang")).to eq(["fr"])
    expect(uri.path_segments[2].values("lang")).to eq(["en"])
  end

  it "keeps matrix parameters attached to their segment during relative joins" do
    base = described_class.parse("http://example.com/books;lang=zh/")
    joined = base.join("42;rev=3")
    expect(joined.path).to eq("/books;lang=zh/42;rev=3")
    expect(joined.path_segments[1].matrix_params).to eq([["lang", "zh"]])
    expect(joined.path_segments[2].matrix_params).to eq([["rev", "3"]])
  end

  it "keeps matrix parameters attached to their segment during dot-segment normalization in joins" do
    base = described_class.parse("http://example.com/a;p=1/b")
    joined = base.join("../c;r=2")
    expect(joined.path).to eq("/c;r=2")
    expect(joined.path_segments.map(&:name)).to eq(["", "c"])
    expect(joined.path_segments[1].matrix_params).to eq([["r", "2"]])
  end

  it "removes a segment's parameters along with the segment during normalization" do
    uri = described_class.parse("http://example.com/a;x=1/b;y=2/../c;z=3")
    uri.normalize!
    expect(uri.path).to eq("/a;x=1/c;z=3")
    expect(uri.path_segments[1].matrix_params).to eq([["x", "1"]])
    expect(uri.path_segments[2].matrix_params).to eq([["z", "3"]])
  end
end
