require "spec_helper"
require "bigdecimal"

RSpec.describe MultiJson::CanonicalDump do
  def canonical_bytes(object)
    MultiJson.canonical_dump(object).b.bytes
  end

  def expected_bytes(json)
    json.b.bytes
  end

  it "returns a UTF-8 encoded string" do
    result = MultiJson.canonical_dump({"a" => 1})
    expect(result.encoding).to eq(Encoding::UTF_8)
    expect(result).to eq(%({"a":1}))
  end

  it "serializes primitives without whitespace" do
    expect(MultiJson.canonical_dump([nil, true, false, "x", 1, [2], {"a" => 3}])).to eq(%([null,true,false,"x",1,[2],{"a":3}]))
  end

  it "serializes symbols as strings" do
    expect(MultiJson.canonical_dump({key: :value})).to eq(%({"key":"value"}))
  end

  describe "object key ordering" do
    it "sorts properties by code point for BMP-only names" do
      expect(MultiJson.canonical_dump({"b" => 1, "a" => 2, "aa" => 3, "a\u0000" => 4})).to eq(%({"a":2,"a\\u0000":4,"aa":3,"b":1}))
    end

    it "sorts non-BMP property names by UTF-16 code units, not UTF-8 bytes" do
      # UTF-16 order: "a" (0061) < "z" (007A) < U+10000 (D800 DC00) < U+E000 (E000).
      # UTF-8 byte order would place U+E000 (EE 80 80) before U+10000 (F0 90 80 80).
      input = {"\u{E000}" => 1, "\u{10000}" => 2, "z" => 3, "a" => 4}
      expect(canonical_bytes(input)).to eq(expected_bytes(%({"a":4,"z":3,"\u{10000}":2,"\u{E000}":1})))
    end

    it "orders supplementary characters by their leading surrogate" do
      # U+1F600 -> D83D DE00, U+20000 -> D840 DC00, U+FFFD -> FFFD.
      input = {"\u{FFFD}" => 1, "\u{20000}" => 2, "\u{1F600}" => 3}
      expect(canonical_bytes(input)).to eq(expected_bytes(%({"\u{1F600}":3,"\u{20000}":2,"\u{FFFD}":1})))
    end
  end

  describe "string serialization" do
    it "escapes only quote, backslash and control characters" do
      input = %("\\\b\f\n\r\t\u{0}\u{1}\u{1F}\u{7F})
      expected = %("\\"\\\\\\b\\f\\n\\r\\t\\u0000\\u0001\\u001f\u{7F}")
      expect(canonical_bytes(input)).to eq(expected_bytes(expected))
    end

    it "emits non-ASCII characters as raw UTF-8" do
      expect(canonical_bytes("héllo \u{1F600}")).to eq(expected_bytes(%("héllo \u{1F600}")))
    end

    it "does not apply Unicode normalization" do
      composed = "\u{00E9}"      # "é" as a single code point
      decomposed = "e\u{0301}"   # "é" as e + combining acute accent
      expect(canonical_bytes(composed)).to eq(expected_bytes(%("\u{00E9}")))
      expect(canonical_bytes(decomposed)).to eq(expected_bytes(%("e\u{0301}")))
      expect(canonical_bytes(composed)).not_to eq(canonical_bytes(decomposed))
    end

    it "transcodes valid non-UTF-8 strings" do
      expect(MultiJson.canonical_dump("é".encode("ISO-8859-1"))).to eq(%("é"))
    end
  end

  describe "number serialization" do
    # Expected values follow ECMAScript Number::toString as mandated by
    # RFC 8785, including its notation boundaries.
    {
      0.0 => "0",
      -0.0 => "0",
      1.0 => "1",
      -1.0 => "-1",
      0.1 => "0.1",
      0.30000000000000004 => "0.30000000000000004",
      3.141592653589793 => "3.141592653589793",
      1e15 => "1000000000000000",
      1e20 => "100000000000000000000",
      1e21 => "1e+21",
      -1e21 => "-1e+21",
      1.5e21 => "1.5e+21",
      1e-6 => "0.000001",
      1e-7 => "1e-7",
      1.5e-7 => "1.5e-7",
      -2.5e-8 => "-2.5e-8",
      Float::MAX => "1.7976931348623157e+308",
      Float::MIN => "2.2250738585072014e-308",
      5e-324 => "5e-324"
    }.each do |float, json|
      it "serializes #{float.inspect} byte-exactly as #{json}" do
        expect(canonical_bytes(float)).to eq(expected_bytes(json))
      end
    end

    it "serializes integers at the exact IEEE 754 boundary" do
      expect(MultiJson.canonical_dump(2**53)).to eq("9007199254740992")
      expect(MultiJson.canonical_dump(-(2**53))).to eq("-9007199254740992")
    end
  end

  describe "rejected values" do
    it "raises for integers outside the exactly representable range" do
      expect { MultiJson.canonical_dump(2**53 + 1) }.to raise_error(MultiJson::CanonicalDumpError)
      expect { MultiJson.canonical_dump(-(2**53) - 1) }.to raise_error(MultiJson::CanonicalDumpError)
      expect { MultiJson.canonical_dump(10**30) }.to raise_error(MultiJson::CanonicalDumpError)
    end

    it "raises for NaN and Infinity" do
      expect { MultiJson.canonical_dump(Float::NAN) }.to raise_error(MultiJson::CanonicalDumpError)
      expect { MultiJson.canonical_dump(Float::INFINITY) }.to raise_error(MultiJson::CanonicalDumpError)
      expect { MultiJson.canonical_dump(-Float::INFINITY) }.to raise_error(MultiJson::CanonicalDumpError)
    end

    it "raises for custom objects instead of coercing them" do
      expect { MultiJson.canonical_dump(Object.new) }.to raise_error(MultiJson::CanonicalDumpError)
      expect { MultiJson.canonical_dump(1..2) }.to raise_error(MultiJson::CanonicalDumpError)
      expect { MultiJson.canonical_dump({"a" => BigDecimal("1.5")}) }.to raise_error(MultiJson::CanonicalDumpError)
    end

    it "raises for invalid Unicode" do
      invalid = "a\xffb".dup.force_encoding(Encoding::UTF_8)
      expect { MultiJson.canonical_dump(invalid) }.to raise_error(MultiJson::CanonicalDumpError)
      expect { MultiJson.canonical_dump({invalid => 1}) }.to raise_error(MultiJson::CanonicalDumpError)
    end

    it "raises for binary strings" do
      expect { MultiJson.canonical_dump("\xFF".b) }.to raise_error(MultiJson::CanonicalDumpError)
    end

    it "raises for non-string object keys" do
      expect { MultiJson.canonical_dump({1 => "a"}) }.to raise_error(MultiJson::CanonicalDumpError)
    end

    it "raises for duplicate keys after symbol conversion" do
      expect { MultiJson.canonical_dump({a: 1, "a" => 2}) }.to raise_error(MultiJson::CanonicalDumpError)
    end
  end

  describe "independence from adapters and options" do
    it "produces identical bytes regardless of the current adapter and dump options" do
      object = {"b" => 1.5, "a" => [true, nil, "x"]}
      expected = %({"a":[true,null,"x"],"b":1.5})

      expect(MultiJson.canonical_dump(object)).to eq(expected)

      MultiJson.dump_options = {pretty: true}
      expect(MultiJson.canonical_dump(object)).to eq(expected)

      MultiJson.with_adapter(:ok_json) do
        expect(MultiJson.canonical_dump(object)).to eq(expected)
      end
    ensure
      MultiJson.dump_options = nil
    end
  end
end
