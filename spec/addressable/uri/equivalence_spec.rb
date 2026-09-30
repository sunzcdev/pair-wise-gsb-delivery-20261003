# frozen_string_literal: true

require "spec_helper"

require "addressable/uri"

describe Addressable::URI::Equivalence do
  describe "#initialize" do
    it "does not include the fragment by default" do
      strategy = described_class.new
      expect(strategy.fragment).to eq(false)
    end

    it "reorders no query parameters by default" do
      strategy = described_class.new
      expect(strategy.reorderable_query_params).to eq([])
    end

    it "treats missing and empty values as distinct by default" do
      strategy = described_class.new
      expect(strategy.missing_value_equals_empty_value).to eq(false)
    end

    it "accepts symbols as reorderable parameter names" do
      strategy = described_class.new(reorderable_query_params: [:tag])
      expect(strategy.reorderable_query_params).to eq(["tag"])
    end

    it "is immutable" do
      strategy = described_class.new
      expect(strategy.frozen?).to eq(true)
    end
  end

  describe "tag versus step reordering" do
    let(:tag_strategy) do
      described_class.new(reorderable_query_params: ["tag"])
    end

    let(:step_strategy) do
      described_class.new(reorderable_query_params: ["step"])
    end

    let(:uri_a) { "http://example.com/flow?step=1&tag=a&tag=b" }
    let(:uri_b) { "http://example.com/flow?step=1&tag=b&tag=a" }

    it "regards swapped tags as equivalent" do
      expect(tag_strategy.equivalent?(uri_a, uri_b)).to eq(true)
      expect(tag_strategy.difference(uri_a, uri_b)).to eq(nil)
    end

    it "keeps step order significant when only tag is reorderable" do
      uri_one = "http://example.com/flow?step=2&step=1&tag=a"
      uri_two = "http://example.com/flow?step=1&step=2&tag=a"
      expect(tag_strategy.equivalent?(uri_one, uri_two)).to eq(false)
      expect(tag_strategy.difference(uri_one, uri_two)).to eq(:query)
    end

    it "regards a changed fixed parameter value as distinct under the tag strategy" do
      uri_one = "http://example.com/flow?tag=a&step=2"
      uri_two = "http://example.com/flow?tag=a&step=1"
      expect(tag_strategy.equivalent?(uri_one, uri_two)).to eq(false)
    end

    it "regards swapped tags as distinct under the step strategy" do
      expect(step_strategy.equivalent?(uri_a, uri_b)).to eq(false)
      expect(step_strategy.difference(uri_a, uri_b)).to eq(:query)
    end

    it "regards swapped steps as equivalent under the step strategy" do
      uri_one = "http://example.com/flow?step=2&step=1&tag=a"
      uri_two = "http://example.com/flow?step=1&step=2&tag=a"
      expect(step_strategy.equivalent?(uri_one, uri_two)).to eq(true)
    end

    it "generates equal keys exactly when the URIs are equivalent" do
      expect(
        tag_strategy.equivalence_key(uri_a)
      ).to eq(tag_strategy.equivalence_key(uri_b))
      expect(
        step_strategy.equivalence_key(uri_a)
      ).not_to eq(step_strategy.equivalence_key(uri_b))
    end

    it "groups reordered tags into a single bucket" do
      bucket = {}
      [uri_a, uri_b].each do |uri|
        key = tag_strategy.equivalence_key(uri)
        bucket[key] ||= []
        bucket[key] << uri
      end
      expect(bucket.values.map(&:length)).to eq([2])
    end
  end

  describe "ordered query handling (conservative default)" do
    subject(:strategy) { described_class.new }

    it "keeps parameter order significant" do
      expect(
        strategy.equivalent?(
          "http://example.com/?a=1&b=2",
          "http://example.com/?b=2&a=1"
        )
      ).to eq(false)
    end

    it "preserves repeated parameters instead of collapsing to a Hash" do
      expect(
        strategy.equivalent?(
          "http://example.com/?a=1&a=2",
          "http://example.com/?a=2&a=1"
        )
      ).to eq(false)

      expect(
        strategy.equivalent?(
          "http://example.com/?a=1&a=2",
          "http://example.com/?a=1&a=2"
        )
      ).to eq(true)
    end

    it "detects a different number of repeated parameters" do
      expect(
        strategy.equivalent?(
          "http://example.com/?a=1&a=2",
          "http://example.com/?a=1"
        )
      ).to eq(false)
    end

    it "keeps repeated reorderable parameters as a multiset" do
      reorderable = described_class.new(reorderable_query_params: ["a"])
      expect(
        reorderable.equivalent?(
          "http://example.com/?a=1&a=2",
          "http://example.com/?a=2&a=1"
        )
      ).to eq(true)

      expect(
        reorderable.equivalent?(
          "http://example.com/?a=1&a=2",
          "http://example.com/?a=1&a=1"
        )
      ).to eq(false)
    end

    it "allows reorderable parameters to cross fixed parameters" do
      reorderable = described_class.new(reorderable_query_params: ["tag"])
      expect(
        reorderable.equivalent?(
          "http://example.com/?tag=a&step=1&tag=b",
          "http://example.com/?tag=b&step=1&tag=a"
        )
      ).to eq(true)
    end

    it "does not let a fixed parameter change order" do
      reorderable = described_class.new(reorderable_query_params: ["tag"])
      expect(
        reorderable.equivalent?(
          "http://example.com/?step=1&tag=a",
          "http://example.com/?step=2&tag=a"
        )
      ).to eq(false)
    end
  end

  describe "missing versus empty values" do
    subject(:strategy) { described_class.new }

    it "treats them as distinct by default" do
      expect(
        strategy.equivalent?(
          "http://example.com/?tag",
          "http://example.com/?tag="
        )
      ).to eq(false)
      expect(
        strategy.difference(
          "http://example.com/?tag",
          "http://example.com/?tag="
        )
      ).to eq(:query)
    end

    it "treats them as equivalent when enabled" do
      lenient = described_class.new(missing_value_equals_empty_value: true)
      expect(
        lenient.equivalent?(
          "http://example.com/?tag",
          "http://example.com/?tag="
        )
      ).to eq(true)
    end

    it "distinguishes both forms from a present value" do
      lenient = described_class.new(missing_value_equals_empty_value: true)
      expect(
        lenient.equivalent?(
          "http://example.com/?tag",
          "http://example.com/?tag=x"
        )
      ).to eq(false)
    end
  end

  describe "fragment handling" do
    subject(:strategy) { described_class.new }

    it "ignores the fragment by default" do
      expect(
        strategy.equivalent?(
          "http://example.com/a#one",
          "http://example.com/a#two"
        )
      ).to eq(true)
    end

    it "includes the fragment when enabled" do
      including = described_class.new(fragment: true)
      expect(
        including.equivalent?(
          "http://example.com/a#one",
          "http://example.com/a#two"
        )
      ).to eq(false)
      expect(
        including.difference(
          "http://example.com/a#one",
          "http://example.com/a#two"
        )
      ).to eq(:fragment)
    end

    it "normalizes percent escapes in the fragment" do
      including = described_class.new(fragment: true)
      expect(
        including.equivalent?(
          "http://example.com/a#x%2fy",
          "http://example.com/a#x%2Fy"
        )
      ).to eq(true)
    end
  end

  describe "conservative component normalization" do
    subject(:strategy) { described_class.new }

    it "folds scheme case" do
      expect(
        strategy.equivalent?(
          "HTTP://example.com/",
          "http://example.com/"
        )
      ).to eq(true)
    end

    it "folds host case" do
      expect(
        strategy.equivalent?(
          "http://EXAMPLE.COM/path",
          "http://example.com/path"
        )
      ).to eq(true)
    end

    it "keeps the path case-sensitive" do
      expect(
        strategy.equivalent?(
          "http://example.com/PathA",
          "http://example.com/patha"
        )
      ).to eq(false)
    end

    it "omits default ports" do
      expect(
        strategy.equivalent?(
          "http://example.com:80/a",
          "http://example.com/a"
        )
      ).to eq(true)
    end

    it "keeps non-default ports significant" do
      expect(
        strategy.equivalent?(
          "http://example.com:8080/a",
          "http://example.com/a"
        )
      ).to eq(false)
    end

    it "normalizes percent escapes per component" do
      expect(
        strategy.equivalent?(
          "http://example.com/a%2eb",
          "http://example.com/a.b"
        )
      ).to eq(true)
    end

    it "removes dot segments from the path" do
      expect(
        strategy.equivalent?(
          "http://example.com/a/./b/../c",
          "http://example.com/a/c"
        )
      ).to eq(true)
    end

    it "normalizes userinfo" do
      expect(
        strategy.equivalent?(
          "http://user:pass@example.com/a",
          "http://user:pass@example.com/a"
        )
      ).to eq(true)
    end
  end

  describe "query percent-encoding semantics" do
    subject(:strategy) { described_class.new }

    it "treats percent-encoded ampersand as data, not a separator" do
      expect(
        strategy.equivalent?(
          "http://example.com/?a=b%26c%3Dd",
          "http://example.com/?a=b&c=d"
        )
      ).to eq(false)
    end

    it "treats percent-escape hex digits case-insensitively" do
      expect(
        strategy.equivalent?(
          "http://example.com/?a=%2f",
          "http://example.com/?a=%2F"
        )
      ).to eq(true)
    end

    it "decodes plus as space in form-encoded queries" do
      expect(
        strategy.equivalent?(
          "http://example.com/?a=b+c",
          "http://example.com/?a=b%20c"
        )
      ).to eq(true)
    end

    it "does not decode plus as space outside http/https" do
      expect(
        strategy.equivalent?(
          "ftp://example.com/?a=b+c",
          "ftp://example.com/?a=b%20c"
        )
      ).to eq(false)
    end

    it "treats an empty query like an absent one" do
      expect(
        strategy.equivalent?(
          "http://example.com/a",
          "http://example.com/a?"
        )
      ).to eq(true)
    end
  end

  describe "#difference" do
    subject(:strategy) { described_class.new }

    {
      ["http://example.com/a", "https://example.com/a"] => :scheme,
      ["http://a.example.com/a", "http://b.example.com/a"] => :host,
      ["http://example.com:8080/a", "http://example.com/a"] => :port,
      ["http://u:p@example.com/a", "http://u:q@example.com/a"] => :userinfo,
      ["http://example.com/a", "http://example.com/b"] => :path,
      ["http://example.com/?a=1", "http://example.com/?a=2"] => :query
    }.each do |(uri_a, uri_b), component|
      it "reports #{component} as the differing component" do
        expect(strategy.difference(uri_a, uri_b)).to eq(component)
      end
    end

    it "returns nil for equivalent URIs" do
      expect(
        strategy.difference(
          "http://EXAMPLE.com:80/a",
          "http://example.com/a"
        )
      ).to eq(nil)
    end
  end

  describe "input handling" do
    subject(:strategy) { described_class.new }

    it "accepts Addressable::URI objects" do
      uri = Addressable::URI.parse("http://example.com/?a=1")
      expect(strategy.equivalent?(uri, "http://example.com/?a=1")).to eq(true)
    end

    it "accepts strings" do
      expect(
        strategy.equivalence_key("http://example.com/?a=1")
      ).to be_an(Array)
    end

    it "produces a key usable as a Hash key" do
      key = strategy.equivalence_key("http://example.com/?a=1")
      bucket = {key => 1}
      expect(bucket[strategy.equivalence_key("http://example.com/?a=1")]).to eq(1)
    end
  end
end
