# frozen_string_literal: true

RSpec.describe "lossless numeric types" do
  let(:integer) { Dry::Types["lossless.integer"] }
  let(:float) { Dry::Types["lossless.float"] }
  let(:decimal) { Dry::Types["lossless.decimal"] }

  describe "lossless.integer" do
    subject(:type) { integer }

    it_behaves_like "a constrained type", inputs: [
      Object.new, [], {}, 3.7, "3.7", "3.0", "foo", "23asf",
      BigDecimal("3.7"), Float::NAN, Float::INFINITY, -Float::INFINITY
    ]

    describe "representations that are accepted" do
      it "passes Integer values through unchanged" do
        expect(type[3]).to be(3)
        expect(type[-17]).to be(-17)
        expect(type[0]).to be(0)
      end

      it "preserves integers of arbitrary magnitude" do
        huge = 10**100
        expect(type[huge]).to eq(huge)
        expect(type[-huge]).to eq(-huge)
      end

      it "accepts integer-valued floats (e.g. 3.0), changing only the representation" do
        expect(type[3.0]).to eql(3)
        expect(type[-3.0]).to eql(-3)
      end

      it "accepts integer-valued floats at the exact binary64 integral boundary" do
        expect(type[(2.0**53)]).to eql(2**53)
        expect(type[0.0]).to eql(0)
      end

      it "accepts integer-valued BigDecimals" do
        expect(type[BigDecimal("3")]).to eql(3)
        expect(type[BigDecimal("3.0")]).to eql(3)
        expect(type[BigDecimal("-17.00")]).to eql(-17)
        expect(type[BigDecimal("1E100")]).to eql(10**100)
      end

      it "parses strict base-10 integer strings" do
        expect(type["3"]).to eql(3)
        expect(type["-17"]).to eql(-17)
        expect(type["+3"]).to eql(3)
        expect(type["0"]).to eql(0)
        expect(type["010"]).to eql(10)
        expect(type["  3 "]).to eql(3)
      end
    end

    describe "values that must fail" do
      it "does not truncate 3.7 into 3" do
        expect { type[3.7] }.to raise_error(
          Dry::Types::CoercionError, /losslessly coerced to integer/
        )
        expect(type.(3.7) { :fallback }).to be(:fallback)
      end

      it "rejects fractional floats even when they round to an integer" do
        expect { type[3.14] }.to raise_error(Dry::Types::CoercionError)
        expect { type[-0.5] }.to raise_error(Dry::Types::CoercionError)
      end

      it "rejects fractional BigDecimals" do
        expect { type[BigDecimal("3.7")] }.to raise_error(Dry::Types::CoercionError)
        expect { type[BigDecimal("0.001")] }.to raise_error(Dry::Types::CoercionError)
      end

      it "rejects strings carrying a fraction even if the fraction is zero" do
        expect { type["3.7"] }.to raise_error(Dry::Types::CoercionError)
        expect { type["3.0"] }.to raise_error(Dry::Types::CoercionError)
        expect { type["1_000.0"] }.to raise_error(Dry::Types::CoercionError)
      end

      it "rejects non-numeric strings" do
        expect { type["foo"] }.to raise_error(Dry::Types::CoercionError)
        expect { type["23asf"] }.to raise_error(Dry::Types::CoercionError)
        expect { type[""] }.to raise_error(Dry::Types::CoercionError)
      end

      it "rejects non-finite values" do
        expect { type[Float::NAN] }.to raise_error(Dry::Types::CoercionError)
        expect { type[Float::INFINITY] }.to raise_error(Dry::Types::CoercionError)
        expect { type[-Float::INFINITY] }.to raise_error(Dry::Types::CoercionError)
      end

      it "rejects non-numeric objects" do
        expect { type[Object.new] }.to raise_error(Dry::Types::CoercionError)
        expect { type.call([]) }.to raise_error(Dry::Types::CoercionError)
        expect { type.call({}) }.to raise_error(Dry::Types::CoercionError)
        expect { type.call(:three) }.to raise_error(Dry::Types::CoercionError)
      end

      it "reports the failure through #try" do
        result = type.try(3.7)
        expect(result).to be_failure
        expect(result.error).to be_a(Dry::Types::CoercionError)
      end
    end
  end

  describe "lossless.float" do
    subject(:type) { float }

    it_behaves_like "a constrained type", inputs: [
      Object.new, [], {}, "foo", "23asd", "3.14", "1e400", "NaN", "Infinity",
      2**53 + 1, 10**100, BigDecimal("3.14"), BigDecimal("1e400")
    ]

    describe "representations that are accepted" do
      it "passes finite Float values through" do
        expect(type[3.14]).to eql(3.14)
        expect(type[3.7]).to eql(3.7)
        expect(type[0.0]).to eql(0.0)
        expect(type[-2.5]).to eql(-2.5)
      end

      it "accepts decimal strings exactly representable as binary64" do
        expect(type["0.5"]).to eql(0.5)
        expect(type["0.25"]).to eql(0.25)
        expect(type["-0.125"]).to eql(-0.125)
        expect(type["3.0"]).to eql(3.0)
        expect(type["0"]).to eql(0.0)
        expect(type["1_000.5"]).to eql(1000.5)
      end

      it "accepts integers exactly representable as binary64" do
        expect(type[0]).to eql(0.0)
        expect(type[100]).to eql(100.0)
        expect(type[-(2**53)]).to eql(-(2**53).to_f)
      end

      it "accepts every integer up to 2**53 in magnitude" do
        expect(type[2**53]).to eql((2**53).to_f)
        expect(type[-(2**53)]).to eql(-(2**53).to_f)
      end

      it "accepts larger integers that happen to be exactly representable (powers of two)" do
        expect(type[2**100]).to eql((2**100).to_f)
        expect(type[2**1023]).to eql((2**1023).to_f)
      end

      it "accepts BigDecimals that are exactly representable as binary64" do
        expect(type[BigDecimal("0.5")]).to eql(0.5)
        expect(type[BigDecimal("3.0")]).to eql(3.0)
        expect(type[BigDecimal("-1000.25")]).to eql(-1000.25)
      end

      it "is reversible for every accepted value" do
        ["0.5", "0.25", "-0.125", "1000.0"].each do |literal|
          expect(type[literal].to_s).to eq(Float(literal).to_s)
        end
      end

      it "accepts exact values written with an exponent" do
        expect(type["1e3"]).to eql(1000.0)
        expect(type["1.5e2"]).to eql(150.0)
        expect(type["0.1e1"]).to eql(1.0)
      end

      it "round-trips an accepted integer exactly" do
        value = type[2**53]
        expect(value.to_i).to eql(2**53)
        expect(value).to eql(2**53.to_f)
      end
    end

    describe "values that must fail" do
      it "rejects decimal strings that are not exactly representable as binary64" do
        expect { type["3.14"] }.to raise_error(
          Dry::Types::CoercionError, /losslessly coerced to float/
        )
        expect { type["0.1"] }.to raise_error(Dry::Types::CoercionError)
        expect { type["3.7"] }.to raise_error(Dry::Types::CoercionError)
        expect(type.("3.14") { :fallback }).to be(:fallback)
      end

      it "rejects the first integer beyond the exact-integer boundary" do
        expect { type[2**53 + 1] }.to raise_error(Dry::Types::CoercionError)
        expect { type[-(2**53 + 1)] }.to raise_error(Dry::Types::CoercionError)
      end

      it "keeps 2**53 reversible but 2**53+1 lossy" do
        expect((2**53).to_f).to eq(2**53)
        expect((2**53 + 1).to_f).not_to eq(2**53 + 1)
      end

      it "rejects decimal literals that fail even though they parse as floats" do
        # 0.1 parses as a finite float but changes value in binary64
        expect(Float("0.1").to_r).not_to eq(BigDecimal("0.1").to_r)
        expect { type["0.1"] }.to raise_error(Dry::Types::CoercionError)
      end

      it "rejects large integers that silently change value in Float" do
        big = 10**100
        expect(big.to_f).not_to eq(big)
        expect { type[big] }.to raise_error(Dry::Types::CoercionError)
      end

      it "rejects BigDecimals lacking an exact binary64 representation" do
        expect { type[BigDecimal("3.14")] }.to raise_error(Dry::Types::CoercionError)
        expect { type[BigDecimal("1e300")] }.to raise_error(Dry::Types::CoercionError)
      end

      it "rejects values that overflow binary64 regardless of the source" do
        expect { type["1e400"] }.to raise_error(Dry::Types::CoercionError)
        expect { type[BigDecimal("1e400")] }.to raise_error(Dry::Types::CoercionError)
        expect { type[2**1024] }.to raise_error(Dry::Types::CoercionError)
      end

      it "rejects non-finite floats" do
        expect { type[Float::NAN] }.to raise_error(Dry::Types::CoercionError)
        expect { type[Float::INFINITY] }.to raise_error(Dry::Types::CoercionError)
        expect { type[-Float::INFINITY] }.to raise_error(Dry::Types::CoercionError)
      end

      it "rejects non-finite string literals" do
        expect { type["NaN"] }.to raise_error(Dry::Types::CoercionError)
        expect { type["Infinity"] }.to raise_error(Dry::Types::CoercionError)
        expect { type["-Infinity"] }.to raise_error(Dry::Types::CoercionError)
      end

      it "rejects non-numeric objects" do
        expect { type[Object.new] }.to raise_error(Dry::Types::CoercionError)
        expect { type.call(:one) }.to raise_error(Dry::Types::CoercionError)
      end
    end

    describe "negative zero" do
      it "normalizes -0.0 to 0.0" do
        expect(type[-0.0]).to eql(0.0)
        expect(1.0 / type[-0.0]).to eql(Float::INFINITY)
      end

      it "normalizes a -0.0 literal to 0.0" do
        expect(type["-0.0"]).to eql(0.0)
        expect(1.0 / type["-0.0"]).to eql(Float::INFINITY)
      end

      it "keeps positive zero" do
        expect(type["0.0"]).to eql(0.0)
      end
    end
  end

  describe "lossless.decimal" do
    subject(:type) { decimal }

    it_behaves_like "a constrained type", inputs: [
      Object.new, [], {}, "foo", "23asf",
      Float::NAN, Float::INFINITY, -Float::INFINITY,
      BigDecimal("NaN"), BigDecimal("Infinity"), BigDecimal("-Infinity")
    ]

    describe "representations that are accepted" do
      it "passes finite BigDecimal values through" do
        expect(type[BigDecimal("3.7")]).to eql(BigDecimal("3.7"))
        expect(type[BigDecimal("-17.01")]).to eql(BigDecimal("-17.01"))
      end

      it "converts integers exactly regardless of magnitude" do
        expect(type[3]).to eql(BigDecimal("3"))
        expect(type[-17]).to eql(BigDecimal("-17"))
        huge = 10**100
        expect(type[huge]).to eql(BigDecimal(huge.to_s))
        expect(type[10**10_000]).to eql(BigDecimal("1e10000"))
      end
      it "parses decimal strings preserving all significant digits" do
        expect(type["3.7"]).to eql(BigDecimal("3.7"))
        expect(type["0.0001"]).to eql(BigDecimal("0.0001"))
        expect(type["1_000.5"]).to eql(BigDecimal("1000.5"))
        expect(type["1e9999"]).to eql(BigDecimal("1e9999"))
      end

      it "does not lose precision of long decimal literals" do
        literal = "0.66666666666666666667"
        expect(type[literal]).to eql(BigDecimal(literal))
      end

      it "converts finite floats when the conversion round-trips" do
        expect(type[3.14]).to eql(BigDecimal("3.14"))
        expect(type[0.5]).to eql(BigDecimal("0.5"))
        result = type[3.14]
        expect(result.to_f).to eql(3.14)
      end
    end

    describe "values that must fail" do
      it "rejects non-finite floats" do
        expect { type[Float::NAN] }.to raise_error(Dry::Types::CoercionError)
        expect { type[Float::INFINITY] }.to raise_error(Dry::Types::CoercionError)
        expect { type[-Float::INFINITY] }.to raise_error(Dry::Types::CoercionError)
      end

      it "rejects non-finite BigDecimals" do
        expect { type[BigDecimal("NaN")] }.to raise_error(Dry::Types::CoercionError)
        expect { type[BigDecimal("Infinity")] }.to raise_error(Dry::Types::CoercionError)
        expect { type[BigDecimal("-Infinity")] }.to raise_error(Dry::Types::CoercionError)
      end

      it "rejects strings that do not parse as decimals" do
        expect { type["foo"] }.to raise_error(Dry::Types::CoercionError)
        expect { type["23asf"] }.to raise_error(Dry::Types::CoercionError)
        expect { type["3."] }.to raise_error(Dry::Types::CoercionError)
      end

      it "rejects non-numeric objects" do
        expect { type[Object.new] }.to raise_error(Dry::Types::CoercionError)
        expect { type.call(:three) }.to raise_error(Dry::Types::CoercionError)
      end
    end

    describe "negative zero" do
      it "normalizes negative zero BigDecimal to positive zero" do
        expect(type[BigDecimal("-0")]).to eql(BigDecimal("0"))
        expect(type[BigDecimal("-0")].sign).to eql(BigDecimal("0").sign)
      end

      it "normalizes -0.0 float to positive zero decimal" do
        expect(type[-0.0]).to eql(BigDecimal("0"))
        expect(type[-0.0].sign).to eql(BigDecimal("0").sign)
      end

      it "normalizes negative zero literals" do
        expect(type["-0.0"]).to eql(BigDecimal("0"))
        expect(type["-0"]).to eql(BigDecimal("0"))
      end
    end
  end

  describe "composition: the lossless check is not bypassable" do
    specify "an outer constructor returning a fractional float fails for integer" do
      bad = integer.constructor { 3.7 }
      expect { bad["3"] }.to raise_error(Dry::Types::CoercionError)
    end

    specify "an appended constructor returning a fractional float fails for integer" do
      bad = integer.append { 3.7 }
      expect { bad["3"] }.to raise_error(Dry::Types::CoercionError)
    end

    specify "a prepended constructor returning a fractional float fails for integer" do
      bad = integer.prepend { 3.7 }
      expect { bad["3"] }.to raise_error(Dry::Types::CoercionError)
    end

    specify "operator composition keeps the same guarantee" do
      bad = integer >> ->(*) { 3.7 }
      expect { bad["3"] }.to raise_error(Dry::Types::CoercionError)

      bad = integer << ->(*) { 3.7 }
      expect { bad["3"] }.to raise_error(Dry::Types::CoercionError)
    end

    specify "an outer constructor cannot smuggle Infinity into float" do
      bad = float.constructor { Float::INFINITY }
      expect { bad["0.5"] }.to raise_error(Dry::Types::CoercionError)
    end

    specify "an outer constructor cannot smuggle NaN into decimal" do
      bad = decimal.constructor { BigDecimal("NaN") }
      expect { bad["3"] }.to raise_error(Dry::Types::CoercionError)
    end

    specify "wrapper constructors that invoke the wrapped type stay safe" do
      wrapped = integer.constructor { |input, type| type.(input) { :fallback } }
      expect(wrapped["3"]).to eql(3)
      expect(wrapped[3.7]).to be(:fallback)
    end

    specify "a wrapper constructor that ignores the wrapped type is an explicit escape hatch" do
      # This mirrors every other constructor type in dry-types: taking the
      # block with |input, type| means the caller is in full control.
      bypass = integer.constructor { |_input, _type| 3.7 }
      expect(bypass["3"]).to eql(3.7)
    end

    specify "outer constructors that pre-process valid input still work" do
      cleaned = integer.constructor { |input| input.to_s.strip }
      expect(cleaned[:" 3 "]).to eql(3)
    end

    specify "the lossless check runs after every attached constructor" do
      t = integer.constructor { |x| x }.constructor { |x| x }
      expect(t["3"]).to eql(3)
      expect { t[3.7] }.to raise_error(Dry::Types::CoercionError)
    end

    describe "inside arrays" do
      let(:array_type) { Dry::Types["array"].of(integer) }

      it "coerces valid members" do
        expect(array_type[["3", 4, 5.0]]).to eql([3, 4, 5])
      end

      it "fails on the first non-lossless member" do
        expect { array_type[["3.7"]] }.to raise_error(Dry::Types::CoercionError)
        expect { array_type[[3.7]] }.to raise_error(Dry::Types::CoercionError)
      end

      it "is not bypassed by an outer array constructor" do
        wrapped = array_type.constructor { |x| x }
        expect { wrapped[["3.7"]] }.to raise_error(Dry::Types::CoercionError)
      end
    end

    describe "inside schemas" do
      let(:schema_type) do
        Dry::Types["hash"].schema(amount: integer, rate: float, total: decimal)
      end

      it "coerces valid values" do
        expect(schema_type[{amount: "3", rate: "0.5", total: "3.7"}]).to eql(
          amount: 3, rate: 0.5, total: BigDecimal("3.7")
        )
      end

      it "rejects lossy values per key" do
        expect { schema_type[{amount: 3.7}] }.to raise_error(Dry::Types::CoercionError)
        expect { schema_type[{amount: "3.7"}] }.to raise_error(Dry::Types::CoercionError)
        expect { schema_type[{rate: "3.14"}] }.to raise_error(Dry::Types::CoercionError)
        expect { schema_type[{total: BigDecimal("NaN")}] }
          .to raise_error(Dry::Types::CoercionError)
      end

      it "is not bypassed by an outer schema constructor" do
        wrapped = schema_type.constructor { |h| h }
        expect { wrapped[{amount: 3.7}] }.to raise_error(Dry::Types::CoercionError)
      end
    end

    describe "inside sums" do
      let(:sum_type) { float | integer }

      it "accepts values accepted by a branch" do
        expect(sum_type[3.0]).to eql(3.0)
        expect(sum_type["0.5"]).to eql(0.5)
        expect(sum_type["3"]).to eql(3.0)
      end

      it "fails when neither branch accepts the value losslessly" do
        expect { sum_type["3.7"] }.to raise_error(Dry::Types::CoercionError)
        expect { sum_type["1e400"] }.to raise_error(Dry::Types::CoercionError)
      end
    end

    describe "optional types" do
      let(:optional_integer) { Dry::Types["optional.lossless.integer"] }
      let(:optional_decimal) { Dry::Types["optional.lossless.decimal"] }

      it "accepts nil" do
        expect(optional_integer[nil]).to be_nil
        expect(optional_decimal[nil]).to be_nil
      end

      it "coerces valid values" do
        expect(optional_integer["3"]).to eql(3)
        expect(optional_decimal["3.7"]).to eql(BigDecimal("3.7"))
      end

      it "rejects lossy values" do
        expect { optional_integer[3.7] }.to raise_error(Dry::Types::CoercionError)
        expect { optional_decimal[Float::INFINITY] }
          .to raise_error(Dry::Types::CoercionError)
      end
    end
  end

  describe "constraints on top of lossless types" do
    subject(:type) { integer.constrained(gt: 0) }

    it "enforces the constraint and the lossless rule" do
      expect(type["5"]).to eql(5)
      expect { type["-1"] }.to raise_error(Dry::Types::ConstraintError)
      expect { type[3.7] }.to raise_error(Dry::Types::CoercionError)
    end

    it "is not bypassed by an outer constructor returning an invalid value" do
      bad = type.constructor { 3.7 }
      expect { bad["5"] }.to raise_error(Dry::Types::CoercionError)
    end
  end

  describe "defaults and metadata" do
    it "supports metadata" do
      with_meta = integer.meta(currency: :USD)
      expect(with_meta.meta).to include(currency: :USD)
      expect(with_meta["3"]).to eql(3)
    end

    it "supports valid defaults" do
      with_default = integer.default(0)
      expect(with_default.()).to eql(0)
      expect(with_default["3"]).to eql(3)
      expect(with_default.(3.7) { 0 }).to eql(0)
    end
  end

  describe "the existing coercible defaults stay lossy and unchanged" do
    specify "coercible.integer truncates 3.7 like Kernel.Integer" do
      expect(Dry::Types["coercible.integer"][3.7]).to eql(3)
    end

    specify "coercible.float silently converts large integers" do
      expect(Dry::Types["coercible.float"][10**100]).to eql((10**100).to_f)
    end

    specify "params.integer rejects strings as before" do
      expect(Dry::Types["params.integer"]["312"]).to eql(312)
      expect { Dry::Types["params.integer"]["3.7"] }
        .to raise_error(Dry::Types::CoercionError)
    end

    specify "params.float accepts non-exact literals" do
      expect(Dry::Types["params.float"]["3.14"]).to eql(3.14)
    end

    specify "json.decimal keeps its behavior" do
      expect(Dry::Types["json.decimal"]["3.12"]).to eql(BigDecimal("3.12"))
      expect(Dry::Types["json.decimal"][3.12]).to eql(BigDecimal("3.12"))
    end
  end

  describe "module export" do
    it "is available under the Lossless namespace" do
      mod = Module.new.tap { |m| m.include(Dry.Types(:lossless)) }
      expect(mod::Lossless::Integer["3"]).to eql(3)
      expect(mod::Lossless::Float["0.5"]).to eql(0.5)
      expect(mod::Lossless::Decimal["3.7"]).to eql(BigDecimal("3.7"))
      expect { mod::Lossless::Integer[3.7] }
        .to raise_error(Dry::Types::CoercionError)
    end

    it "can be used as the default namespace" do
      mod = Module.new.tap { |m| m.include(Dry.Types(default: :lossless)) }
      expect(mod::Integer).to be_a(Dry::Types::Constructor::Lossless)
      expect(mod::Integer["3"]).to eql(3)
    end
  end
end
