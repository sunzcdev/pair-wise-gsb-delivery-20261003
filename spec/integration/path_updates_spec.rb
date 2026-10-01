# frozen_string_literal: true

RSpec.describe Dry::Struct, "#update_in" do
  let(:types) { Dry.Types() }

  let(:address_class) do
    Class.new(Dry::Struct) do
      attribute :city, Dry::Types["strict.string"]
      attribute :street, Dry::Types["strict.string"]
      attribute? :zipcode, Dry::Types["strict.string"]
    end
  end

  let(:order_class) do
    address_class = self.address_class
    types = self.types

    Class.new(Dry::Struct) do
      attribute :name, Dry::Types["strict.string"]
      attribute :address, address_class
      attribute :items, types::Array.of(Dry::Types["strict.string"])
    end
  end

  let(:address) do
    address_class.new(city: "Paris", street: "Rue de Rivoli", zipcode: "75001")
  end

  subject(:order) do
    order_class.new(
      name: "Books",
      address: address,
      items: ["book", "pen"]
    )
  end

  it "changes a nested struct attribute and keeps the original unchanged" do
    updated = order.update_in([:address, :street], "Avenue des Champs")

    expect(updated.address.street).to eql("Avenue des Champs")
    expect(order.address.street).to eql("Rue de Rivoli")
    expect(updated).not_to equal(order)
  end

  it "keeps values and unrelated nested objects" do
    updated = order.update_in([:address, :street], "Avenue des Champs")

    expect(updated.name).to eql("Books")
    expect(updated.address.city).to eql("Paris")
    expect(updated.address.zipcode).to eql("75001")
    expect(updated.items).to eql(["book", "pen"])
    expect(updated.items).to equal(order.items)
  end

  it "changes an array element without resizing the array" do
    updated = order.update_in([:items, 1], "pencil")

    expect(updated.items).to eql(["book", "pencil"])
    expect(updated.address).to equal(order.address)
    expect(updated.items).not_to equal(order.items)
  end

  it "coerces and checks the new value through its attribute type" do
    integer_type = Dry::Types["coercible.integer"]
    klass = Class.new(Dry::Struct) do
      attribute :nested do
        attribute :count, integer_type
      end
    end

    object = klass.new(nested: {count: "1"})
    updated = object.update_in([:nested, :count], "2")

    expect(updated.nested.count).to eql(2)
  end

  it "raises the struct error when the new value has the wrong type" do
    expect { order.update_in([:address, :city], 1) }
      .to raise_error(Dry::Struct::Error, /:city/)
  end

  it "does not change anything when a value fails its type" do
    expect { order.update_in([:address, :city], 1) }.to raise_error(Dry::Struct::Error)

    expect(order.address.city).to eql("Paris")
  end

  it "does not run constructors for unchanged values" do
    calls = 0
    nested_type = Dry::Types["strict.integer"]
      .constructor(-> value { calls += 1; value })
    klass = Class.new(Dry::Struct) do
      attribute :address do
        attribute :street, Dry::Types["strict.string"]
        attribute :number, nested_type
      end
    end

    object = klass.new(address: {street: "Main", number: 12})
    calls_after_initialization = calls

    updated = object.update_in([:address, :street], "Broadway")

    expect(updated.address.number).to eql(12)
    expect(calls).to eql(calls_after_initialization)
  end

  it "does not reconstruct an existing struct passed as a replacement" do
    calls = 0
    nested_class = Class.new(Dry::Struct) do
      attribute :city, Dry::Types["strict.string"]
    end
    nested_type = nested_class.constructor(-> input {
      calls += 1
      nested_class.call(input)
    })
    klass = Class.new(Dry::Struct) do
      attribute :address, nested_type
    end

    object = klass.new(address: {city: "Paris"})
    replacement = nested_class.new(city: "Rome")
    calls_after_initialization = calls

    updated = object.update_in([:address], replacement)

    expect(updated.address).to equal(replacement)
    expect(calls).to eql(calls_after_initialization)
  end

  it "does not re-run a custom constructor for the enclosing struct" do
    calls = 0
    klass = Class.new(Dry::Struct) do
      attribute :address do
        attribute :city, Dry::Types["strict.string"]
      end
    end
    type = klass.constructor(-> input, struct_type {
      calls += 1
      struct_type.call(input)
    })

    object = type.call(address: {city: "Paris"})
    calls_after_initialization = calls

    updated = object.update_in([:address, :city], "Rome")

    expect(updated.address.city).to eql("Rome")
    expect(calls).to eql(calls_after_initialization)
  end

  describe "nil and deletion" do
    it "assigns nil when the terminal type accepts nil" do
      klass = Class.new(Dry::Struct) do
        attribute :address do
          attribute :city, Dry::Types["strict.string"].optional
        end
      end

      object = klass.new(address: {city: "Paris"})
      updated = object.update_in([:address, :city], nil)

      expect(updated.address.city).to be_nil
      expect(updated.to_h).to eql(address: {city: nil})
    end

    it "deletes an optional struct attribute with the explicit sentinel" do
      updated = order.update_in([:address, :zipcode], described_class::DELETE)

      expect(updated.address.zipcode).to be_nil
      expect(updated.address.attributes).to eql(city: "Paris", street: "Rue de Rivoli")
      expect(updated.to_h).to eql(
        name: "Books",
        address: {city: "Paris", street: "Rue de Rivoli"},
        items: ["book", "pen"]
      )
    end

    it "supports delete_in" do
      updated = order.delete_in([:address, :zipcode])

      expect(updated.address.attributes.key?(:zipcode)).to be(false)
      expect(order.address.attributes.key?(:zipcode)).to be(true)
    end

    it "refuses to delete a required attribute" do
      expect { order.delete_in([:address, :city]) }
        .to raise_error(ArgumentError, /required attribute/)
    end

    it "refuses to delete an array member" do
      expect { order.update_in([:items, 0], described_class::DELETE) }
        .to raise_error(ArgumentError, /array member/)
    end
  end

  describe "paths" do
    it "rejects unknown attributes" do
      expect { order.update_in([:address, :country], "FR") }
        .to raise_error(Dry::Struct::MissingAttributeError)
    end

    it "rejects traversal through a non-struct attribute" do
      expect { order.update_in([:name, :first], "Jane") }
        .to raise_error(Dry::Struct::Error, /invalid path/)
    end

    it "rejects an out-of-bounds array index" do
      expect { order.update_in([:items, 2], "bag") }
        .to raise_error(Dry::Struct::Error, /outside of array bounds/)
    end

    it "rejects a negative array index" do
      expect { order.update_in([:items, -1], "bag") }
        .to raise_error(Dry::Struct::Error, /negative/)
    end

    it "rejects a malformed path" do
      expect { order.update_in(:address, "Rome") }.to raise_error(ArgumentError)
      expect { order.update_in([], "Rome") }.to raise_error(ArgumentError)
      expect { order.update_in([:address, "city"], "Rome") }.to raise_error(ArgumentError)
    end
  end

  describe "#update_paths" do
    it "applies several changes atomically from a path-keyed hash" do
      updated = order.update_paths(
        [:address, :city] => "Rome",
        [:items, 0] => "map"
      )

      expect(updated.address.city).to eql("Rome")
      expect(updated.items).to eql(["map", "pen"])
    end

    it "applies several changes passed as path/value arguments" do
      updated = order.update_paths(
        [:address, :city], "Rome",
        [:items, 0], "map"
      )

      expect(updated.address.city).to eql("Rome")
      expect(updated.items).to eql(["map", "pen"])
    end

    it "rejects a parent path and one of its descendants" do
      expect {
        order.update_paths(
          [:address] => {city: "Rome", street: "Via"},
          [:address, :city] => "Milan"
        )
      }.to raise_error(ArgumentError, /conflicting paths/)
    end

    it "rejects duplicate paths" do
      expect {
        order.update_paths(
          [
            [[:address, :city], "Rome"],
            [[:address, :city], "Milan"]
          ]
        )
      }.to raise_error(ArgumentError, /conflicting paths/)
    end

    it "returns the original object for an empty changeset" do
      expect(order.update_paths({})).to equal(order)
      expect(order.update_paths([])).to equal(order)
      expect(order.update_paths).to equal(order)
    end

    it "rejects the batch without applying the valid change" do
      expect {
        order.update_paths(
          [:address, :city] => "Rome",
          [:items, 1] => 42
        )
      }.to raise_error(Dry::Struct::Error)

      expect(order.address.city).to eql("Paris")
      expect(order.items).to eql(["book", "pen"])
    end
  end

  describe "frozen value structs", :suppress_deprecations do
    before { require "dry/struct/value" }

    it "deep freezes the updated copy" do
      address_class = self.address_class
      value_class = Class.new(Dry::Struct::Value) do
        attribute :address, address_class
      end
      value = value_class.new(address: {city: "Paris", street: "Rue"})

      updated = value.update_in([:address, :city], "Rome")

      expect(updated).to be_frozen
      expect(updated.address).to be_frozen
      expect(value.address.city).to eql("Paris")
    end
  end

  describe "nested arrays" do
    let(:order_class) do
      types = self.types

      Class.new(Dry::Struct) do
        attribute :groups, types::Array.of(types::Array.of(Dry::Types["strict.string"]))
      end
    end

    subject(:order) { order_class.new(groups: [%w[a b], %w[c d]]) }

    it "changes a nested array element and preserves other arrays" do
      updated = order.update_in([:groups, 1, 0], "x")

      expect(updated.groups).to eql([%w[a b], %w[x d]])
      expect(updated.groups[0]).to equal(order.groups[0])
      expect(updated.groups[1]).not_to equal(order.groups[1])
    end
  end

  describe "arrays of structs" do
    let(:order_class) do
      address_class = self.address_class
      types = self.types

      Class.new(Dry::Struct) do
        attribute :addresses, types::Array.of(address_class)
      end
    end

    subject(:order) do
      order_class.new(addresses: [address, address_class.new(city: "Rome", street: "Via")])
    end

    it "changes a nested struct in an array" do
      updated = order.update_in([:addresses, 1, :city], "Milan")

      expect(updated.addresses[1].city).to eql("Milan")
      expect(updated.addresses[0]).to equal(order.addresses[0])
    end
  end
end
