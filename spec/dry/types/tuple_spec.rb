# frozen_string_literal: true

RSpec.describe Dry::Types::Tuple do
  let(:symbol) { Dry::Types["strict.symbol"] }
  let(:integer) { Dry::Types["strict.integer"] }
  let(:string) { Dry::Types["strict.string"] }

  describe "fixed prefix" do
    subject(:type) { Dry::Types["nominal.array"].tuple(symbol, integer) }

    it "coerces a matching array" do
      expect(type.([:create, 1])).to eql([:create, 1])
    end

    it "coerces element types" do
      type = Dry::Types["nominal.array"].tuple(
        Dry::Types["coercible.symbol"],
        Dry::Types["coercible.integer"]
      )

      expect(type.(["create", "1"])).to eql([:create, 1])
    end

    it "rejects missing elements instead of filling them with nil" do
      expect { type.([:create]) }.to raise_error(
        Dry::Types::TupleError, /too few elements/
      )
    end

    it "does not treat missing elements as nil" do
      expect(type.try([:create])).to be_failure
    end

    it "rejects extra elements instead of dropping them silently" do
      expect { type.([:create, 1, 2]) }.to raise_error(
        Dry::Types::TupleError, /too many elements/
      )
    end

    it "rejects non-array input" do
      expect(type.try("create")).to be_failure
    end
  end

  describe "optional tail items" do
    subject(:type) do
      Dry::Types["nominal.array"].tuple(symbol, integer, string.omittable)
    end

    it "accepts the full arity" do
      expect(type.([:create, 1, "v1"])).to eql([:create, 1, "v1"])
    end

    it "accepts a missing optional tail" do
      expect(type.([:create, 1])).to eql([:create, 1])
    end

    it "keeps the required prefix enforced" do
      expect { type.([:create]) }.to raise_error(Dry::Types::TupleError)
    end

    it "does not accept a nil in place of a missing element" do
      expect { type.([:create, 1, nil]) }.to raise_error(Dry::Types::TupleError)
    end

    it "supports nilable slots for explicit nil values while staying required" do
      type = Dry::Types["nominal.array"].tuple(symbol, string.optional)

      expect(type.([:create, nil])).to eql([:create, nil])
      expect { type.([:create]) }.to raise_error(Dry::Types::TupleError)
    end
  end

  describe "default values" do
    subject(:type) do
      Dry::Types["nominal.array"].tuple(
        symbol,
        integer.default(1),
        string.default("v0")
      )
    end

    it "fills missing tail elements with defaults" do
      expect(type.([:create])).to eql([:create, 1, "v0"])
    end

    it "does not override present elements" do
      expect(type.([:create, 2])).to eql([:create, 2, "v0"])
      expect(type.([:create, 2, "v2"])).to eql([:create, 2, "v2"])
    end

    it "supports callable defaults" do
      calls = 0
      type = Dry::Types["nominal.array"].tuple(
        symbol,
        integer.default(-> _ { calls += 1 })
      )

      expect(type.([:create])).to eql([:create, 1])
      expect(type.([:create])).to eql([:create, 2])
    end
  end

  describe "repeated tail" do
    subject(:type) do
      Dry::Types["nominal.array"].tuple(
        symbol,
        Dry::Types["coercible.integer"].rest
      )
    end

    it "matches the fixed prefix with no rest elements" do
      expect(type.([:create])).to eql([:create])
    end

    it "collects the rest elements" do
      expect(type.([:create, "1", "2", "3"])).to eql([:create, 1, 2, 3])
    end

    it "coerces every rest element with its type" do
      expect { type.([:create, "1", "nope"]) }.to raise_error(
        Dry::Types::TupleError, /invalid value for Integer/
      )
    end

    it "can be combined with optional slots" do
      type = Dry::Types["nominal.array"].tuple(
        symbol,
        integer.omittable,
        string.rest
      )

      expect(type.([:create, 1, "a", "b"])).to eql([:create, 1, "a", "b"])
      expect(type.([:create])).to eql([:create])
    end
  end

  describe "declaration validation" do
    it "rejects a required slot following an optional slot" do
      expect {
        Dry::Types["nominal.array"].tuple(string.omittable, integer)
      }.to raise_error(
        Dry::Types::TupleDeclarationError, /required slot cannot follow an optional slot/
      )
    end

    it "treats slots with default values as optional" do
      expect {
        Dry::Types["nominal.array"].tuple(string.default("x"), integer)
      }.to raise_error(Dry::Types::TupleDeclarationError)
    end

    it "rejects more than one repeated tail" do
      expect {
        Dry::Types["nominal.array"].tuple(string.rest, integer.rest)
      }.to raise_error(Dry::Types::TupleDeclarationError, /repeated tail must be the last/)
    end

    it "rejects a repeated tail that is not positioned last" do
      expect {
        Dry::Types["nominal.array"].tuple(string.rest, integer.omittable)
      }.to raise_error(Dry::Types::TupleDeclarationError)
    end

    it "accepts an empty tuple" do
      type = Dry::Types["nominal.array"].tuple

      expect(type.([])).to eql([])
      expect(type.try([1])).to be_failure
    end
  end

  describe "#call" do
    subject(:type) { Dry::Types["nominal.array"].tuple(symbol, integer, string.omittable) }

    it "raises on the first offending position" do
      expect { type.([:create, "bad"]) }.to raise_error(Dry::Types::TupleError) { |error|
        expect(error.index).to be(1)
      }
    end

    it "points at the first unexpected element on extra input" do
      expect {
        type.([:create, 1, "v1", :extra])
      }.to raise_error(Dry::Types::TupleError) { |error|
        expect(error.index).to be(3)
      }
    end

    it "reports cardinality with an index" do
      expect { type.([:create]) }.to raise_error(Dry::Types::TupleError) { |error|
        expect(error.index).to be(1)
      }
    end

    it "reports no position for a non-array input" do
      failure = type.try("nope")

      expect(failure).to be_failure
      expect(failure.error.index).to be_nil
    end

    context "with a failure block" do
      it "yields a failure and returns the block result" do
        expect(type.([:create, "bad"]) { :fallback }).to be(:fallback)
      end

      it "yields partially coerced output" do
        result = type.([:create, "bad", "v1"]) { |output| output }

        expect(result).to eql([:create])
      end
    end
  end

  describe "#try" do
    subject(:type) { Dry::Types["nominal.array"].tuple(symbol, integer) }

    it "returns a success" do
      expect(type.try([:create, 1])).to be_success
    end

    it "returns a failure carrying the original index" do
      failure = type.try([:create, "bad"])

      expect(failure).to be_failure
      expect(failure.error).to be_a(Dry::Types::TupleError)
      expect(failure.error.index).to be(1)
    end

    it "calls the block with the failure" do
      result = type.try([:create, "bad"]) { |failure| "failed@#{failure.error.index}" }

      expect(result).to eql("failed@1")
    end
  end

  describe "#valid?" do
    subject(:type) { Dry::Types["nominal.array"].tuple(symbol, integer) }

    it "is true for conforming input" do
      expect(type).to be_valid([:create, 1])
    end

    it "is false for wrong element types" do
      expect(type).not_to be_valid([:create, "1"])
    end

    it "is false for wrong arity" do
      expect(type).not_to be_valid([:create])
      expect(type).not_to be_valid([:create, 1, 2])
    end

    it "is false for non-arrays" do
      expect(type).not_to be_valid(:create)
    end
  end

  describe "#to_s" do
    it "prints the tuple with positional types" do
      type = Dry::Types["nominal.array"].tuple(symbol, integer, string.omittable)

      expect(type.to_s).to eql(
        "#<Dry::Types[Tuple<" \
        "Constrained<Nominal<Symbol> rule=[type?(Symbol)]>, " \
        "Constrained<Nominal<Integer> rule=[type?(Integer)]>, " \
        "Constrained<Nominal<String> rule=[type?(String)]>?>]>"
      )
    end

    it "prints repeated tails" do
      type = Dry::Types["nominal.array"].tuple(symbol, integer.rest)

      expect(type.to_s).to end_with("Tuple<Constrained<Nominal<Symbol> rule=[type?(Symbol)]>, " \
                                    "Constrained<Nominal<Integer> rule=[type?(Integer)]>*>]>")
    end
  end

  describe "#to_ast" do
    subject(:type) do
      Dry::Types["nominal.array"].tuple(symbol, integer, string.omittable)
    end

    it "dumps tuple and slot nodes" do
      ast = type.to_ast

      expect(ast[0]).to be(:tuple)

      slots = ast[1][0]
      expect(slots.map { |slot| slot[0] }).to eql(%i[slot slot slot])
      expect(slots.map { |slot| slot[1][1] }).to eql(
        [
          {optional: false, rest: false},
          {optional: false, rest: false},
          {optional: true, rest: false}
        ]
      )
    end

    it "round-trips through the compiler" do
      compiler = Dry::Types::Compiler.new(Dry::Types)

      expect(compiler.(type.to_ast)).to eql(type)
    end

    it "preserves repeated tails after compilation" do
      type = Dry::Types["nominal.array"].tuple(symbol, integer.rest)
      compiled = Dry::Types::Compiler.new(Dry::Types).(type.to_ast)

      expect(compiled.([:op, 1, 2, 3])).to eql([:op, 1, 2, 3])
    end

    it "preserves default values after AST compilation" do
      type = Dry::Types["nominal.array"].tuple(symbol, integer.default(0), string.default("x"))
      compiled = Dry::Types::Compiler.new(Dry::Types).(type.to_ast)

      expect(compiled.([:op])).to eql([:op, 0, "x"])
      expect(compiled).to eql(type)
    end

    it "preserves callable defaults after AST compilation" do
      type = Dry::Types["nominal.array"].tuple(
        symbol,
        integer.default(-> _ { 10 })
      )
      compiled = Dry::Types::Compiler.new(Dry::Types).(type.to_ast)

      expect(compiled.([:op])).to eql([:op, 10])
    end

    it "preserves meta after compilation" do
      with_meta = type.meta(foo: :bar)
      compiled = Dry::Types::Compiler.new(Dry::Types).(with_meta.to_ast)

      expect(compiled.meta).to eql(foo: :bar)
    end
  end

  describe "homogeneous arrays" do
    it "keeps Array#of behavior intact" do
      type = Dry::Types["nominal.array"].of(Dry::Types["coercible.integer"])

      expect(type.(%w[1 2 3])).to eql([1, 2, 3])
      expect(type).to be_a(Dry::Types::Array::Member)
    end
  end

  describe "constructors" do
    it "builds a tuple on a coercible array" do
      type = Dry::Types["coercible.array"].tuple(integer, string)

      expect(type.call([1, "a"])).to eql([1, "a"])
      expect(type).to be_a(Dry::Types::Constructor)
    end

    it "applies the array constructor before tuple coercion" do
      type = Dry::Types["coercible.array"].tuple(
        Dry::Types["coercible.integer"],
        Dry::Types["coercible.string"]
      )

      expect(type.call([1, :a])).to eql([1, "a"])
    end
  end

  describe "#lax" do
    subject(:type) { Dry::Types["nominal.array"].tuple(integer).lax }

    it "coerces valid input" do
      expect(type.([1])).to eql([1])
    end

    it "keeps the original element when it fails to coerce" do
      expect(type.(["bad"])).to eql(["bad"])
    end

    it "returns the original input on an arity mismatch" do
      expect(type.([1, 2])).to eql([1, 2])
    end
  end

  it_behaves_like Dry::Types::Nominal do
    subject(:type) { Dry::Types["nominal.array"].tuple(symbol, integer) }
  end
end
