# frozen_string_literal: true

RSpec.describe Dry::Struct, "path updates" do
  before do
    module Test
      class Address < Dry::Struct
        attribute :city, "string"
        attribute? :note, "string"
      end

      class Line < Dry::Struct
        attribute :qty, "integer"
      end

      class Order < Dry::Struct
        attribute :id, "coercible.integer"
        attribute :address, Test::Address
        attribute :tags, Dry::Types["array"].of(Dry::Types["string"])
        attribute :lines, Dry::Types["array"].of(Test::Line)
        attribute :subtitle, Dry::Types["string"].optional
      end
    end
  end

  subject(:order) do
    Test::Order.new(
      id: 1,
      address: {city: "Paris", note: "leave at door"},
      tags: %w[gift],
      lines: [{qty: 1}],
      subtitle: "sub"
    )
  end

  describe "#update_in" do
    it "updates a nested struct attribute without touching the original" do
      updated = order.update_in(:address, :city, "Lyon")

      expect(updated.address.city).to eql("Lyon")
      expect(updated.address.note).to eql("leave at door")
      expect(order.address.city).to eql("Paris")
    end

    it "accepts the path as a single array" do
      expect(order.update_in([:address, :city], "Lyon").address.city).to eql("Lyon")
    end

    it "updates an array element by index" do
      updated = order.update_in(:tags, 0, "urgent")

      expect(updated.tags).to eql(["urgent"])
      expect(order.tags).to eql(%w[gift])
    end

    it "supports negative array indices" do
      expect(order.update_in(:tags, -1, "urgent").tags).to eql(["urgent"])
    end

    it "updates a struct nested inside an array" do
      updated = order.update_in(:lines, 0, :qty, 5)

      expect(updated.lines[0].qty).to be(5)
      expect(order.lines[0].qty).to be(1)
    end

    it "coerces new values through the attribute type" do
      updated = order.update_in(:id, "2")

      expect(updated.id).to be(2)
    end

    it "replaces a whole nested struct when the path ends at it" do
      updated = order.update_in(:address, city: "Lyon")

      expect(updated.address).to eql(Test::Address.new(city: "Lyon"))
    end

    it "assigns nil without removing the attribute" do
      updated = order.update_in(:subtitle, nil)

      expect(updated.to_h).to have_key(:subtitle)
      expect(updated.subtitle).to be_nil
    end

    it "raises a struct error when a value does not pass its type check" do
      expect { order.update_in(:id, "one") }.to raise_error(
        Dry::Struct::Error, /invalid value "one" at path \[:id\]/
      )
    end

    it "raises a struct error when nil is assigned to a non-optional type" do
      expect { order.update_in(:id, nil) }.to raise_error(Dry::Struct::Error)
    end

    it "raises a struct error when an array element does not pass its type check" do
      expect { order.update_in(:tags, 0, nil) }.to raise_error(Dry::Struct::Error)
    end

    it "raises when an attribute does not exist" do
      expect { order.update_in(:unknown, 1) }.to raise_error(Dry::Struct::MissingAttributeError)
    end

    it "raises when the path goes through a non-struct value" do
      expect { order.update_in(:id, :to_i, 1) }.to raise_error(
        Dry::Struct::InvalidPathError, /neither a Dry::Struct nor an Array/
      )
    end

    it "raises when a struct is traversed with an array index" do
      expect { order.update_in(:address, 0, "x") }.to raise_error(
        Dry::Struct::InvalidPathError, /is not an attribute/
      )
    end

    it "raises when an array is traversed with an attribute name" do
      expect { order.update_in(:tags, :first, "x") }.to raise_error(
        Dry::Struct::InvalidPathError, /array indices must be Integers/
      )
    end

    it "raises when an array index is out of bounds instead of expanding the array" do
      expect { order.update_in(:tags, 1, "x") }.to raise_error(
        Dry::Struct::InvalidPathError, /out of bounds/
      )
    end

    it "raises on an empty path" do
      expect { order.update_in(1) }.to raise_error(
        Dry::Struct::InvalidPathError, /cannot be empty/
      )
    end

    it "raises on invalid path segments" do
      expect { order.update_in("address", "Lyon") }.to raise_error(
        Dry::Struct::InvalidPathError, /invalid path segment/
      )
    end
  end

  describe "#delete_in" do
    it "removes an optional attribute" do
      updated = order.delete_in(:address, :note)

      expect(updated.address.to_h).to eql(city: "Paris")
      expect(order.address.to_h).to eql(city: "Paris", note: "leave at door")
    end

    it "is equivalent to passing DELETE as the value" do
      expect(order.update_in(:address, :note, Dry::Struct::DELETE))
        .to eql(order.delete_in(:address, :note))
    end

    it "raises when deleting a required attribute" do
      expect { order.delete_in(:id) }.to raise_error(
        Dry::Struct::RequiredAttributeError, /cannot delete required attribute :id/
      )
    end

    it "raises when deleting an array element" do
      expect { order.delete_in(:tags, 0) }.to raise_error(
        Dry::Struct::InvalidPathError, /cannot delete array element/
      )
    end
  end

  describe "#update" do
    it "applies a batch of updates at once" do
      updated = order.update(
        [:address, :city] => "Lyon",
        [:tags, 0] => "urgent",
        id: 2
      )

      expect(updated.to_h).to eql(
        id: 2,
        address: {city: "Lyon", note: "leave at door"},
        tags: ["urgent"],
        lines: [{qty: 1}],
        subtitle: "sub"
      )
    end

    it "supports DELETE in a batch" do
      updated = order.update([:address, :note] => Dry::Struct::DELETE)

      expect(updated.address.to_h).to eql(city: "Paris")
    end

    it "returns self when there is nothing to update" do
      expect(order.update({})).to be(order)
    end

    it "rejects a batch covering both a path and its subpath" do
      expect {
        order.update([:address] => {city: "Lyon"}, [:address, :city] => "Nice")
      }.to raise_error(Dry::Struct::AmbiguousPathError, /overlaps/)
    end

    it "rejects a batch covering both a subpath and its parent path" do
      expect {
        order.update([:address, :city] => "Nice", [:address] => {city: "Lyon"})
      }.to raise_error(Dry::Struct::AmbiguousPathError, /overlaps/)
    end

    it "rejects duplicated paths" do
      expect {
        order.update([[[ :address, :city ], "Nice"], [[ :address, :city ], "Lyon"]])
      }.to raise_error(Dry::Struct::AmbiguousPathError, /overlaps/)
    end

    it "applies nothing when any value fails its type check" do
      expect {
        order.update([:address, :city] => "Lyon", [:id] => "one")
      }.to raise_error(Dry::Struct::Error)

      expect(order.id).to be(1)
      expect(order.address.city).to eql("Paris")
    end
  end

  describe "structural sharing" do
    it "keeps unmodified nested objects identical" do
      updated = order.update_in(:lines, 0, :qty, 5)

      expect(updated.address).to be(order.address)
      expect(updated.tags).to be(order.tags)
      expect(updated.lines[0]).not_to be(order.lines[0])
    end

    it "keeps unmodified array elements identical" do
      order = Test::Order.new(
        id: 1,
        address: {city: "Paris"},
        tags: %w[a],
        lines: [{qty: 1}, {qty: 2}],
        subtitle: nil
      )

      updated = order.update_in(:lines, 0, :qty, 10)

      expect(updated.lines[1]).to be(order.lines[1])
    end
  end

  describe "constructor invocation" do
    it "does not re-coerce old data" do
      count = 0
      counted = Dry::Types["integer"].constructor { |value| count += 1; value.to_i }

      struct = Class.new(Dry::Struct) do
        attribute :n, counted
        attribute :name, Dry::Types["string"]
      end

      instance = struct.new(n: 1, name: "a")
      expect(count).to be(1)

      updated = instance.update_in(:name, "b")
      expect(count).to be(1)
      expect(updated.n).to be(1)

      updated = instance.update_in(:n, "5")
      expect(count).to be(2)
      expect(updated.n).to be(5)
    end
  end
end
