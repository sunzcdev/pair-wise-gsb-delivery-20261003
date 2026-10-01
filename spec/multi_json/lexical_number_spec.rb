require "spec_helper"
require "json"
require "bigdecimal"

RSpec.describe MultiJson::LexicalNumber do
  subject(:number) { described_class.new("1.2300") }

  describe ".new" do
    it "accepts every shape of legal JSON number" do
      %w[
        0 -0 1 -1 12345678901234567890 0.5 -0.25 1.0
        1e10 1E10 1e-10 1E+10 1.20e3 123e-2 0e0
      ].each do |text|
        expect(described_class.new(text).lexeme).to eq(text)
      end
    end

    %w[
      01 00 -01 +1 .5 5. 1e +e 1E+
      NaN Infinity -Infinity 0x10 1_000
      1.2.3 1ef 1,2 --1
    ].each do |text|
      it "rejects non-numeric text #{text.inspect}" do
        expect { described_class.new(text) }.to raise_error(ArgumentError, /not a valid JSON number/)
      end
    end

    it "rejects whitespace-separated digits" do
      expect { described_class.new("1 2") }.to raise_error(ArgumentError)
    end

    it "rejects text that tries to inject JSON structure" do
      ["1.2\",\"x\":0", "1.2]x", "1);alert(1", "1\n2"].each do |text|
        expect { described_class.new(text) }.to raise_error(ArgumentError)
      end
    end

    it "rejects objects that are not string-like" do
      expect { described_class.new(42) }.to raise_error(TypeError)
      expect { described_class.new(nil) }.to raise_error(TypeError)
    end

    it "does not hold on to the caller's mutable string" do
      source = +"1.20"
      value = described_class.new(source)
      source << "999"
      expect(value.lexeme).to eq("1.20")
    end

    it "is immutable" do
      expect(number).to be_frozen
      expect(number.lexeme).to be_frozen
    end
  end

  describe ".[]" do
    it "is an alias for construction" do
      expect(described_class["1.5"]).to eq(described_class.new("1.5"))
    end
  end

  describe "#lexeme / #to_s" do
    it "exposes the exact original spelling" do
      expect(number.lexeme).to eq("1.2300")
      expect(number.to_s).to eq("1.2300")
    end
  end

  describe "#to_r" do
    {
      "123" => Rational(123),
      "-0" => Rational(0),
      "1.2300" => Rational(123, 100),
      "123e-2" => Rational(123, 100),
      "1e3" => Rational(1000),
      "2.5e2" => Rational(250),
      "0.0001" => Rational(1, 10_000)
    }.each do |text, expected|
      it "converts #{text} exactly to #{expected}" do
        expect(described_class.new(text).to_r).to eq(expected)
      end
    end

    it "stays exact beyond Float precision" do
      value = described_class.new("9007199254740993").to_r
      expect(value).to eq(Rational(9_007_199_254_740_993))
    end
  end

  describe "#to_d" do
    it "builds a BigDecimal from the untouched lexeme" do
      expect(number.to_d).to eq(BigDecimal("1.2300"))
      expect(described_class.new("3.1415926535897932384626433832795").to_d)
        .to eq(BigDecimal("3.1415926535897932384626433832795"))
    end
  end

  describe "#to_i" do
    it "returns the integer when the value is integral" do
      expect(described_class.new("123").to_i).to eq(123)
      expect(described_class.new("1e3").to_i).to eq(1000)
      expect(described_class.new("4.0").to_i).to eq(4)
    end

    it "raises RangeError rather than truncating a fraction" do
      expect { described_class.new("1.5").to_i }.to raise_error(RangeError, /not an integer/)
      expect { described_class.new("1.999").to_i }.to raise_error(RangeError)
    end
  end

  describe "#integer?" do
    it "is true only for mathematical integers" do
      expect(described_class.new("123")).to be_integer
      expect(described_class.new("1e3")).to be_integer
      expect(described_class.new("4.0")).to be_integer
      expect(described_class.new("1.5")).not_to be_integer
    end
  end

  describe "#to_f" do
    it "converts (and may round, like Float)" do
      expect(described_class.new("1.5").to_f).to eq(1.5)
    end
  end

  describe "equality" do
    it "compares exact values with ==" do
      expect(described_class.new("1.0")).to eq(described_class.new("1.00"))
      expect(described_class.new("1")).to eq(1)
      expect(1).to eq(described_class.new("1"))
      expect(described_class.new("1.5")).to eq(1.5)
      expect(described_class.new("1.5")).not_to eq("1.5")
      expect(described_class.new("1.5")).not_to eq(described_class.new("1.6"))
    end

    it "uses the exact spelling for eql? and hash identity" do
      expect(described_class.new("1.0")).to eql(described_class.new("1.0"))
      expect(described_class.new("1.0")).not_to eql(described_class.new("1.00"))
      expect(described_class.new("1.0").hash).to eq(described_class.new("1.0").hash)
      expect(described_class.new("1.0").hash).not_to eq(described_class.new("1.00").hash)

      table = {described_class.new("1.0") => :a, described_class.new("1.00") => :b}
      expect(table.length).to eq(2)
      expect(table[described_class.new("1.0")]).to eq(:a)
    end
  end

  describe "#<=>" do
    it "orders numerically" do
      expect(described_class.new("1.5") <=> described_class.new("2.0")).to eq(-1)
      expect(described_class.new("2") <=> 2).to eq(0)
      expect(described_class.new("3") <=> 1).to eq(1)
      expect([described_class.new("1e2"), described_class.new("9")].sort)
        .to eq([described_class.new("9"), described_class.new("1e2")])
    end
  end

  describe "#inspect" do
    it "shows the class and spelling" do
      expect(described_class.new("1.20").inspect).to eq("#<MultiJson::LexicalNumber:1.20>")
    end
  end

  describe "serialization" do
    it "is not a String and does not respond to to_str" do
      expect(number).not_to be_a(String)
      expect(number).not_to respond_to(:to_str)
    end

    it "is a Numeric" do
      expect(number).to be_a(Numeric)
    end

    it "emits the raw number through to_json without quotes" do
      expect(number.to_json).to eq("1.2300")
      expect(::JSON.dump(number)).to eq("1.2300")
      expect(::JSON.dump("a" => [number, "x"])).to eq('{"a":[1.2300,"x"]}')
    end

    it "keeps the emitted text valid JSON even with exponent and minus" do
      expect(::JSON.parse(described_class.new("-1.20e-3").to_json)).to eq(-0.0012)
    end
  end
end
