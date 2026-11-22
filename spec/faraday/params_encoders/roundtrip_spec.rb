# frozen_string_literal: true

RSpec.describe Faraday::RoundtripParamsEncoder do
  it_behaves_like "a params encoder"

  # End-to-end acceptance: a real Connection.build_url serializes the params,
  # then the matching decoder parses the produced query back. This proves the
  # optional protocol works through Faraday's normal URL pipeline without
  # changing the default nested/flat formats.
  describe "Connection.build_url round-trip" do
    let(:connection) do
      Faraday::Connection.new(
        "http://example.com/api",
        request: { params_encoder: Faraday::RoundtripParamsEncoder }
      )
    end

    it "builds a URL whose query decodes back without merging the empties" do
      params = {
        "nil_field" => nil,
        "empty_string" => "",
        "empty_array" => [],
        "tags" => %w[alpha beta],
        "filter" => { "a[b]" => "v", "empty" => [] }
      }

      uri = connection.build_url("search", params)

      expect(uri.to_s).to start_with("http://example.com/api/search?")

      decoded = Faraday::RoundtripParamsEncoder.decode(uri.query)

      expect(decoded).to eq(params)
      expect(decoded["nil_field"]).to be_nil
      expect(decoded["empty_string"]).to eq("")
      expect(decoded["empty_array"]).to eq([])
      expect(decoded["tags"]).to eq(%w[alpha beta])
      expect(decoded["filter"]["a[b]"]).to eq("v")
      expect(decoded["filter"]["empty"]).to eq([])
    end

    it "keeps order-sensitive repeated values through a built URL" do
      uri = connection.build_url("search", "roll" => %w[california philadelphia])

      expect(uri.query).to eq("roll[0]=california&roll[1]=philadelphia")
      expect(described_class.decode(uri.query)).to eq(
        "roll" => %w[california philadelphia]
      )
    end
  end

  # The three "empty" values that the default encoders conflate must stay
  # distinguishable both on the wire and after a decode.
  describe "empty value distinction" do
    it "encodes nil, an empty string and an empty array differently" do
      expect(subject.encode("nil_field" => nil)).to eq("nil_field")
      expect(subject.encode("empty_string" => "")).to eq("empty_string=")
      expect(subject.encode("empty_array" => [])).to eq("empty_array[]")
    end

    it "decodes the three empty representations back to distinct values" do
      decoded = subject.decode("nil_field&empty_string=&empty_array[]")

      expect(decoded.fetch("nil_field")).to be_nil
      expect(decoded.fetch("empty_string")).to eq("")
      expect(decoded.fetch("empty_array")).to eq([])
    end

    it "keeps the three empties distinct when they share one query string" do
      params = {
        "nil_field" => nil,
        "empty_string" => "",
        "empty_array" => []
      }

      decoded = subject.decode(subject.encode(params))

      expect(decoded).to eq(params)
      # None of the three empties collapsed into another meaning.
      expect(decoded["nil_field"]).to be_nil
      expect(decoded["empty_string"]).to eq("")
      expect(decoded["empty_array"]).to eq([])
    end

    it "distinguishes an empty array from a single-element array" do
      expect(subject.encode("a" => [])).to eq("a[]")
      expect(subject.encode("a" => ["x"])).to eq("a[0]=x")

      expect(subject.decode("a[]")).to eq("a" => [])
      expect(subject.decode("a[0]=x")).to eq("a" => ["x"])
    end
  end

  describe "order-sensitive repeated values" do
    it "encodes array values with positional indices and keeps their order" do
      expect(subject.encode("roll" => %w[california philadelphia]))
        .to eq("roll[0]=california&roll[1]=philadelphia")
    end

    it "round-trips an ordered array including nil and empty strings" do
      params = { "roll" => ["z", nil, ""] }

      expect(subject.decode(subject.encode(params))).to eq(params)
    end

    it "preserves the order of repeated flat pairs with the same key" do
      expect(subject.decode("a=2&a=3&a=1")).to eq("a" => %w[2 3 1])
    end

    it "preserves order for an Array input with duplicate keys" do
      query = subject.encode([["z", "1"], ["a", "2"], ["a", "3"]])

      expect(query).to eq("z=1&a=2&a=3")
      expect(subject.decode(query)).to eq("z" => "1", "a" => %w[2 3])
    end
  end

  describe "special (bracket-containing) keys" do
    it "escapes user brackets so they are not mistaken for nesting" do
      expect(subject.encode("a[b]" => 1)).to eq("a%5Bb%5D=1")
    end

    it "restores an escaped special key on decode" do
      params = { "a[b]c" => "v" }

      expect(subject.decode(subject.encode(params))).to eq(params)
    end

    it "keeps a special key distinct from a nested hash with the same spelling" do
      special = subject.encode("a[b]" => 1)
      nested = subject.encode("a" => { "b" => 1 })

      expect(special).to eq("a%5Bb%5D=1")
      expect(nested).to eq("a[b]=1")
      expect(subject.decode(special)).to eq("a[b]" => "1")
      expect(subject.decode(nested)).to eq("a" => { "b" => "1" })
    end
  end

  describe "nested structures" do
    it "round-trips nested hashes and arrays" do
      params = {
        "nested" => {
          "plain" => ["z", nil, ""],
          "empty" => [],
          "h" => { "k" => nil }
        }
      }

      expect(subject.decode(subject.encode(params))).to eq(params)
    end

    it "round-trips an array of hashes" do
      params = { "a" => [{ "b" => "1" }, { "c" => "2" }] }

      expect(subject.decode(subject.encode(params))).to eq(params)
    end

    it "lets an empty-array marker and a sibling hash coexist" do
      params = { "nested" => { "empty" => [], "h" => { "k" => nil } } }

      expect(subject.decode(subject.encode(params))).to eq(params)
    end

    it "populates an array declared empty when indexed entries follow" do
      expect(subject.decode("a[]&a[0]=x")).to eq("a" => ["x"])
    end
  end

  describe "escaping" do
    it "escapes spaces, plus signs and unicode in keys and values" do
      params = { "q r" => { "café" => ["a+b", "c d", nil], "x" => "" }, "%" => "100%" }

      expect(subject.encode(params)).to eq(
        "%25=100%25&q+r[caf%C3%A9][0]=a%2Bb&q+r[caf%C3%A9][1]=c+d&q+r[caf%C3%A9][2]&q+r[x]="
      )
      expect(subject.decode(subject.encode(params))).to eq(params)
    end

    it "encodes booleans as strings" do
      expect(subject.encode("a" => true, "b" => false)).to eq("a=true&b=false")
    end
  end

  describe "nil and empty inputs" do
    it "returns nil for nil input" do
      expect(subject.encode(nil)).to be_nil
      expect(subject.decode(nil)).to be_nil
    end

    it "returns an empty string/hash for empty input" do
      expect(subject.encode({})).to eq("")
      expect(subject.decode("")).to eq({})
    end

    it "tolerates malformed ownerless markers" do
      expect(subject.decode("=1&[]=2")).to eq({})
    end

    it "does not crash on brackets without a closing pair" do
      expect { subject.decode("a[b") }.not_to raise_error
    end

    it "overwrites a repeated numeric index like the other encoders" do
      expect(subject.decode("a[0]=1&a[0]=2")).to eq("a" => ["2"])
    end

    it "orders numeric indices by their value, not their wire position" do
      expect(subject.decode("a[10]=x&a[2]=y")).to eq("a" => %w[y x])
    end
  end

  describe "structural conflicts" do
    it "raises when a scalar and a nested hash share a path" do
      expect { subject.decode("a=1&a[b]=2") }.to raise_error(TypeError)
    end

    it "raises when array indices and hash keys are mixed on one node" do
      expect { subject.decode("a[0]=1&a[b]=2") }.to raise_error(TypeError)
    end
  end

  describe "sorting" do
    after { described_class.sort_params = true }

    it "sorts hash params by default" do
      expect(subject.encode("b" => 1, "a" => 2)).to eq("a=2&b=1")
    end

    it "preserves insertion order when sorting is disabled" do
      described_class.sort_params = false

      expect(subject.encode("b" => 1, "a" => 2)).to eq("b=1&a=2")
    end
  end
end