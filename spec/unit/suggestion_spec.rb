# frozen_string_literal: true

RSpec.describe Dry::Validation::Suggestion do
  def delete(path)
    described_class.delete(path)
  end

  def replace(path, value)
    described_class.replace(path, value)
  end

  describe ".[]" do
    it "coerces a hash spec" do
      suggestion = described_class[{operation: :replace, path: [:status], value: "active"}]

      expect(suggestion).to eql(replace([:status], "active"))
    end

    it "returns a suggestion as-is" do
      suggestion = delete([:legacy])

      expect(described_class[suggestion]).to be(suggestion)
    end

    it "rejects unknown operations" do
      expect { described_class[{operation: :append, path: [:tags]}] }.to raise_error(ArgumentError)
    end
  end

  describe "#conflicts_with?" do
    it "does not conflict with an identical suggestion" do
      expect(replace([:status], "active").conflicts_with?(replace([:status], "active")))
        .to be(false)
    end

    it "conflicts with a different replacement for the same path" do
      expect(replace([:status], "active").conflicts_with?(replace([:status], "archived")))
        .to be(true)
    end

    it "conflicts with a different operation for the same path" do
      expect(delete([:status]).conflicts_with?(replace([:status], "active"))).to be(true)
    end

    it "conflicts when one path is a parent of the other" do
      expect(delete([:address]).conflicts_with?(replace([:address, :city], "Berlin"))).to be(true)
      expect(replace([:address, :city], "Berlin").conflicts_with?(delete([:address]))).to be(true)
    end

    it "does not conflict for sibling keys" do
      expect(replace([:a], 1).conflicts_with?(replace([:b], 2))).to be(false)
    end

    it "conflicts when deleting an array element and touching another index of the same array" do
      expect(delete([:tags, 0]).conflicts_with?(replace([:tags, 1], "x"))).to be(true)
      expect(delete([:tags, 0]).conflicts_with?(delete([:tags, 2]))).to be(true)
    end

    it "does not conflict for replacements of different indices of the same array" do
      expect(replace([:tags, 0], "x").conflicts_with?(replace([:tags, 1], "y"))).to be(false)
    end

    it "does not conflict for indices of different arrays" do
      expect(delete([:tags, 0]).conflicts_with?(replace([:links, 0], "x"))).to be(false)
    end
  end

  describe "#apply" do
    it "deletes a key from a hash" do
      expect(delete([:legacy]).apply({legacy: 1, name: "Jane"})).to eql({name: "Jane"})
    end

    it "deletes an array element by index" do
      expect(delete([:tags, 0]).apply({tags: %w[a b]})).to eql({tags: %w[b]})
    end

    it "replaces a nested value" do
      expect(replace([:address, :city], "Berlin").apply({address: {city: "Paris"}}))
        .to eql({address: {city: "Berlin"}})
    end

    it "tolerates missing paths" do
      expect(delete([:legacy]).apply({name: "Jane"})).to eql({name: "Jane"})
    end
  end
end
