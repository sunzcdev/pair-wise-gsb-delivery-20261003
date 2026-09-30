# frozen_string_literal: true

RSpec.describe Dry::Types::Shared do
  subject(:shared_type) { type.shared }

  let(:type) { Dry::Types["hash"].schema(type_map) }
  let(:type_map) do
    {
      name: Dry::Types["coercible.string"],
      age: Dry::Types["coercible.integer"]
    }
  end

  it_behaves_like "Dry::Types::Nominal without primitive" do
    subject(:type) { Dry::Types["hash"].schema({}).shared }
  end

  describe "#call" do
    context "hash schema" do
      it "returns the same hash object when nothing changes" do
        input = {name: "Jane", age: 21}

        expect(shared_type.(input)).to be(input)
      end

      it "returns a new hash when a value is coerced" do
        input = {name: :Jane, age: "21"}

        result = shared_type.(input)

        expect(result).to_not be(input)
        expect(result).to eql(name: "Jane", age: 21)
      end

      it "never mutates the original hash on a successful conversion" do
        input = {name: :Jane, age: "21"}

        shared_type.(input)

        expect(input).to eql(name: :Jane, age: "21")
      end

      it "does not rely on == to decide sharing" do
        schema = Dry::Types["hash"].schema(n: Dry::Types["coercible.float"]).shared
        input = {n: 1}

        result = schema.(input)

        expect(result).to_not be(input)
        expect(result).to eql(n: 1.0)
      end

      it "keeps identical (equal?) values shared" do
        schema = Dry::Types["hash"].schema(n: Dry::Types["nominal.float"]).shared
        input = {n: 1.0}

        expect(schema.(input)).to be(input)
      end

      it "returns a new hash when an unknown key is discarded" do
        input = {name: "Jane", age: 21, extra: true}

        result = shared_type.(input)

        expect(result).to_not be(input)
        expect(result).to eql(name: "Jane", age: 21)
        expect(input).to eql(name: "Jane", age: 21, extra: true)
      end

      it "preserves key order when dropping unknown keys" do
        input = {name: "Jane", unknown: 1, age: 21}

        expect(shared_type.(input).keys).to eql(%i[name age])
      end

      it "returns a new hash when a default value is inserted" do
        schema = Dry::Types["hash"].schema(
          name: Dry::Types["coercible.string"],
          age: Dry::Types["integer"].default(30)
        ).shared
        input = {name: "Jane"}

        result = schema.(input)

        expect(result).to_not be(input)
        expect(result).to eql(name: "Jane", age: 30)
        expect(input).to eql(name: "Jane")
      end

      it "reuses the hash when a default key is already present" do
        schema = Dry::Types["hash"].schema(
          name: Dry::Types["coercible.string"],
          age: Dry::Types["integer"].default(30)
        ).shared
        input = {name: "Jane", age: 21}

        expect(schema.(input)).to be(input)
      end

      it "returns a new hash when keys are transformed" do
        schema = Dry::Types["hash"]
          .schema(name: Dry::Types["nominal.any"])
          .with_key_transform(&:to_sym)
          .shared
        input = {"name" => "Jane"}

        result = schema.(input)

        expect(result).to_not be(input)
        expect(result).to eql(name: "Jane")
        expect(result).to_not have_key("name")
      end

      it "reuses unchanged sibling values when one value changes" do
        inner = Dry::Types["hash"].schema(n: Dry::Types["coercible.integer"])
        schema = Dry::Types["hash"].schema(
          left: inner,
          right: inner
        ).shared
        input = {left: {n: "1"}, right: {n: 2}}

        result = schema.(input)

        expect(result).to_not be(input)
        expect(result[:left]).to_not be(input[:left])
        expect(result[:right]).to be(input[:right])
      end

      it "reuses nested containers when nothing changes" do
        schema = Dry::Types["hash"].schema(
          items: Dry::Types["array"].of(Dry::Types["coercible.integer"])
        ).shared
        input = {items: [1, 2, 3]}

        result = schema.(input)

        expect(result).to be(input)
        expect(result[:items]).to be(input[:items])
      end

      it "leaves the input intact when coercion fails" do
        schema = Dry::Types["hash"].schema(age: Dry::Types["strict.integer"]).shared
        input = {age: "x"}

        expect { schema.(input) }.to raise_error(Dry::Types::SchemaError)
        expect(input).to eql(age: "x")
      end

      it "reuses an empty hash for an empty schema" do
        input = {}

        expect(Dry::Types["hash"].schema({}).shared.(input)).to be(input)
      end

      it "raises on unknown keys for strict schemas without touching input" do
        schema = Dry::Types["hash"].schema({}).strict.shared
        input = {extra: 1}

        expect { schema.(input) }.to raise_error(Dry::Types::UnknownKeysError)
        expect(input).to eql(extra: 1)
      end
    end

    context "typed array" do
      subject(:array_type) do
        Dry::Types["array"].of(member).shared
      end

      let(:member) { Dry::Types["coercible.integer"] }

      it "returns the same array when nothing changes" do
        input = [1, 2, 3]

        expect(array_type.(input)).to be(input)
      end

      it "returns a new array when an element is coerced" do
        input = ["1", 2, 3]

        result = array_type.(input)

        expect(result).to_not be(input)
        expect(result).to eql([1, 2, 3])
      end

      it "reuses unchanged elements before and after a change" do
        first = Object.new
        third = Object.new
        member = Dry::Types["nominal.any"].constructor { |value|
          value.is_a?(::Symbol) ? value.to_s : value
        }
        array_type = Dry::Types["array"].of(member).shared
        input = [first, :changed, third]

        result = array_type.(input)

        expect(result[0]).to be(first)
        expect(result[1]).to eql("changed")
        expect(result[2]).to be(third)
      end

      it "does not reuse elements that are merely ==" do
        input = [1.0, 2.0]

        expect(array_type.(input)).to_not be(input)
      end

      it "returns a new array when Undefined elements are filtered" do
        member = Dry::Types["nominal.string"].constructor { |value|
          value == "" ? Dry::Types::Undefined : value
        }
        array_type = Dry::Types["array"].of(member).shared
        input = ["", "foo", ""]

        result = array_type.(input)

        expect(result).to_not be(input)
        expect(result).to eql(["foo"])
        expect(input).to eql(["", "foo", ""])
      end

      it "reuses the array when no element is filtered" do
        input = ["foo", "bar"]
        member = Dry::Types["nominal.string"]

        expect(Dry::Types["array"].of(member).shared.(input)).to be(input)
      end

      it "leaves the input intact when coercion of an element fails" do
        array_type = Dry::Types["array"].of(Dry::Types["strict.integer"]).shared
        input = [1, "x", 3]

        expect { array_type.(input) }.to raise_error(Dry::Types::CoercionError)
        expect(input).to eql([1, "x", 3])
      end

      it "reuses nested hash elements that do not change" do
        member = Dry::Types["hash"].schema(n: Dry::Types["coercible.integer"])
        array_type = Dry::Types["array"].of(member).shared
        input = [{n: 1}, {n: "2"}, {n: 3}]

        result = array_type.(input)

        expect(result).to_not be(input)
        expect(result[0]).to be(input[0])
        expect(result[1]).to_not be(input[1])
        expect(result[2]).to be(input[2])
      end
    end

    context "scalar nominal types" do
      it "returns the same object" do
        input = Object.new

        expect(Dry::Types["nominal.any"].shared.(input)).to be(input)
      end
    end

    describe "constructor invocation boundary" do
      it "calls constructors only for elements that need them while reusing" do
        calls = 0
        member = Dry::Types["nominal.any"].constructor { |value|
          calls += 1
          value
        }
        array_type = Dry::Types["array"].of(member).shared
        input = [Object.new, Object.new, Object.new]

        array_type.(input)

        expect(calls).to be(3)
      end

      it "allocates new values once per changed element but reuses untouched ones" do
        allocations = 0
        klass = Class.new
        ctor = klass.method(:new)
        counting = Dry::Types["nominal.any"].constructor { |value|
          if value.is_a?(::Integer)
            allocations += 1
            ctor.call.tap { |obj| obj.define_singleton_method(:source) { value } }
          else
            value
          end
        }
        array_type = Dry::Types["array"].of(counting).shared
        untouched = Object.new
        input = [untouched, 1, untouched]

        result = array_type.(input)

        expect(allocations).to be(1)
        expect(result[0]).to be(untouched)
        expect(result[2]).to be(untouched)
        expect(result.size).to be(3)
      end

      it "does not invoke default constructors when sharing remains possible" do
        evaluations = 0
        default_type = Dry::Types["integer"].default {
          evaluations += 1
          30
        }
        schema = Dry::Types["hash"].schema(age: default_type).shared
        input = {age: 21}

        expect(schema.(input)).to be(input)
        expect(evaluations).to be(0)
      end
    end
  end

  describe "#call with a block" do
    it "yields on failure without mutating the input" do
      schema = Dry::Types["hash"].schema(age: Dry::Types["strict.integer"]).shared
      input = {age: "x"}

      expect(schema.(input) { :fallback }).to be(:fallback)
      expect(input).to eql(age: "x")
    end
  end

  describe "#shared" do
    it "is idempotent" do
      expect(shared_type.shared).to be(shared_type)
    end
  end

  describe "composition" do
    it "shares unchanged containers through a lax wrapper" do
      lax = Dry::Types["array"].of(Dry::Types["strict.integer"]).shared.lax
      input = [1, 2, 3]

      expect(lax.(input)).to be(input)
    end

    it "works when shared wraps lax" do
      lax = Dry::Types["array"].of(Dry::Types["strict.integer"]).lax.shared
      input = [1, 2, 3]

      expect(lax.(input)).to be(input)
    end

    it "shares unchanged values through a plain constructor wrapper" do
      wrapped = Dry::Types["hash"]
        .schema(a: Dry::Types["coercible.integer"])
        .constructor { |input| input }
        .shared
      input = {a: 1}

      expect(wrapped.(input)).to be(input)
    end

    it "shares via apply with skip_missing and defaults disabled" do
      schema = Dry::Types["hash"].schema(
        a: Dry::Types["nominal.any"],
        b: Dry::Types["integer"].default(5)
      ).shared
      input = {a: 1}

      result = schema.apply(
        input,
        skip_missing: true,
        resolve_defaults: false
      )

      expect(result).to be(input)
    end

    it "reuses untouched sibling containers in deeply nested structures" do
      array_of_hash = Dry::Types["array"].of(
        Dry::Types["hash"].schema(x: Dry::Types["coercible.integer"])
      )
      schema = Dry::Types["hash"].schema(list: array_of_hash).shared
      input = {list: [{x: 1}, {x: "2"}]}

      result = schema.(input)

      expect(result).to_not be(input)
      expect(result[:list]).to_not be(input[:list])
      expect(result[:list][0]).to be(input[:list][0])
      expect(result[:list][1]).to_not be(input[:list][1])
    end
  end

  describe "#to_s" do
    it "renders the wrapper" do
      expect(shared_type.to_s).to start_with("#<Dry::Types[Shared<")
    end
  end
end
