# frozen_string_literal: true

RSpec.describe Dry::Types::Tuple do
  subject(:tuple) { Dry::Types["nominal.array"].tuple(string, integer) }

  let(:string) { Dry::Types["strict.string"] }
  let(:integer) { Dry::Types["strict.integer"] }

  describe "building" do
    it "builds a tuple from types" do
      expect(tuple).to be_a(described_class)
    end

    it "accepts string identifiers" do
      type = Dry::Types["nominal.array"].tuple("string", "coercible.integer")
      expect(type.(["a", "1"])).to eql(["a", 1])
    end

    it "accepts classes" do
      type = Dry::Types["nominal.array"].tuple(String, Integer)
      expect(type.(["a", 1])).to eql(["a", 1])
    end

    it "is exposed through the Types module helper" do
      type = Dry.Types(:strict).Tuple(Dry::Types["strict.string"], Dry::Types["strict.integer"])
      expect(type.type).to be_a(described_class)
      expect(type.(["a", 1])).to eql(["a", 1])
    end

    it_behaves_like Dry::Types::Nominal do
      subject(:type) { tuple }
    end
  end

  describe "declaration validation" do
    it "rejects a required position after a defaulted one" do
      expect {
        Dry::Types["nominal.array"].tuple(
          Dry::Types["strict.integer"].default(1), Dry::Types["strict.string"]
        )
      }.to raise_error(ArgumentError, /required tuple position after an optional one/)
    end

    it "rejects positional arguments together with a block" do
      expect {
        Dry::Types["nominal.array"].tuple(Dry::Types["strict.string"]) {
          required Dry::Types["strict.integer"]
        }
      }.to raise_error(ArgumentError, /both positional types and a block/)
    end
  end

  describe Dry::Types::TupleError do
    it "is a CoercionError and carries the input index" do
      error = Dry::Types::TupleError.new("boom", index: 3)

      expect(error).to be_a(Dry::Types::CoercionError)
      expect(error.message).to eql("boom")
      expect(error.index).to be(3)
    end

    it "can be created without an index" do
      expect(Dry::Types::TupleError.new("boom").index).to be_nil
    end
  end

  describe "#call" do
    it "coerces every position" do
      expect(tuple.(["answer", 42])).to eql(["answer", 42])
    end

    it "raises on a value rejected at a position" do
      expect { tuple.(["answer", "42"]) }.to raise_error(
        Dry::Types::TupleError, /tuple position 1/
      )
    end

    it "reports the original input index on element errors" do
      begin
        tuple.(["answer", "42"])
      rescue Dry::Types::TupleError => e
        expect(e.index).to be(1)
      end
    end

    it "does not pad missing positions with nil" do
      begin
        tuple.(["answer"])
      rescue Dry::Types::TupleError => error
        expect(error.message).to match(/missing a value for position 1/)
        expect(error.index).to be(1)
      else
        raise "expected TupleError"
      end
    end

    it "does not silently drop extra positions" do
      begin
        tuple.(["answer", 1, :extra])
      rescue Dry::Types::TupleError => error
        expect(error.message).to match(/unexpected value at position 2/)
        expect(error.index).to be(2)
      else
        raise "expected TupleError"
      end
    end

    it "reports all missing positions" do
      type = Dry::Types["nominal.array"].tuple(integer, integer, integer)

      begin
        type.([0])
      rescue Dry::Types::TupleError => error
        expect(error.message).to match(/missing values for positions 1, 2/)
        expect(error.index).to be(1)
      else
        raise "expected TupleError"
      end
    end

    it "reports all extra positions" do
      type = Dry::Types["nominal.array"].tuple(integer, integer)

      begin
        type.([1, 2, 3, 4])
      rescue Dry::Types::TupleError => error
        expect(error.message).to match(/unexpected values at positions 2, 3/)
        expect(error.index).to be(2)
      else
        raise "expected TupleError"
      end
    end

    context "non-array input" do
      specify "nominal tuples pass the input through" do
        expect(tuple.(42)).to be(42)
      end

      specify "strict tuples raise" do
        strict = Dry.Types(:strict).Tuple(string, integer)
        expect { strict.(42) }.to raise_error(Dry::Types::ConstraintError)
      end
    end

    context "with a failure block" do
      it "yields a partial result on element failure" do
        expect(tuple.(["answer", "42"]) { |out| out }).to eql(["answer"])
      end

      it "yields a partial result for missing values" do
        expect(tuple.(["answer"]) { |out| out }).to eql(["answer"])
      end

      it "yields all values for extra items" do
        result = tuple.(["answer", 1, 2]) { |out| out }
        expect(result).to eql(["answer", 1, 2])
      end

      it "yields the original value for non-array input" do
        expect(tuple.(42) { 42 }).to be(42)
      end
    end
  end

  describe "#try" do
    it "returns a success for valid input" do
      expect(tuple.try(["a", 1])).to be_success
    end

    it "returns a failure for invalid input with an index-aware error" do
      result = tuple.try(["a", "1"])

      expect(result).to be_failure
      expect(result.error).to be_a(Dry::Types::TupleError)
      expect(result.error.index).to be(1)
      expect(result.error.message).to include("tuple position 1")
    end

    it "returns a failure for too short input" do
      result = tuple.try(["a"])

      expect(result).to be_failure
      expect(result.error.index).to be(1)
    end

    it "returns a failure for too long input" do
      result = tuple.try(["a", 1, 2])

      expect(result).to be_failure
      expect(result.error.index).to be(2)
    end

    it "returns a failure for non-array input" do
      result = tuple.try(42)

      expect(result).to be_failure
      expect(result.error.message).to eql("42 is not an array")
    end

    it "calls the block on failure" do
      result = tuple.try(["a", 1, 2]) { :failed }
      expect(result).to be(:failed)
    end
  end

  describe "optional positions" do
    subject(:tuple) do
      Dry::Types["nominal.array"].tuple(
        integer, described_class.optional(string)
      )
    end

    it "requires fixed prefix positions" do
      expect { tuple.([]) }.to raise_error(Dry::Types::TupleError, /position 0/)
    end

    it "matches input without the optional tail" do
      expect(tuple.([1])).to eql([1])
    end

    it "matches input with the optional tail" do
      expect(tuple.([1, "a"])).to eql([1, "a"])
    end

    it "does not treat missing positions as nil" do
      expect(tuple.([1])).to_not include(nil)
    end

    it "validates the present optional value" do
      begin
        tuple.([1, :bad])
      rescue Dry::Types::TupleError => e
        expect(e.index).to be(1)
      else
        raise "expected TupleError"
      end
    end

    it "rejects required positions declared after optional ones" do
      expect {
        Dry::Types["nominal.array"].tuple(
          integer, described_class.optional(string), integer
        )
      }.to raise_error(ArgumentError, /required tuple position after an optional one/)
    end

    it "can be declared with a block" do
      type = Dry::Types["nominal.array"].tuple do
        required Dry::Types["strict.integer"]
        optional Dry::Types["strict.string"]
      end

      expect(type.([1])).to eql([1])
      expect(type.([1, "a"])).to eql([1, "a"])
    end
  end

  describe "rest positions" do
    subject(:tuple) do
      Dry::Types["nominal.array"].tuple(
        string, integer, described_class.rest(Dry::Types["nominal.any"])
      )
    end

    it "matches without trailing items" do
      expect(tuple.(["a", 1])).to eql(["a", 1])
    end

    it "matches with any number of trailing items" do
      expect(tuple.(["a", 1, :x, "y", 3])).to eql(["a", 1, :x, "y", 3])
    end

    it "applies the rest type and keeps original indices" do
      type = Dry::Types["nominal.array"].tuple(
        string, described_class.rest(integer)
      )

      expect(type.(["a", 1, 2])).to eql(["a", 1, 2])

      begin
        type.(["a", 1, "bad"])
      rescue Dry::Types::TupleError => error
        expect(error.index).to be(2)
        expect(error.message).to include("tuple position 2")
      else
        raise "expected TupleError"
      end
    end

    it "can be combined with optional positions" do
      type = Dry::Types["nominal.array"].tuple do
        required integer
        optional string
        rest integer
      end

      expect(type.([1])).to eql([1])
      expect(type.([1, "a", 2, 3])).to eql([1, "a", 2, 3])
      # trailing items always feed the rest
      expect(type.([1, "a"])).to eql([1, "a"])

      begin
        type.([1, 2, 3])
      rescue Dry::Types::TupleError => error
        expect(error.index).to be(1)
      else
        raise "expected TupleError"
      end
    end

    it "supports a rest-only tuple" do
      type = Dry::Types["nominal.array"].tuple(described_class.rest(integer))

      expect(type.([])).to eql([])
      expect(type.([1, 2, 3])).to eql([1, 2, 3])

      expect { type.([1, "bad"]) }.to raise_error(Dry::Types::TupleError, /position 1/)
    end
  end

  describe "default values" do
    subject(:tuple) do
      Dry::Types["nominal.array"].tuple(
        string, Dry::Types["strict.integer"].default(0)
      )
    end

    it "fills missing positions with their default" do
      expect(tuple.(["a"])).to eql(["a", 0])
    end

    it "uses provided values over defaults" do
      expect(tuple.(["a", 5])).to eql(["a", 5])
    end

    it "keeps default semantics through AST compilation" do
      compiled = Dry::Types::Compiler.new(Dry::Types).(tuple.to_ast)

      expect(compiled.(["a"])).to eql(["a", 0])
      expect(compiled.(["a", 9])).to eql(["a", 9])
    end
  end

  describe "#to_ast" do
    it "dumps fixed, optional and rest positions" do
      type = Dry::Types["nominal.array"].tuple do
        required Dry::Types["nominal.string"]
        optional Dry::Types["nominal.integer"]
        rest Dry::Types["nominal.any"]
      end

      expect(type.to_ast).to eql(
        [:tuple, [
          [
            [:tuple_position, [[:nominal, [String, {}, {}]]]],
            [:tuple_optional, [[:nominal, [Integer, {}, {}]]]],
            [:tuple_rest, [[:any, {}]]]
          ],
          {}
        ]]
      )
    end

    it "round-trips through the compiler" do
      type = Dry::Types["nominal.array"].tuple do
        required Dry::Types["strict.string"]
        optional Dry::Types["coercible.integer"].default(1)
        rest Dry::Types["coercible.integer"]
      end

      compiled = Dry::Types::Compiler.new(Dry::Types).(type.to_ast)

      expect(compiled.to_ast).to eql(type.to_ast)
      expect(compiled.(["a", "2", "3"])).to eql(["a", 2, 3])
      expect(compiled.(["a"])).to eql(["a", 1])
    end

    it "does not affect homogeneous array AST" do
      type = Dry::Types["nominal.array"].of(Dry::Types["nominal.string"])
      expect(type.to_ast).to eql([:array, [[:nominal, [String, {}, {}]], {}]])
    end
  end

  describe "#valid?" do
    subject(:tuple) { Dry.Types(:strict).Tuple(string, integer) }

    it "is true for valid input" do
      expect(tuple.valid?(["a", 1])).to be(true)
    end

    it "is false for invalid input" do
      expect(tuple.valid?(["a", "1"])).to be(false)
      expect(tuple.valid?(["a"])).to be(false)
      expect(tuple.valid?(["a", 1, 2])).to be(false)
      expect(tuple.valid?(42)).to be(false)
    end
  end

  describe "#to_s" do
    it "prints positions" do
      type = Dry::Types["nominal.array"].tuple(
        Dry::Types["nominal.string"], Dry::Types["nominal.integer"]
      )
      expect(type.to_s).to eql("#<Dry::Types[Tuple<Nominal<String>, Nominal<Integer>>]>")
    end

    it "prints optional and rest markers" do
      type = Dry::Types["nominal.array"].tuple do
        required Dry::Types["nominal.string"]
        optional Dry::Types["nominal.integer"]
        rest Dry::Types["nominal.any"]
      end

      expect(type.to_s).to eql(
        "#<Dry::Types[Tuple<Nominal<String>, Nominal<Integer>?, *Any>]>"
      )
    end
  end

  describe "#==" do
    it "equals a tuple with the same positions" do
      other = Dry::Types["nominal.array"].tuple(string, integer)
      expect(tuple).to eql(other)
    end

    it "is not equal to a tuple with different positions" do
      other = Dry::Types["nominal.array"].tuple(string, string)
      expect(tuple).to_not eql(other)
    end
  end

  describe "constructors" do
    it "wraps a tuple preserving its coercion function" do
      type = Dry::Types["coercible.array"].tuple(
        Dry::Types["coercible.string"],
        Dry::Types["coercible.integer"]
      )

      expect(type.([:a, "1"])).to eql(["a", 1])
    end

    it "adds an extra constructor on top" do
      type = tuple.constructor { |input| input.map(&:to_s) }
      expect(type).to be_a(Dry::Types::Array::Constructor)
    end
  end

  describe "#lax" do
    it "returns a lax tuple that does not raise on invalid elements" do
      lax = tuple.lax

      expect(lax.(["a", 1])).to eql(["a", 1])
      expect(lax.(["a", "b"])).to eql(["a", "b"])
    end

    it "preserves optional and rest positions" do
      type = Dry::Types["nominal.array"].tuple do
        required Dry::Types["strict.integer"]
        optional Dry::Types["strict.string"]
        rest Dry::Types["strict.integer"]
      end

      lax = type.lax

      expect(lax.([1, "a", 2])).to eql([1, "a", 2])
      expect(lax.([1])).to eql([1])
    end
  end
end
