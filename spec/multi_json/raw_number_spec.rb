require "spec_helper"

begin
  require "bigdecimal"
rescue LoadError
  # bigdecimal is a bundled gem on Ruby >= 3.4 and may not be loadable in
  # every environment; the #to_d examples are skipped in that case.
end

RSpec.describe MultiJson::RawNumber do
  describe ".new" do
    [
      "0",
      "-0",
      "1",
      "-1",
      "42",
      "1.2300",
      "123e-2",
      "1.5e+10",
      "0.5",
      "-0.5E-3",
      "900719925474099312345678901234567890",
      "3.14159265358979323846264338327950288"
    ].each do |literal|
      it "accepts valid JSON number #{literal.inspect}" do
        expect(described_class.new(literal).to_s).to eq(literal)
      end
    end

    [
      "",
      "01",
      "1.",
      ".5",
      "+1",
      "1e",
      "1e+",
      "--1",
      "NaN",
      "Infinity",
      "-Infinity",
      "0x10",
      "1_000",
      "1 2",
      "1,2",
      "abc",
      '"1"',
      "1,true,",
      '1,"x":1}',
      "1; DROP TABLE users",
      nil,
      1.5,
      42,
      {}
    ].each do |literal|
      it "rejects invalid JSON number #{literal.inspect}" do
        expect { described_class.new(literal) }.to raise_error(ArgumentError)
      end
    end

    it "freezes the instance and its text" do
      number = described_class.new("1.5")
      expect(number).to be_frozen
      expect(number.to_s).to be_frozen
    end

    it "copies the given string so external mutation cannot corrupt it" do
      text = "1.5"
      number = described_class.new(text)
      text << "9"
      expect(number.to_s).to eq("1.5")
    end
  end

  describe ".valid?" do
    it "returns true for valid JSON numbers" do
      expect(described_class.valid?("1.2300")).to be(true)
    end

    it "returns false for invalid JSON numbers" do
      expect(described_class.valid?("01")).to be(false)
      expect(described_class.valid?(nil)).to be(false)
      expect(described_class.valid?(1.5)).to be(false)
    end
  end

  describe "identity" do
    it "is not a String and not a Numeric" do
      number = described_class.new("1.5")
      expect(number).not_to be_a(String)
      expect(number).not_to be_a(Numeric)
    end

    it "does not equal a String with the same text" do
      expect(described_class.new("1.5")).not_to eq("1.5")
    end

    it "equals another RawNumber with the same text" do
      expect(described_class.new("1.2300")).to eq(described_class.new("1.2300"))
      expect(described_class.new("1.2300")).to eql(described_class.new("1.2300"))
      expect(described_class.new("1.2300").hash).to eq(described_class.new("1.2300").hash)
    end

    it "distinguishes different lexemes of the same value" do
      expect(described_class.new("1.2300")).not_to eq(described_class.new("123e-2"))
    end
  end

  describe "explicit conversions" do
    it "converts to Integer with #to_i" do
      expect(described_class.new("42").to_i).to eq(42)
      expect(described_class.new("900719925474099312345678901234567890").to_i)
        .to eq(900719925474099312345678901234567890)
    end

    it "truncates toward zero when converting fractional numbers to Integer" do
      expect(described_class.new("1.2300").to_i).to eq(1)
      expect(described_class.new("-1.5").to_i).to eq(-1)
    end

    it "converts to an exact Rational with #to_r" do
      expect(described_class.new("1.2300").to_r).to eq(Rational(123, 100))
      expect(described_class.new("123e-2").to_r).to eq(Rational(123, 100))
      expect(described_class.new("1.5e3").to_r).to eq(Rational(1500, 1))
      expect(described_class.new("-0.5E-3").to_r).to eq(Rational(-1, 2000))
      expect(described_class.new("0").to_r).to eq(Rational(0, 1))
    end

    it "keeps full precision for numbers beyond Float range" do
      literal = "123456789012345678901234567890.123456789"
      expect(described_class.new(literal).to_r).to eq(Rational("123456789012345678901234567890123456789".to_i, 10**9))
    end

    it "converts to BigDecimal with #to_d" do
      skip "bigdecimal is not available in this environment" unless defined?(BigDecimal)

      expect(described_class.new("1.2300").to_d).to eq(BigDecimal("1.23"))
      expect(described_class.new("123e-2").to_d).to eq(BigDecimal("1.23"))
      expect(described_class.new("1.2300").to_d).to be_a(BigDecimal)
    end
  end

  describe "#to_json" do
    it "returns the bare number literal without quotes" do
      expect(described_class.new("1.2300").to_json).to eq("1.2300")
    end
  end

  describe "loading with the OkJson adapter" do
    around do |example|
      MultiJson.with_adapter(:ok_json) { example.run }
    end

    it "preserves the original number text" do
      result = MultiJson.load('{"a": 1.2300, "b": 123e-2}', raw_numbers: true)
      expect(result["a"]).to eq(described_class.new("1.2300"))
      expect(result["b"]).to eq(described_class.new("123e-2"))
      expect(result["a"].to_s).to eq("1.2300")
      expect(result["b"].to_s).to eq("123e-2")
    end

    it "keeps numbers beyond Float precision exact" do
      result = MultiJson.load("[900719925474099312345678901234567890]", raw_numbers: true)
      expect(result.first.to_i).to eq(900719925474099312345678901234567890)
    end

    it "handles deeply nested arrays and objects" do
      result = MultiJson.load('[[[1.2300]], {"k": [123e-2, {"j": 0.5}]}]', raw_numbers: true)
      expect(result[0][0][0]).to eq(described_class.new("1.2300"))
      expect(result[1]["k"][0]).to eq(described_class.new("123e-2"))
      expect(result[1]["k"][1]["j"]).to eq(described_class.new("0.5"))
    end

    it "does not confuse strings with numbers, even escaped ones" do
      # The second string is "1.5" written with a \u0031 escape; both must
      # stay plain Strings, only the bare number becomes a RawNumber.
      result = MultiJson.load('["1.5", "\u0031.5", "a\nb", true, false, null, 1.5]', raw_numbers: true)
      expect(result[0]).to eq("1.5")
      expect(result[0]).to be_a(String)
      expect(result[1]).to eq("1.5")
      expect(result[1]).to be_a(String)
      expect(result[2]).to eq("a\nb")
      expect(result[3]).to be(true)
      expect(result[4]).to be(false)
      expect(result[5]).to be_nil
      expect(result[6]).to eq(described_class.new("1.5"))
    end

    it "loads rootless numbers" do
      expect(MultiJson.load("1.2300", raw_numbers: true)).to eq(described_class.new("1.2300"))
    end

    it "combines with :symbolize_keys" do
      result = MultiJson.load('{"a": {"b": 1.2300}}', raw_numbers: true, symbolize_keys: true)
      expect(result).to eq({a: {b: described_class.new("1.2300")}})
    end

    it "leaves default parsing untouched" do
      expect(MultiJson.load("[1.2300]").first).to eq(1.23)
      expect(MultiJson.load("[42]").first).to eq(42)
    end

    ["01", "1.", ".5", "+1", "1e", "1e+", "--1", "NaN", "Infinity", "0x10", "1_000", "1 2"].each do |literal|
      it "raises MultiJson::ParseError on invalid number #{literal.inspect}" do
        expect { MultiJson.load("[#{literal}]", raw_numbers: true) }.to raise_error(MultiJson::ParseError)
      end
    end
  end

  describe "dumping with the OkJson adapter" do
    around do |example|
      MultiJson.with_adapter(:ok_json) { example.run }
    end

    it "writes the number bare, without string quotes" do
      expect(MultiJson.dump(described_class.new("1.2300"))).to eq("1.2300")
    end

    it "writes nested numbers bare" do
      number = described_class.new("123e-2")
      expect(MultiJson.dump({"n" => [number, "1.5"]})).to eq('{"n":[123e-2,"1.5"]}')
    end

    it "round-trips the exact lexeme" do
      json = '{"a": 1.2300, "b": [123e-2]}'
      loaded = MultiJson.load(json, raw_numbers: true)
      expect(MultiJson.dump(loaded)).to eq('{"a":1.2300,"b":[123e-2]}')
    end

    it "cannot be used to inject arbitrary text" do
      expect { described_class.new('1,"x":1}') }.to raise_error(ArgumentError)
      expect { described_class.new("1,true,") }.to raise_error(ArgumentError)
    end
  end
end
