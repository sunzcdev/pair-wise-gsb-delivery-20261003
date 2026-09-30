# frozen_string_literal: true

RSpec.describe "lossless numeric types" do
  def expect_coercion_error(type, input)
    expect { type.call(input) }.to raise_error(Dry::Types::CoercionError)
  end

  describe Dry::Types::Coercions::Lossless do
    describe ".to_int" do
      it "is the function behind lossless.integer" do
        method_object = described_class.method(:to_int)
        expect(Dry::Types["lossless.integer"].fn.target).to be(method_object.receiver)
        expect(Dry::Types["lossless.integer"].fn.name).to be(:to_int)
      end
    end
  end

  describe "lossless.integer" do
    subject(:type) { Dry::Types["lossless.integer"] }

    context "value-preserving conversions (round-trip equals the input)" do
      [
        ["decimal strings", "3", 3],
        ["signed strings", "-17", -17],
        ["positive sign", "+42", 42],
        ["zero string", "0", 0],
        ["negative zero string", "-0", 0],
        ["leading zeros stay base 10", "010", 10],
        ["plain integers", 123, 123],
        ["negative integers", -9, -9],
        ["bignums stay bignums", 10**100, 10**100],
        ["integral floats", 3.0, 3],
        ["negative integral floats", -3.0, -3],
        ["largest consecutive binary64 integer", (2**53).to_f, 2**53],
        ["an integral float input converts with no loss", ((2**53) + 1).to_f, 2**53],
        ["exactly represented large float", 1e20, 10**20],
        ["integral BigDecimal", BigDecimal("3.00"), 3],
        ["integral scientific BigDecimal", BigDecimal("1.5e2"), 150],
        ["bignum BigDecimal", BigDecimal(10**100), 10**100]
      ].each do |label, input, output|
        it "accepts #{label}: #{input.inspect}" do
          result = type.call(input)
          expect(result).to eql(output)
          expect(result).to be_a(Integer)
          # the inverse conversion must recover the mathematical value
          expect(result).to eq(input) if input.is_a?(Integer)
        end
      end
    end

    context "conversions that must fail" do
      [
        "3.7",      # fraction must not be truncated to 3
        "-0.5",
        "3.0",      # a decimal string is not an integer literal
        "",
        "abc",
        "3abc",
        " 3",       # no surrounding whitespace
        "1_000",    # no separators
        "0x10",     # no other bases
        "1e3",      # no exponent notation
        3.7,        # Float#to_i would truncate
        -0.5,
        1.5,
        -0.0,       # negative zero has no integer representation
        Float::NAN,
        Float::INFINITY,
        -Float::INFINITY,
        BigDecimal("3.7"),
        BigDecimal("-0.0"), # negative zero
        BigDecimal("NaN"),
        BigDecimal("Infinity"),
        BigDecimal("-Infinity"),
        nil,
        true,
        Object.new,
        [],
        {}
      ].each do |input|
        it "rejects #{input.inspect}" do
          expect_coercion_error(type, input)
        end
      end

      it "keeps a bignum input exactly (integers never go through Float)" do
        exact = (2**53) + 1
        expect(exact.to_f.to_i).not_to eq(exact) # proof the float path would lose it
        expect(type.call(exact)).to be(exact)
      end
    end

    it "returns a Failure instead of raising with #try" do
      result = type.try("3.7")
      expect(result).to be_failure
      expect(result.error).to be_a(Dry::Types::CoercionError)
    end

    it "calls the failure block in safe mode" do
      expect(type.call("3.7") { :fallback }).to be(:fallback)
    end
  end

  describe "lossless.float" do
    subject(:type) { Dry::Types["lossless.float"] }

    context "value-preserving conversions" do
      [
        ["binary-exact decimal string", "3.5", 3.5],
        ["zero string", "0", 0.0],
        ["zero float string", "0.0", 0.0],
        ["negative exact fraction", "-0.25", -0.25],
        ["exponent notation", "1.5e2", 150.0],
        ["capital exponent", "1.5E2", 150.0],
        ["leading-dot fraction", ".5", 0.5],
        ["plain float identity", 3.7, 3.7],
        ["integral float identity", 3.0, 3.0],
        ["integer exactly in binary64", 3, 3.0],
        ["small integer", (2**53), (2**53).to_f],
        ["exact decimal power of ten", 10**20, 1e20],
        ["binary-exact BigDecimal", BigDecimal("3.5"), 3.5],
        ["integral BigDecimal", BigDecimal("3"), 3.0],
        ["scientific exact BigDecimal", BigDecimal("1.5e2"), 150.0]
      ].each do |label, input, output|
        it "accepts #{label}: #{input.inspect}" do
          result = type.call(input)
          expect(result).to eql(output)
          expect(result).to be_a(Float)
          expect(result.finite?).to be(true)
        end
      end

      it "returns the same Float object for a binary-exact float input" do
        input = 3.7
        expect(type.call(input)).to equal(input)
      end
    end

    context "conversions that must fail" do
      [
        "3.7",   # 3.7 is not exactly representable in binary64
        "0.1",
        "1.1",
        "-0.0",  # negative zero
        "nan",
        "Infinity",
        "-Infinity",
        "",
        "abc",
        "0x1p4", # hex float is not a decimal string
        "1_000.0",
        " 3.5 ",
        "3.",    # digits required after the point
        ".",
        (2**53) + 1, # rounds to 2**53
        -0.0,
        Float::NAN,
        Float::INFINITY,
        -Float::INFINITY,
        BigDecimal("3.7"),    # not exactly representable
        BigDecimal("0.1"),
        BigDecimal("1e400"),  # would become Infinity
        BigDecimal("-0.0"),
        BigDecimal("NaN"),
        BigDecimal("Infinity"),
        nil,
        Object.new
      ].each do |input|
        it "rejects #{input.inspect}" do
          expect_coercion_error(type, input)
        end
      end

      # These conversions overflow to Infinity. The library suppresses the
      # corresponding Kernel warnings itself and rejects the values.
      it "rejects a decimal string that overflows binary64 (1e400)" do
        expect_coercion_error(type, "1e400")
      end

      it "rejects a huge integer that overflows binary64 (10**400)" do
        expect_coercion_error(type, 10**400)
      end

      it "does not accept the nearest float approximation of a decimal string" do
        # Float("3.7") quietly produces a value that is not 3.7
        approximate = Float("3.7")
        expect(approximate.to_r).not_to eq(BigDecimal("3.7"))
        expect_coercion_error(type, "3.7")
      end
    end

    it "returns a Failure with #try" do
      expect(type.try("3.7")).to be_failure
    end
  end

  describe "lossless.decimal" do
    subject(:type) { Dry::Types["lossless.decimal"] }

    context "value-preserving conversions" do
      [
        ["fractional string", "3.7", BigDecimal("3.7")],
        ["high precision string", "0.66666666666666666667",
         BigDecimal("0.66666666666666666667")],
        ["integer string", "3", BigDecimal("3")],
        ["exponent string", "1.5e2", BigDecimal("150")],
        ["integer", 3, BigDecimal("3")],
        ["bignum", 10**100, BigDecimal(10**100)],
        ["BigDecimal identity", BigDecimal("3.7"), BigDecimal("3.7")],
        ["scientific BigDecimal", BigDecimal("1.5e2"), BigDecimal("150")]
      ].each do |label, input, output|
        it "accepts #{label}: #{input.inspect}" do
          expect(type.call(input)).to eql(output)
        end
      end

      it "materializes the exact value of a finite Float (no Float::DIG rounding)" do
        # The familiar Float#to_d rounds and is provably lossy:
        expect(3.7.to_d).not_to eq(3.7.to_r)

        result = type.call(3.7)
        expect(result).to be_a(BigDecimal)
        expect(result.to_r).to eq(3.7.to_r)
        expect(result).not_to eql(BigDecimal("3.7")) # exact binary value, not the decimal spelling
      end

      it "keeps full precision for the 1/3 float" do
        input = 1.0 / 3
        result = type.call(input)
        expect(result.to_r).to eq(input.to_r)
      end

      it "accepts positive zero" do
        expect(type.call("0")).to eql(BigDecimal("0"))
        expect(type.call(0.0)).to eql(BigDecimal("0"))
      end
    end

    context "conversions that must fail" do
      [
        "",
        "abc",
        "3.7.0",
        "1_000",
        "0x10",
        "Infinity",
        "NaN",
        "-0.0", # negative zero
        -0.0,
        Float::NAN,
        Float::INFINITY,
        -Float::INFINITY,
        BigDecimal("-0.0"),
        BigDecimal("NaN"),
        BigDecimal("Infinity"),
        BigDecimal("-Infinity"),
        nil,
        Object.new,
        true
      ].each do |input|
        it "rejects #{input.inspect}" do
          expect_coercion_error(type, input)
        end
      end
    end

    it "returns a Failure with #try" do
      expect(type.try(Float::NAN)).to be_failure
    end
  end

  describe "inverse (round-trip) proof across the numeric matrix" do
    integer = Dry::Types["lossless.integer"]
    float = Dry::Types["lossless.float"]
    decimal = Dry::Types["lossless.decimal"]

    # Every successful conversion must be reversible: convert the output back
    # to the input representation and compare mathematical values.
    specify "integer -> decimal -> integer" do
      [0, 1, -17, 10**100].each do |n|
        d = decimal.call(n)
        expect(integer.call(d)).to eq(n)
      end
    end

    specify "integer <-> float only when binary-exact" do
      n = 10**20
      expect(integer.call(float.call(n))).to eq(n)

      expect { float.call((2**53) + 1) }.to raise_error(Dry::Types::CoercionError)
    end

    specify "float -> decimal -> float recovers the same binary64 value" do
      [3.7, 1.0 / 3, 1e100, Float::MIN].each do |f|
        d = decimal.call(f)
        back = float.call(d)
        expect(back).to eql(f)
      end
    end

    specify "decimal string -> float only for binary-exact decimals" do
      expect(float.call("3.5")).to eql(3.5)
      expect { float.call("3.7") }.to raise_error(Dry::Types::CoercionError)
    end

    specify "every decimal string survives in a BigDecimal" do
      %w[3.7 0.1 0.66666666666666666667 1e400].each do |s|
        expect(decimal.call(s).to_s("F")).to eq(BigDecimal(s).to_s("F"))
      end
    end
  end

  describe "composition: the guarantee is not bypassable" do
    let(:integer) { Dry::Types["lossless.integer"] }
    let(:float) { Dry::Types["lossless.float"] }
    let(:decimal) { Dry::Types["lossless.decimal"] }

    it "works inside an array member" do
      array = Dry::Types["array"].of(integer)
      expect(array.(["1", "2", 3.0])).to eql([1, 2, 3])
      expect { array.(["3.7"]) }.to raise_error(Dry::Types::CoercionError)

      result = array.try(["1", "3.7"])
      expect(result).to be_failure
    end

    it "works inside a hash schema" do
      schema = Dry::Types["hash"].schema(amount: decimal, qty: integer, rate: float)

      expect(schema.(amount: "3.7", qty: "2", rate: "0.5"))
        .to eql(amount: BigDecimal("3.7"), qty: 2, rate: 0.5)

      expect { schema.(amount: "3.7", qty: "2", rate: "0.1") }
        .to raise_error(Dry::Types::CoercionError)
    end

    it "works in a sum type and reports failure when neither branch fits" do
      sum = integer | float
      expect(sum.("3.0")).to eql(3.0)  # float branch takes the integral decimal
      expect(sum.(3)).to eql(3)
      expect { sum.("3.7") }.to raise_error(Dry::Types::CoercionError)
    end

    it "works as an optional sum with nil" do
      type = integer.optional
      expect(type.(nil)).to be_nil
      expect(type.("42")).to be(42)
      expect { type.("3.7") }.to raise_error(Dry::Types::CoercionError)
    end

    it "is registered as optional.lossless.*" do
      expect(Dry::Types["optional.lossless.float"].(nil)).to be_nil
      expect(Dry::Types["optional.lossless.float"].("3.5")).to eql(3.5)
      expect { Dry::Types["optional.lossless.float"].("3.7") }
        .to raise_error(Dry::Types::CoercionError)
    end

    it "survives an outer .constrained" do
      positive = integer.constrained(gt: 0)
      expect(positive.("5")).to be(5)
      expect { positive.("3.7") }.to raise_error(Dry::Types::CoercionError)
      expect { positive.("-1") }.to raise_error(Dry::Types::ConstraintError)
    end

    it "survives an outer appended constructor" do
      decorated = integer.constructor { |input, type| type.(input) * 2 }
      expect(decorated.("4")).to be(8)
      expect { decorated.("3.7") }.to raise_error(Dry::Types::CoercionError)
    end

    it "cannot be bypassed by chaining another constructor" do
      # The appended constructor receives the already-coerced value; it never
      # sees a chance to damage it.
      identity = ->(value) { value }
      type = integer.constructor(identity).constructor(identity)
      expect(type.("42")).to be(42)
      expect { type.("3.7") }.to raise_error(Dry::Types::CoercionError)
    end

    it "enforces the check even when a prepended constructor feeds it values" do
      # prepend runs before the type, but whenever it delegates to the type
      # the lossless rule still applies
      type = integer.prepend { |input| input.is_a?(Integer) ? input.to_s : input }
      expect(type.(7)).to be(7)
      expect { type.(3.7) }.to raise_error(Dry::Types::CoercionError)
    end

    it "survives .default and .fallback" do
      with_default = integer.default(0)
      expect(with_default.()).to be(0)
      expect(with_default.("7")).to be(7)
      expect { with_default.("3.7") }.to raise_error(Dry::Types::CoercionError)

      with_fallback = integer.fallback(-1)
      expect(with_fallback.("3.7")).to be(-1)
      expect(with_fallback.("7")).to be(7)
    end

    it "survives lax composition (failures yield the original input)" do
      lax = integer.lax
      expect(lax.("3.7")).to eql("3.7")
      expect(lax.("4")).to be(4)
    end

    it "stays lossless inside dry-schema-style nested structures" do
      schema = Dry::Types["hash"].schema(
        lines: Dry::Types["array"].of(
          Dry::Types["hash"].schema(amount: decimal)
        )
      )

      input = {lines: [{amount: "3.7"}, {amount: "10.25"}]}
      expect(schema.(input)).to eql(
        lines: [{amount: BigDecimal("3.7")}, {amount: BigDecimal("10.25")}]
      )

      bad = {lines: [{amount: "x"}]}
      expect { schema.(bad) }.to raise_error(Dry::Types::CoercionError)
    end
  end

  describe "existing coercible defaults stay unchanged" do
    specify "coercible types keep using Kernel coercion" do
      expect(Dry::Types["coercible.integer"].(3)).to be(3)
      expect(Dry::Types["coercible.integer"].(3.0)).to be(3)
      expect(Dry::Types["coercible.float"].("3.7")).to eql(3.7)
    end

    specify "params types keep their current behavior" do
      expect(Dry::Types["params.integer"].("312")).to be(312)
      expect(Dry::Types["params.float"].("3.12")).to eql(3.12)
      expect(Dry::Types["params.decimal"].("3.12")).to eql(BigDecimal("3.12"))
      # params.decimal rounds floats to Float::DIG (the old, lossy behavior)
      expect(Dry::Types["params.decimal"][3.12]).to eql(BigDecimal("3.12"))
    end

    specify "json.decimal keeps its current behavior" do
      expect(Dry::Types["json.decimal"].("3.12")).to eql(BigDecimal("3.12"))
      expect(Dry::Types["json.decimal"][3.12]).to eql(BigDecimal("3.12"))
    end

    specify "nominal and strict numeric types do not coerce" do
      expect(Dry::Types["nominal.integer"].("3")).to eql("3")
      expect { Dry::Types["strict.integer"].("3") }
        .to raise_error(Dry::Types::ConstraintError)
    end
  end

  describe "module export" do
    it "is available via Dry.Types(:lossless)" do
      mod = Dry.Types(:lossless)
      expect(mod::Lossless::Integer).to be(Dry::Types["lossless.integer"])
      expect(mod::Lossless::Float).to be(Dry::Types["lossless.float"])
      expect(mod::Lossless::Decimal).to be(Dry::Types["lossless.decimal"])
    end

    it "rejects unknown namespace typos" do
      expect { Dry.Types(:losless) }.to raise_error(ArgumentError, /lossless/)
    end
  end
end
