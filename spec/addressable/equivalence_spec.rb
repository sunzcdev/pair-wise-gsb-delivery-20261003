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

require "addressable/equivalence"

describe Addressable::Equivalence do
  let(:tag_reordered_a) do
    "http://example.com/pipeline?tag=red&tag=blue&step=extract&step=load"
  end
  let(:tag_reordered_b) do
    "http://example.com/pipeline?tag=blue&tag=red&step=extract&step=load"
  end
  let(:step_reordered) do
    "http://example.com/pipeline?tag=red&tag=blue&step=load&step=extract"
  end

  describe "with tag declared commutative" do
    subject(:policy) do
      described_class.new(commutative_params: ["tag"])
    end

    it "treats reordered tag parameters as equivalent" do
      expect(policy.equivalent?(tag_reordered_a, tag_reordered_b)).to be(true)
    end

    it "produces identical keys for reordered tag parameters" do
      expect(policy.key(tag_reordered_a)).to eq(policy.key(tag_reordered_b))
    end

    it "rejects reordered step parameters" do
      expect(policy.equivalent?(tag_reordered_a, step_reordered)).to be(false)
    end

    it "produces different keys for reordered step parameters" do
      expect(policy.key(tag_reordered_a)).not_to eq(policy.key(step_reordered))
    end

    it "reports the query component as the source of the difference" do
      result = policy.compare(tag_reordered_a, step_reordered)
      expect(result).not_to be_equivalent
      expect(result.differences).to eq([:query])
    end

    it "reports no differences for equivalent URIs" do
      result = policy.compare(tag_reordered_a, tag_reordered_b)
      expect(result).to be_equivalent
      expect(result.differences).to be_empty
    end
  end

  describe "with the conservative default policy" do
    subject(:policy) { described_class.new }

    it "treats any query reordering as a difference" do
      expect(policy.equivalent?(tag_reordered_a, tag_reordered_b)).to be(false)
    end

    it "compares fragments" do
      result = policy.compare(
        "http://example.com/page#one", "http://example.com/page#two"
      )
      expect(result.differences).to eq([:fragment])
    end

    it "distinguishes a missing value from an empty value" do
      expect(
        policy.equivalent?("http://example.com/?flag", "http://example.com/?flag=")
      ).to be(false)
    end
  end

  describe "with ignore_fragment enabled" do
    subject(:policy) { described_class.new(ignore_fragment: true) }

    it "ignores fragment differences" do
      expect(
        policy.equivalent?(
          "http://example.com/page#one", "http://example.com/page#two"
        )
      ).to be(true)
    end

    it "excludes the fragment from the key" do
      expect(policy.key("http://example.com/page#one")).to(
        eq(policy.key("http://example.com/page"))
      )
    end
  end

  describe "with empty_value_equals_missing enabled" do
    subject(:policy) { described_class.new(empty_value_equals_missing: true) }

    it "treats ?flag and ?flag= as equivalent" do
      expect(
        policy.equivalent?("http://example.com/?flag", "http://example.com/?flag=")
      ).to be(true)
    end

    it "still distinguishes missing values from non-empty values" do
      expect(
        policy.equivalent?("http://example.com/?flag", "http://example.com/?flag=x")
      ).to be(false)
    end
  end

  describe "duplicate query parameters" do
    subject(:policy) do
      described_class.new(commutative_params: ["tag"])
    end

    it "are preserved, so collapsing them changes equivalence" do
      expect(
        policy.equivalent?(
          "http://example.com/?tag=a&tag=a", "http://example.com/?tag=a"
        )
      ).to be(false)
    end

    it "are matched as a multiset for commutative parameters" do
      expect(
        policy.equivalent?(
          "http://example.com/?tag=a&tag=b&tag=a",
          "http://example.com/?tag=a&tag=a&tag=b"
        )
      ).to be(true)
    end
  end

  describe "component semantics" do
    subject(:policy) { described_class.new }

    it "ignores default ports" do
      expect(
        policy.equivalent?("http://example.com:80/a", "http://example.com/a")
      ).to be(true)
    end

    it "keeps non-default ports significant" do
      result = policy.compare(
        "http://example.com:8080/a", "http://example.com/a"
      )
      expect(result.differences).to eq([:port])
    end

    it "ignores host and scheme case" do
      expect(
        policy.equivalent?("HTTP://EXAMPLE.COM/a", "http://example.com/a")
      ).to be(true)
    end

    it "treats equivalent percent-encodings in the path as equal" do
      expect(
        policy.equivalent?("http://example.com/%7Euser", "http://example.com/~user")
      ).to be(true)
    end

    it "treats equivalent percent-encodings in query values as equal" do
      expect(
        policy.equivalent?(
          "http://example.com/?tag=%41", "http://example.com/?tag=A"
        )
      ).to be(true)
    end

    it "identifies the component responsible for a difference" do
      result = policy.compare(
        "http://example.com/a", "http://example.com/b"
      )
      expect(result.differences).to eq([:path])
    end
  end

  describe "key consistency" do
    subject(:policy) do
      described_class.new(
        commutative_params: [:tag],
        ignore_fragment: true,
        empty_value_equals_missing: true
      )
    end

    it "accepts parameter names given as symbols" do
      expect(
        policy.equivalent?(
          "http://example.com/?tag=a&tag=b", "http://example.com/?tag=b&tag=a"
        )
      ).to be(true)
    end

    it "generates equal keys exactly when URIs are equivalent" do
      uri_a = "HTTP://EXAMPLE.COM:80/p?tag=a&tag=b&flag#frag"
      uri_b = "http://example.com/p?tag=b&flag=&tag=a"
      expect(policy.equivalent?(uri_a, uri_b)).to be(true)
      expect(policy.key(uri_a)).to eq(policy.key(uri_b))
    end

    it "accepts Addressable::URI instances" do
      uri = Addressable::URI.parse("http://example.com/?tag=a&tag=b")
      expect(policy.key(uri)).to eq(policy.key("http://example.com/?tag=b&tag=a"))
    end

    it "does not let encoded delimiters in values collide with key syntax" do
      key_a = policy.key("http://example.com/?a=1%26b%3D2")
      key_b = policy.key("http://example.com/?a=1&b=2")
      expect(key_a).not_to eq(key_b)
    end
  end
end
