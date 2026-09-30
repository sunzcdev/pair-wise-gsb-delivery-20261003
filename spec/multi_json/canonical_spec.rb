require "spec_helper"
require "json"

RSpec.describe MultiJson, ".canonical_dump" do
  subject(:canonical) { described_class.method(:canonical_dump) }

  def hex(bytes)
    bytes.unpack1("H*")
  end

  def utf8(hex_string)
    [hex_string].pack("H*").force_encoding(Encoding::UTF_8)
  end

  describe "primitives" do
    it "serializes literals" do
      expect(canonical.call(nil)).to eq("null")
      expect(canonical.call(true)).to eq("true")
      expect(canonical.call(false)).to eq("false")
    end

    it "serializes empty containers" do
      expect(canonical.call([])).to eq("[]")
      expect(canonical.call({})).to eq("{}")
    end

    it "emits UTF-8 bytes" do
      expect(canonical.call("€").encoding).to eq(Encoding::UTF_8)
      expect(hex(canonical.call("€"))).to eq("22e282ac22")
    end
  end

  describe "RFC 8785 section 3.2.2 worked example" do
    let(:object) do
      {
        "numbers" => [333333333.33333329, 1e30, 4.5, 0.002, 1e-27],
        "string" => "€$\u000f\nA'B\"\\\\\"/",
        "literals" => [nil, true, false]
      }
    end

    let(:expected) do
      "{\"literals\":[null,true,false]," \
        "\"numbers\":[333333333.3333333,1e+30,4.5,0.002,1e-27]," \
        "\"string\":\"€$\\u000f\\nA'B\\\"\\\\\\\\\\\"/\"}"
    end

    it "matches the RFC output byte for byte" do
      expect(canonical.call(object)).to eq(expected)
      expect(hex(canonical.call(object))).to eq(hex(expected))
    end

    it "does not depend on hash insertion order" do
      shuffled = object.keys.shuffle.each_with_object({}) do |key, hash|
        hash[key] = object[key]
      end
      expect(canonical.call(shuffled)).to eq(canonical.call(object))
    end
  end

  describe "property sorting" do
    it "sorts recursively while preserving array element order" do
      object = {
        "b" => [{"z" => 1, "a" => 2}, 3],
        "a" => {"y" => [{"d" => 4, "c" => 3}]}
      }
      expect(canonical.call(object)).to eq(
        '{"a":{"y":[{"c":3,"d":4}]},"b":[{"a":2,"z":1},3]}'
      )
    end

    it "orders a shorter prefix before the longer name" do
      expect(canonical.call({"ab" => 1, "a" => 2, "aa" => 3, "" => 4})).to eq(
        '{"":4,"a":2,"aa":3,"ab":1}'
      )
    end

    it "compares property names as unsigned UTF-16 code units" do
      object = {"😀" => 2, "風" => 1, "￿" => 3, "A" => 4, "" => "x"}
      expected_hex =
        "7b22223a2278222c2241223a342c22e9a2a8223a312c" \
          "22f09f9880223a322c22efbfbf223a337d"
      expect(hex(canonical.call(object))).to eq(expected_hex)
    end

    it "orders an astral name by its high surrogate, not its code point" do
      # U+1F600 is UTF-16 D83D DE00; D83D < E000 even though 0x1F600 > 0xE000.
      expect(canonical.call({"\uE000" => 1, "😀" => 2})).to eq(
        '{"😀":2,"' + "\uE000" + '":1}'
      )
    end

    it "sorts raw unescaped names, not their escaped forms" do
      object = {"\n" => 1, "\u0001" => 2, "a" => 3}
      expect(canonical.call(object)).to eq(
        '{"\u0001":2,"\n":1,"a":3}'
      )
    end
  end

  describe "string serialization" do
    it "uses the predefined short escapes and lowercase hex escapes" do
      expect(canonical.call("a\tb\nc\fd\re\bf")).to eq(
        '"a\tb\nc\fd\re\bf"'
      )
      expect(canonical.call("\u0000\u000f\u001f\u007f")).to eq(
        "\"\\u0000\\u000f\\u001f\u007f\""
      )
      expect(canonical.call('q"\\/z')).to eq('"q\\"\\\\/z"')
    end

    it "keeps non-ASCII characters as raw UTF-8 without NFC/NFD normalization" do
      nfc = "\u00e9"
      nfd = "e\u0301"
      expect(nfc.bytes).not_to eq(nfd.bytes)

      object = {nfd => 1, nfc => 2}
      expected =
        "{\"#{nfd}\":1,\"#{nfc}\":2}"
      expect(hex(canonical.call(object))).to eq(hex(expected))
    end

    it "keeps astral characters as four UTF-8 bytes instead of surrogate pairs" do
      expect(hex(canonical.call("😀"))).to eq("22f09f988022")
    end
  end

  describe "number serialization" do
    # RFC 8785 Appendix B: IEEE 754 hex representation => canonical JSON.
    rfc_samples = {
      "0000000000000000" => "0",
      "8000000000000000" => "0",
      "0000000000000001" => "5e-324",
      "8000000000000001" => "-5e-324",
      "7fefffffffffffff" => "1.7976931348623157e+308",
      "ffefffffffffffff" => "-1.7976931348623157e+308",
      "4340000000000000" => "9007199254740992",
      "c340000000000000" => "-9007199254740992",
      "4430000000000000" => "295147905179352830000",
      "44b52d02c7e14af5" => "9.999999999999997e+22",
      "44b52d02c7e14af6" => "1e+23",
      "44b52d02c7e14af7" => "1.0000000000000001e+23",
      "444b1ae4d6e2ef4e" => "999999999999999700000",
      "444b1ae4d6e2ef4f" => "999999999999999900000",
      "444b1ae4d6e2ef50" => "1e+21",
      "3eb0c6f7a0b5ed8c" => "9.999999999999997e-7",
      "3eb0c6f7a0b5ed8d" => "0.000001",
      "41b3de4355555553" => "333333333.3333332",
      "41b3de4355555554" => "333333333.33333325",
      "41b3de4355555555" => "333333333.3333333",
      "41b3de4355555556" => "333333333.3333334",
      "41b3de4355555557" => "333333333.33333343",
      "becbf647612f3696" => "-0.0000033333333333333333",
      "43143ff3c1cb0959" => "1424953923781206.2"
    }

    rfc_samples.each do |bits, expected|
      it "serializes IEEE 754 #{bits} as #{expected.inspect}" do
        value = [bits].pack("H*").unpack1("G")
        expect(canonical.call(value)).to eq(expected)
      end
    end

    it "uses the fixed/exponential boundaries from ECMAScript" do
      expect(canonical.call(1e20)).to eq("100000000000000000000")
      expect(canonical.call(1e21)).to eq("1e+21")
      expect(canonical.call(1e-6)).to eq("0.000001")
      expect(canonical.call(1e-7)).to eq("1e-7")
      expect(canonical.call(-0.0)).to eq("0")
    end

    it "accepts safe integers and emits them without a decimal point" do
      expect(canonical.call(9007199254740991)).to eq("9007199254740991")
      expect(canonical.call(-9007199254740991)).to eq("-9007199254740991")
    end

    it "matches V8 over independently generated random IEEE 754 values", :v8 do
      state = 0x1234abcd9876feda
      values = []
      bits = []
      while values.length < 5_000
        state = (state * 6364136223846793005 + 1442695040888963407) & 0xffffffffffffffff
        pattern = format("%016x", state)
        value = [pattern].pack("H*").unpack1("G")
        if value.finite? && !value.zero?
          bits << pattern
          values << value
        end
      end

      require "open3"
      script = File.expand_path("../support/jcs_canonicalize.js", __dir__)
      node = ENV["NODE"] || "node"
      begin
        stdout, status = Open3.capture2(node, script, stdin_data: JSON.generate(bits))
      rescue Errno::ENOENT, Errno::EACCES
        skip("node is not available for V8 cross-checking")
      end
      expect(status).to be_success

      expected = JSON.parse(stdout)
      expect(values.map { |value| canonical.call(value) }).to eq(expected)
    end
  end

  describe "rejected inputs" do
    def expect_rejection(value)
      expect { canonical.call(value) }.to raise_error(MultiJson::CanonicalizationError)
    end

    it "rejects integers outside the safe range instead of rounding them" do
      expect_rejection(9007199254740992)
      expect_rejection(-9007199254740992)
      expect_rejection(2**63)
      expect_rejection(-(2**63))
    end

    it "rejects NaN and both infinities" do
      expect_rejection(Float::NAN)
      expect_rejection(Float::INFINITY)
      expect_rejection(-Float::INFINITY)
    end

    it "rejects invalid UTF-8 strings and lone surrogate data" do
      expect_rejection(utf8("ff").dup)
      expect_rejection(utf8("eda080").dup)
      expect_rejection("latin".dup.force_encoding(Encoding::ISO_8859_1))
    end

    it "rejects symbols, non-string keys, and custom objects" do
      expect_rejection(:symbol)
      expect_rejection(1..3)
      expect_rejection(Object.new)
      expect_rejection(BasicObject.new)
      expect_rejection({key: 1})
      expect_rejection({1 => 2})
      expect_rejection({nil => true})
    end

    it "does not coerce unsupported values nested inside containers" do
      expect_rejection([{ "a" => [Float::NAN] }])
      expect_rejection({ "a" => [2**60] })
      expect_rejection([:symbol])
    end
  end

  describe "independence from adapter configuration" do
    it "returns identical bytes under every loadable adapter" do
      object = {"b" => [1, {"d" => 0.1, "c" => 1e21}], "a" => "€"}
      require "multi_json/adapters/json_gem"
      require "multi_json/adapters/json_pure"
      adapters = [
        MultiJson::Adapters::JsonGem,
        MultiJson::Adapters::JsonPure
      ]

      results = adapters.map do |adapter|
        described_class.with_adapter(adapter) do
          described_class.dump_options = {pretty: true}
          described_class.canonical_dump(object)
        end
      end

      expect(results.uniq).to eq([results.first])
      expect(results.first).to eq('{"a":"€","b":[1,{"c":1e+21,"d":0.1}]}')
    end

    it "leaves regular dump behavior driven by the adapter options" do
      require "multi_json/adapters/json_gem"
      described_class.with_adapter(MultiJson::Adapters::JsonGem) do
        expect(described_class.dump({"b" => 1, "a" => 2})).to eq(
          described_class.dump({"b" => 1, "a" => 2})
        )
      end
    end
  end
end
