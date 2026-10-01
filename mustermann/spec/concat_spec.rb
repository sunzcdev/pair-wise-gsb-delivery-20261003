# frozen_string_literal: true
require 'support'
require 'mustermann'

describe Mustermann::Concat do
  describe Mustermann::Concat::Native do
    context "sinatra + sinatra" do
      subject(:pattern) { Mustermann.new("/:foo") +  Mustermann.new("/:bar") }
      its(:class) { should be == Mustermann::Sinatra }
      its(:to_s)  { should be == "/{foo}/{bar}" }
    end

    context "sinatra + string" do
      subject(:pattern) { Mustermann.new("/:foo") +  "/:bar" }
      its(:class) { should be == Mustermann::Sinatra }
      its(:to_s)  { should be == "/{foo}/\\:bar" }
    end

    context "regular + regular" do
      subject(:pattern) { Mustermann.new(/foo/) +  Mustermann.new(/bar/) }
      its(:class) { should be == Mustermann::Regular }
      its(:to_s)  { should be == "foobar" }
    end

    context "sinatra + rails" do
      subject(:pattern) { Mustermann.new("/:foo") +  Mustermann.new("/:bar", type: :rails) }
      its(:class) { should be == Mustermann::Sinatra }
      its(:to_s)  { should be == "/{foo}/{bar}" }
    end

    context "sinatra + flask" do
      subject(:pattern) { Mustermann.new("/:foo") +  Mustermann.new("/<bar>", type: :flask) }
      its(:class) { should be == Mustermann::Sinatra }
      its(:to_s)  { should be == "/{foo}/{bar}" }
    end

    context "sinatra + flask (typed)" do
      subject(:pattern) { Mustermann.new("/:foo") +  Mustermann.new("/<int:bar>", type: :flask) }
      its(:class) { should be == Mustermann::Concat }
      its(:to_s)  { should be == '(sinatra:"/:foo" + flask:"/<int:bar>")' }
    end

    context "sinatra + sinatra (different options)" do
      subject(:pattern) { Mustermann.new("/:foo") +  Mustermann.new("/:bar", uri_decode: false) }
      its(:class) { should be == Mustermann::Concat }
      its(:to_s)  { should be == '(sinatra:"/:foo" + sinatra:"/:bar")' }
    end

    context "sinatra + rails (different options)" do
      subject(:pattern) { Mustermann.new("/:foo") +  Mustermann.new("/:bar", type: :rails, uri_decode: false) }
      its(:class) { should be == Mustermann::Concat }
      its(:to_s)  { should be == '(sinatra:"/:foo" + rails:"/:bar")' }
    end

    context "sinatra + rails (different options) + sinatra" do
      subject(:pattern) { Mustermann.new("/:foo") +  Mustermann.new("/:bar", type: :rails, uri_decode: false) + Mustermann.new("/:baz") }
      its(:class) { should be == Mustermann::Concat }
      its(:to_s)  { should be == '(sinatra:"/:foo" + rails:"/:bar" + sinatra:"/:baz")' }
    end

    context "sinatra + (sinatra + regular)" do
      subject(:pattern) { Mustermann.new("/foo") + (Mustermann.new("/bar") + Mustermann.new(/baz/)) }
      its(:class) { should be == Mustermann::Concat }
      its(:to_s)  { should be == '(sinatra:"/foo/bar" + regular:"baz")' }
    end

    context "sinatra + (sinatra + rails (different options) + sinatra)" do
      subject(:pattern) { Mustermann.new("/foo") + (Mustermann.new("/bar") + Mustermann.new("/baz", type: :rails, uri_decode: false) + Mustermann.new("/boo")) }
      its(:class) { should be == Mustermann::Concat }
      its(:to_s)  { should be == '(sinatra:"/foo/bar" + hybrid:"/baz/boo")' }
    end

    context "rails with Integer capture + sinatra with Symbol capture" do
      subject(:pattern) { Mustermann.new("/:a", type: :rails, capture: Integer) + Mustermann.new("/:b", capture: Symbol) }
      its(:class) { should be == Mustermann::Hybrid }
      its(:to_s)  { should be == '/{a}/{b}'         }
      example { subject.params("/42/foo").should be == { "a" => 42, "b" => :foo } }
      example { subject.params("/foo/bar").should be_nil }
    end
  end

  subject(:pattern) { Mustermann::Concat.new("/:foo", "/:bar") }

  describe :=== do
    example { (pattern === "/foo/bar")  .should be true  }
    example { (pattern === "/foo/bar/") .should be false }
    example { (pattern === "/foo")      .should be false }
  end

  describe :match do
    it { should match("/foo/bar").capturing(foo: "foo", bar: "bar") }
    it { should_not match("/foo/bar/") }
    it { should_not match("/foo/") }
  end

  describe :params do
    example { pattern.params("/foo/bar")  .should be == { "foo" => "foo", "bar" => "bar" }}
    example { pattern.params("/foo/bar/") .should be_nil }
    example { pattern.params("/foo")      .should be_nil }
  end

  describe :peek do
    example { pattern.peek("/foo/bar/baz")  .should be == "/foo/bar" }
    example { pattern.peek("/foo")          .should be_nil }
  end

  describe :peek_params do
    example { pattern.peek_params("/foo/bar/baz")  .should be == [{ "foo" => "foo", "bar" => "bar" }, 8]}
    example { pattern.peek_params("/foo")          .should be_nil }
  end

  describe :peek_match do
    example { pattern.peek_match("/foo/bar/baz").to_s .should be == "/foo/bar" }
    example { pattern.peek_match("/foo")              .should be_nil }
  end

  describe :peek_size do
    example { pattern.peek_size("/foo/bar/baz")  .should be == 8 }
    example { pattern.peek_size("/foo")          .should be_nil }
  end

  describe :expand do
    it { should expand(foo: :bar, bar: :foo)                    .to('/bar/foo')        }
    it { should expand(:append, foo: :bar, bar: :foo, baz: 42)  .to('/bar/foo?baz=42') }
    it { should_not expand(foo: :bar) }
  end

  describe :to_templates do
    subject(:pattern) { Mustermann::Concat.new("/:foo|:bar", "(/:baz)?") }
    it { should generate_template("/{foo}/{baz}") }
    it { should generate_template("{bar}/{baz}") }
    it { should generate_template("/{foo}") }
    it { should generate_template("{bar}") }
  end

  describe "backtracking" do
    # the different pattern types and options keep the three segments from
    # being merged into a single pattern, so matching has to find a way to
    # split the string between a multi-segment splat, a capture and a fixed
    # /manifest tail
    subject(:pattern) do
      Mustermann.new("/*path", uri_decode: false) +
        Mustermann.new("/:id", type: :rails) +
        Mustermann.new("/manifest", type: :identity)
    end

    example { pattern.should be_a Mustermann::Concat }

    describe :=== do
      example { (pattern === "/a/b/42/manifest")  .should be true  }
      example { (pattern === "/a/b/manifest")     .should be true  }
      example { (pattern === "/a/b/42/other")     .should be false }
      example { (pattern === "/a/b/42/manifest/") .should be false }
    end

    describe :match do
      it { should match("/a/b/42/manifest").capturing(path: "a/b", id: "42") }
      it { should match("/a/b/manifest").capturing(path: "a", id: "b") }
      it { should_not match("/a/b/42/other") }
      it { should_not match("/a/b/42/manifest/") }
    end

    describe :params do
      example { pattern.params("/a/b/42/manifest")  .should be == { "path" => "a/b", "id" => "42" } }
      example { pattern.params("/a/b/42/other")     .should be_nil }
      example { pattern.params("/a/b/42/manifest/") .should be_nil }
    end

    describe :peek do
      example { pattern.peek("/a/b/42/manifest/trailing") .should be == "/a/b/42/manifest" }
      example { pattern.peek("/a/b/42/other")             .should be_nil }
    end

    describe :peek_match do
      example { pattern.peek_match("/a/b/42/manifest/trailing").to_s   .should be == "/a/b/42/manifest" }
      example { pattern.peek_match("/a/b/42/manifest/trailing").params .should be == { "path" => "a/b", "id" => "42" } }
      example { pattern.peek_match("/a/b/42/other")                    .should be_nil }
    end

    describe :peek_params do
      example { pattern.peek_params("/a/b/42/manifest/trailing") .should be == [{ "path" => "a/b", "id" => "42" }, 16] }
      example { pattern.peek_params("/a/b/42/other")             .should be_nil }
    end

    describe :peek_size do
      example { pattern.peek_size("/a/b/42/manifest/trailing") .should be == 16 }
      example { pattern.peek_size("/a/b/42/other")             .should be_nil }
    end

    context "with multiple valid splits" do
      # the leftmost segment keeps as much as possible, each following
      # segment again prefers its longest match
      example { pattern.params("/x/manifest/7/manifest").should be == { "path" => "x/manifest", "id" => "7" } }
    end

    context "with percent-encoded input" do
      subject(:pattern) { Mustermann.new("/*path") + Mustermann.new("/manifest", uri_decode: false) }
      it { should match("/a%20b/c%2Fd/manifest").capturing(path: "a%20b/c%2Fd") }
      example { pattern.match("/a%20b/c%2Fd/manifest").params .should be == { "path" => "a b/c/d" } }
      example { pattern.params("/a%20b/c%2Fd/manifest")       .should be == { "path" => "a b/c/d" } }
      example { pattern.params("/a%20b/c%2Fd/other")          .should be_nil }
    end

    context "with captures only available after backtracking" do
      # failed candidates must not leak their captures into the result
      subject(:pattern) { Mustermann.new(/(?<first>a+)/, uri_decode: false) + Mustermann.new("ab", type: :identity) }
      example { pattern.params("aab")  .should be == { "first" => "a" } }
      example { pattern.params("aaab") .should be == { "first" => "aa" } }
      example { pattern.params("b")    .should be_nil }
    end

    context "with segments preferring the longest match from left to right" do
      subject(:pattern) { Mustermann.new(/(?<first>a+)/, uri_decode: false) + Mustermann.new(/(?<second>a+)/) }
      example { pattern.params("aaa") .should be == { "first" => "aa", "second" => "a" } }
    end

    context "with an empty-matching segment" do
      subject(:pattern) { Mustermann.new(/x*/) + Mustermann.new("b", type: :identity) }
      it { should match("b") }
      it { should match("xxb") }
      it { should_not match("xxc") }
    end

    context "with all segments matching empty" do
      subject(:pattern) { Mustermann.new(/a*/) + Mustermann.new("b", type: :identity) + Mustermann.new(/c*/) }
      it { should match("b") }
      it { should match("abc") }
      it { should_not match("abbc") }
      example { (pattern === "") .should be false }
    end

    context "when patterns can be merged natively" do
      subject(:pattern) { Mustermann.new("/*path") + "/manifest" }
      example { pattern.should_not be_a Mustermann::Concat }
      it { should match("/a/b/manifest").capturing(path: "a/b") }
      example { pattern.params("/a/b/manifest")  .should be == { "path" => "a/b" } }
      example { pattern.params("/a/b/other")     .should be_nil }
    end
  end
end
