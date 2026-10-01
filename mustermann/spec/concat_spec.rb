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
    # a greedy prefix that can swallow multiple directory levels, a fixed
    # middle segment and a fixed /manifest suffix
    subject(:pattern) do
      prefix   = Mustermann.new("/*rest")
      middle   = Mustermann.new("/v2", type: :rails, uri_decode: false)
      suffix   = Mustermann.new("/manifest")
      prefix + middle + suffix
    end

    example { pattern.should be_a(Mustermann::Concat) }

    example "gives characters swallowed by the first segment back to later segments" do
      pattern.match("/api/v2/manifest").params.should be == { "rest" => "api" }
      pattern.match("/api/project/v2/manifest").params.should be == { "rest" => "api/project" }
    end

    example "prefers the longest match for each segment from left to right" do
      pattern.params("/api/v2/v2/manifest").should be == { "rest" => "api/v2" }
    end

    example "results of match, params and peek_match come from the same split" do
      string   = "/api/v2/manifest"
      match    = pattern.match(string)
      peeked   = pattern.peek_match(string)
      params, size = pattern.peek_params(string)

      match.to_s.should     be == string
      match.params.should   be == { "rest" => "api" }
      peeked.to_s.should    be == string
      peeked.params.should  be == match.params
      params.should         be == match.params
      size.should           be == string.size
      pattern.peek_size(string).should be == string.size
    end

    example "only matches the prefix when peeking at a longer string" do
      peeked = pattern.peek_match("/api/v2/manifest/extra")
      peeked.to_s.should   be == "/api/v2/manifest"
      peeked.params.should be == { "rest" => "api" }
      pattern.should_not match("/api/v2/manifest/extra")
      pattern.params("/api/v2/manifest/extra").should be_nil
    end

    example "returns nil when no split can satisfy all segments" do
      pattern.match("/api/manifest").should be_nil
      pattern.match("/api/v2").should be_nil
      pattern.peek_match("/api").should be_nil
      pattern.peek_size("/api").should be_nil
      (pattern === "/api").should be false
    end

    example "does not mix captures from failed candidates into the result" do
      match = pattern.match("/v2/v2/manifest")
      match.params.should be == { "rest" => "v2" }
    end
  end

  describe "backtracking with three greedy segments" do
    subject(:pattern) do
      Mustermann.new("/*a") +
      Mustermann.new("/*b", type: :rails, uri_decode: false) +
      Mustermann.new("/end")
    end

    example { pattern.class.should be == Mustermann::Concat }

    example "finds a split satisfying all three segments" do
      match = pattern.match("/x/y/end")
      match.should be_a(Mustermann::Match)
      match.params.should be == { "a" => "x", "b" => "y" }
      pattern.params("/x/y/end").should be == { "a" => "x", "b" => "y" }
    end

    example "chooses segments longest from left to right" do
      pattern.params("/x/y/z/end").should be == { "a" => "x/y", "b" => "z" }
    end
  end

  describe "backtracking with empty matches" do
    subject(:pattern) do
      Mustermann::Concat.new("(/:a)?", "(/:b)?", "/manifest")
    end

    example "empty matches are allowed to end a segment" do
      pattern.match("/manifest").params.should be == { "a" => nil, "b" => nil }
      pattern.match("/x/manifest").params.should be == { "a" => "x", "b" => nil }
      pattern.match("/x/y/manifest").params.should be == { "a" => "x", "b" => "y" }
    end

    example "longer matches are still preferred" do
      pattern.params("/x/y/manifest").should be == { "a" => "x", "b" => "y" }
      pattern.peek_size("/manifest").should be == "/manifest".size
    end

    example "an all-empty split matches the empty string" do
      Mustermann::Concat.new("(/:a)?", "(/:b)?").match("").to_s.should be == ""
    end
  end

  describe "backtracking with a suffix that never matches" do
    subject(:pattern) { Mustermann.new("/*rest") + Mustermann.new(/(?!)/) }

    example { pattern.class.should be == Mustermann::Concat }

    it { should_not match("/anything") }
    it { should_not match("") }

    example { pattern.params("/anything").should be_nil }
    example { pattern.peek_match("/anything").should be_nil }
    example { pattern.peek_size("/anything").should be_nil }
    example { pattern.peek_params("/anything").should be_nil }
  end

  describe "backtracking with percent encoding" do
    subject(:pattern) do
      Mustermann.new("/*rest") +
      Mustermann.new("/manifest", type: :rails, uri_decode: false)
    end

    example "decodes captures of the successful split" do
      pattern.match("/a%25b/manifest").params.should be == { "rest" => "a%b" }
    end

    example "keeps encoded separators within the first segment's capture" do
      match = pattern.match("/a%2Fb/manifest")
      match.params.should be == { "rest" => "a/b" }
      match.to_s.should   be == "/a%2Fb/manifest"
    end

    example "peeking uses the same split and captures" do
      string = "/a%20b/manifest/extra"
      peeked = pattern.peek_match(string)
      peeked.to_s.should   be == "/a%20b/manifest"
      peeked.params.should be == { "rest" => "a b" }
      pattern.peek_params(string).should be == [{ "rest" => "a b" }, "/a%20b/manifest".size]
    end
  end
end
