# frozen_string_literal: true

RSpec.describe "Structural sharing" do
  subject(:schema) do
    Dry::Types["hash"].schema(
      name: "string",
      age: "coercible.integer",
      tags: Dry::Types["array"].of(Dry::Types["string"]),
      address: Dry::Types["hash"].schema(
        city: "string",
        zip: "coercible.integer"
      )
    )
  end

  let(:conforming_input) do
    {
      name: "Jane",
      age: 30,
      tags: %w[a b],
      address: {city: "Berlin", zip: 10_115}
    }
  end

  describe "when coercion changes nothing" do
    it "returns the very same hash object" do
      input = conforming_input

      expect(schema.apply(input, structural_sharing: true)).to be(input)
    end

    it "still runs member constructors on every call" do
      calls = 0
      counting = Dry::Types["string"].constructor { |v| calls += 1; v }
      array = Dry::Types["array"].of(counting)
      type = Dry::Types["hash"].schema(tags: array)

      input = {tags: %w[a b]}

      expect(type.apply(input, structural_sharing: true)).to be(input)
      expect(calls).to eql(2)

      expect(type.apply(input, structural_sharing: true)).to be(input)
      expect(calls).to eql(4)
    end

    it "reuses containers reached through safe calls" do
      schema = Dry::Types["nominal.hash"].schema(
        name: "string",
        tags: Dry::Types["array"].of(Dry::Types["string"])
      )
      input = {name: "Jane", tags: %w[a b]}

      expect(schema.call_safe(input, structural_sharing: true)).to be(input)
    end

    it "does not reuse containers without the option" do
      input = conforming_input

      output = schema.apply(input)

      expect(output).to eql(input)
      expect(output).not_to be(input)
      expect(output[:tags]).not_to be(input[:tags])
    end
  end

  describe "when a nested value is transformed" do
    it "rebuilds only the containers on the changed path" do
      input = conforming_input
      input[:address] = {city: "Berlin", zip: "10115"}

      output = schema.apply(input, structural_sharing: true)

      expect(output).not_to be(input)
      expect(output[:address]).not_to be(input[:address])
      expect(output[:address][:zip]).to eql(10_115)
      expect(output[:tags]).to be(input[:tags])
      expect(output[:name]).to be(input[:name])
    end

    it "rebuilds the array when an element is coerced" do
      schema = Dry::Types["hash"].schema(
        nums: Dry::Types["array"].of(Dry::Types["coercible.integer"])
      )
      input = {nums: [1, "2"]}

      output = schema.apply(input, structural_sharing: true)

      expect(output[:nums]).to eql([1, 2])
      expect(output[:nums]).not_to be(input[:nums])
    end
  end

  describe "identity semantics" do
    it "does not reuse containers when == holds but identity changed" do
      schema = Dry::Types["hash"].schema(
        nums: Dry::Types["array"].of(Dry::Types["coercible.integer"])
      )
      input = {nums: [1.0]}

      output = schema.apply(input, structural_sharing: true)

      expect(output[:nums]).to eql([1])
      expect(output[:nums]).not_to be(input[:nums])
      expect(output).not_to be(input)
    end

    it "reuses containers holding immediate values" do
      schema = Dry::Types["hash"].schema(
        nums: Dry::Types["array"].of(Dry::Types["coercible.integer"])
      )
      input = {nums: [1, 2]}

      expect(schema.apply(input, structural_sharing: true)).to be(input)
    end

    it "rebuilds containers when a constructor returns a new object each time" do
      calls = 0
      dupping = Dry::Types["string"].constructor { |v| calls += 1; v.dup }
      schema = Dry::Types["hash"].schema(name: dupping)

      input = {name: "Jane"}
      output = schema.apply(input, structural_sharing: true)

      expect(calls).to eql(1)
      expect(output).not_to be(input)
      expect(output[:name]).to eql("Jane")
      expect(output[:name]).not_to be(input[:name])
    end
  end

  describe "key handling" do
    it "rebuilds the hash when unknown keys are dropped" do
      input = conforming_input.merge(extra: "key")

      output = schema.apply(input, structural_sharing: true)

      expect(output).to eql(conforming_input)
      expect(output).not_to be(input)
    end

    it "rebuilds the hash when keys are transformed" do
      type = schema.with_key_transform(&:to_sym)
      input = {
        "name" => "Jane",
        "age" => 30,
        "tags" => %w[a b],
        "address" => {city: "Berlin", zip: 10_115}
      }

      output = type.apply(input, structural_sharing: true)

      expect(output).to eql(conforming_input)
      expect(output).not_to be(input)
      expect(output[:address]).to be(input["address"])
    end

    it "reuses the hash when key transformation is a no-op" do
      type = schema.with_key_transform(&:to_sym)
      input = conforming_input

      expect(type.apply(input, structural_sharing: true)).to be(input)
    end
  end

  describe "defaults and missing keys" do
    subject(:schema) do
      Dry::Types["hash"].schema(
        name: "string",
        role: Dry::Types["string"].default("user")
      )
    end

    it "rebuilds the hash when a default is filled in" do
      input = {name: "Jane"}

      output = schema.apply(input, structural_sharing: true)

      expect(output).to eql(name: "Jane", role: "user")
      expect(output).not_to be(input)
    end

    it "reuses the hash when the default key is already present" do
      input = {name: "Jane", role: "user"}

      expect(schema.apply(input, structural_sharing: true)).to be(input)
    end
  end

  describe "undefined element filtering" do
    subject(:schema) do
      skipping = Dry::Types["nominal.string"].constructor { |v|
        v == "skip" ? Dry::Types::Undefined : v
      }

      Dry::Types["hash"].schema(tags: Dry::Types["array"].of(skipping))
    end

    it "rebuilds the array when elements are filtered out" do
      input = {tags: %w[a skip b]}

      output = schema.apply(input, structural_sharing: true)

      expect(output[:tags]).to eql(%w[a b])
      expect(output[:tags]).not_to be(input[:tags])
      expect(output).not_to be(input)
    end

    it "reuses the array when nothing is filtered" do
      input = {tags: %w[a b]}

      expect(schema.apply(input, structural_sharing: true)).to be(input)
    end
  end

  describe "failures" do
    it "keeps the input structure intact when coercion raises" do
      input = conforming_input
      input[:address] = {city: "Berlin", zip: "not-a-number"}
      snapshot = Marshal.load(Marshal.dump(input))

      expect { schema.apply(input, structural_sharing: true) }
        .to raise_error(Dry::Types::SchemaError)

      expect(input).to eql(snapshot)
    end

    it "keeps the input structure intact when a safe call fails" do
      schema = Dry::Types["nominal.hash"].schema(age: "coercible.integer")
      input = conforming_input
      input[:age] = "not-a-number"
      snapshot = Marshal.load(Marshal.dump(input))

      expect(schema.call_safe(input, structural_sharing: true) { :failed }).to be(:failed)

      expect(input).to eql(snapshot)
    end
  end
end
