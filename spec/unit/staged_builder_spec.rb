# frozen_string_literal: true

RSpec.describe Dry::Struct::StagedBuilder do
  before do
    module Test
      class Address < Dry::Struct
        attribute :city, "string"
        attribute :zipcode, "coercible.string"
      end

      class User < Dry::Struct
        attribute :name, "string"
        attribute :age, "integer"
        attribute? :email, "optional.string"
        attribute :address, Test::Address
        attribute :friends, Dry::Types["strict.array"].of(Test::Address)
      end
    end
  end

  let(:builder) { Test::User.builder }

  describe ".builder" do
    it "returns a staged builder for the struct class" do
      expect(builder).to be_a(Dry::Struct::StagedBuilder)
      expect(builder.struct_class).to be(Test::User)
    end
  end

  describe "#assign" do
    it "returns self so calls can be chained" do
      expect(builder.assign(name: "Jane")).to be(builder)
    end

    it "validates given values right away" do
      expect { builder.assign(age: "21") }
        .to raise_error(Dry::Struct::Error, /\[Test::User.builder\] age:/)
    end

    it "raises on unknown keys" do
      expect { builder.assign(name: "Jane", unknown: 1) }
        .to raise_error(Dry::Struct::Error, /unknown attribute: :unknown/)
    end

    it "handles explicit nil according to the attribute type" do
      expect { builder.assign(name: nil) }.to raise_error(Dry::Struct::Error)

      builder.assign(email: nil)
      expect(finish_with(name: "Jane", age: 21).email).to be_nil
    end

    it "keeps previously stored attributes when a batch fails" do
      builder.assign(name: "Jane")

      expect { builder.assign(age: "oops") }.to raise_error(Dry::Struct::Error)

      expect(finish_with(age: 21).name).to eql("Jane")
    end

    it "rejects the whole batch atomically" do
      expect { builder.assign(name: "Jane", age: "oops") }
        .to raise_error(Dry::Struct::Error)

      expect { builder.finish }
        .to raise_error(described_class::MissingAttributesError, /name/)
    end
  end

  describe "#finish" do
    it "builds a struct from attributes assigned in multiple steps" do
      user = builder
        .assign(name: "Jane")
        .assign(age: 21, address: {city: "NYC", zipcode: 123})
        .assign(friends: [{city: "LA", zipcode: "456"}])
        .finish

      expect(user).to eql(
        Test::User.new(
          name: "Jane",
          age: 21,
          address: {city: "NYC", zipcode: "123"},
          friends: [{city: "LA", zipcode: "456"}]
        )
      )
    end

    it "does not require omittable attributes" do
      expect(finish_with(name: "Jane", age: 21).email).to be_nil
    end

    it "accepts struct instances for nested attributes" do
      address = Test::Address.new(city: "NYC", zipcode: "123")
      user = builder
        .assign(name: "Jane", age: 21, address: address)
        .assign(friends: [address])
        .finish

      expect(user.address).to be(address)
      expect(user.friends).to eql([address])
    end

    it "raises MissingAttributesError listing every missing attribute" do
      expect { builder.finish }.to raise_error(
        described_class::MissingAttributesError,
        "[Test::User.builder] missing attributes: name, age, address, friends"
      )
    end

    it "exposes missing paths as segments" do
      builder.assign(address: {city: "NYC"}, friends: [{zipcode: "123"}])

      expect { builder.finish }.to raise_error(described_class::MissingAttributesError) { |error|
        expect(error.paths).to eql(
          [[:name], [:age], [:address, :zipcode], [:friends, 0, :city]]
        )
      }
    end

    it "reports gaps in nested structs with their full path" do
      builder.assign(address: {city: "NYC"})

      expect { builder.finish }
        .to raise_error(described_class::MissingAttributesError, /address\.zipcode/)
    end

    it "reports gaps in struct arrays with their full path including the index" do
      builder.assign(friends: [{city: "LA"}, {zipcode: "123"}])

      expect { builder.finish }.to raise_error(
        described_class::MissingAttributesError,
        /friends\[0\]\.zipcode, friends\[1\]\.city/
      )
    end

    it "allows continuing after a failed attempt" do
      builder.assign(name: "Jane", address: {city: "NYC"})

      expect { builder.finish }.to raise_error(described_class::MissingAttributesError)

      builder.assign(age: 21, address: {zipcode: "123"}, friends: [])

      expect(builder.finish).to eql(
        Test::User.new(
          name: "Jane",
          age: 21,
          address: {city: "NYC", zipcode: "123"},
          friends: []
        )
      )
    end

    it "merges nested hashes assigned in separate steps" do
      user = builder
        .assign(address: {city: "NYC"})
        .assign(address: {zipcode: "123"})
        .assign(name: "Jane", age: 21, friends: [])
        .finish

      expect(user.address).to eql(Test::Address.new(city: "NYC", zipcode: "123"))
    end

    it "merges struct array elements by index" do
      builder
        .assign(friends: [{city: "LA"}, {city: "SF"}])
        .assign(friends: [{zipcode: "1"}, {zipcode: "2"}])
        .assign(name: "Jane", age: 21, address: {city: "NYC", zipcode: "123"})

      expect(builder.finish.friends).to eql(
        [
          Test::Address.new(city: "LA", zipcode: "1"),
          Test::Address.new(city: "SF", zipcode: "2")
        ]
      )
    end
  end

  describe "default values" do
    let(:defaults) { Hash.new(0) }

    before do
      counter = defaults

      stub_const("Test::Event", Class.new(Dry::Struct) do
        attribute :id, "integer"
        attribute :stamp, Dry::Types["integer"].default { counter[:stamp] += 1 }
      end)
    end

    let(:builder) { Test::Event.builder }

    it "resolves defaults only at finish time" do
      builder.assign(id: 1)

      expect(defaults[:stamp]).to be(0)
      expect(builder.finish.stamp).to be(1)
      expect(defaults[:stamp]).to be(1)
    end

    it "does not re-evaluate defaults resolved before a failed finish" do
      expect { builder.finish }.to raise_error(described_class::MissingAttributesError)
      expect(defaults[:stamp]).to be(1)

      builder.assign(id: 1)

      expect(builder.finish.stamp).to be(1)
      expect(defaults[:stamp]).to be(1)
    end

    it "does not re-evaluate defaults on repeated finishes" do
      builder.assign(id: 1)

      expect(builder.finish.stamp).to be(1)
      expect(builder.finish.stamp).to be(1)
      expect(defaults[:stamp]).to be(1)
    end

    it "ignores defaults for explicitly assigned values" do
      builder.assign(id: 1, stamp: 10)

      expect(builder.finish.stamp).to be(10)
      expect(defaults[:stamp]).to be(0)
    end
  end

  def finish_with(attributes)
    builder
      .assign(address: {city: "NYC", zipcode: "123"}, friends: [])
      .assign(attributes)
      .finish
  end
end
