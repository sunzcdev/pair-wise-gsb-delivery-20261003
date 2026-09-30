# frozen_string_literal: true

RSpec.describe Dry::Struct::Builder do
  before do
    stub_const("Test::Address", Class.new(Dry::Struct) do
      attribute :city, Dry::Types["string"]
      attribute :zip, Dry::Types["string"].default("00000".freeze)
    end)

    stub_const("Test::User", Class.new(Dry::Struct) do
      attribute :name, Dry::Types["string"]
      attribute :email, Dry::Types["string"].optional
      attribute? :nickname, Dry::Types["string"]
      attribute :role, Dry::Types["string"].default("user".freeze)
      attribute :address, Test::Address
      attribute :friends, Dry::Types["array"].of(Test::Address)
    end)
  end

  let(:builder) { described_class.new(Test::User) }

  it "is also accessible via the struct class" do
    expect(Test::User.builder).to be_a(described_class)
  end

  it "rejects non-struct classes" do
    expect { described_class.new(::String) }.to raise_error(ArgumentError)
  end

  describe "#set" do
    it "stages values across multiple calls and finishes" do
      builder.set(name: "Jane")
      builder.set(address: {city: "NYC"}, friends: [])
      builder.set(email: nil)

      user = builder.finish

      expect(user).to eql(
        Test::User.new(
          name: "Jane",
          email: nil,
          role: "user",
          address: {city: "NYC", zip: "00000"},
          friends: []
        )
      )
    end

    it "validates given values immediately" do
      expect { builder.set(name: :Jane) }.to raise_error(Dry::Struct::Error, /name/)
    end

    it "treats explicit nil as a value and checks it against the type" do
      expect { builder.set(name: nil) }.to raise_error(Dry::Struct::Error)

      builder.set(name: "Jane", email: nil, address: {city: "NYC"}, friends: [])
      expect(builder.finish.email).to be_nil
    end

    it "keeps previously staged values when a batch fails" do
      builder.set(name: "Jane")

      expect { builder.set(name: :Jane, email: "jane@doe.test") }.to raise_error(Dry::Struct::Error)

      expect(builder.missing).to contain_exactly("email", "address", "friends")
    end

    it "applies a failing batch atomically" do
      expect { builder.set(name: "Jane", address: {city: :NYC}) }.to raise_error(Dry::Struct::Error)

      expect(builder.missing).to contain_exactly("name", "email", "address", "friends")
    end

    it "reports nested validation errors with their path" do
      expect { builder.set(address: {city: :NYC}) }
        .to raise_error(Dry::Struct::Error, /address.*city/)
    end

    it "reports array element validation errors with their path" do
      expect { builder.set(friends: [{city: "NYC"}, {city: :Atlantis}]) }
        .to raise_error(Dry::Struct::Error, /friends\[1\].*city/)
    end

    it "merges nested hashes staged in separate batches" do
      builder.set(address: {city: "NYC"})
      builder.set(address: {zip: "12345"})

      expect(builder.missing).to contain_exactly("name", "email", "friends")
    end
  end

  describe "#missing" do
    it "lists gaps in nested structs with full paths" do
      builder.set(name: "Jane", friends: [], address: {})

      expect(builder.missing).to contain_exactly("email", "address.city")
    end

    it "lists gaps in arrays of structs with full paths" do
      builder.set(name: "Jane", address: {city: "NYC"}, friends: [{city: "NYC"}, {}])

      expect(builder.missing).to contain_exactly("email", "friends[1].city")
    end

    it "lists neither attributes with defaults nor omittable attributes" do
      expect(builder.missing).to contain_exactly("name", "email", "address", "friends")
    end
  end

  describe "#finish" do
    it "raises an IncompleteError listing full paths of missing attributes" do
      builder.set(friends: [{}])

      expect { builder.finish }.to raise_error(
        Dry::Struct::Builder::IncompleteError,
        /name, email, address, friends\[0\]\.city/
      )
    end

    it "exposes missing paths on the error" do
      builder.set(name: "Jane")

      begin
        builder.finish
      rescue Dry::Struct::Builder::IncompleteError => e
        expect(e.paths).to contain_exactly("email", "address", "friends")
      end
    end

    it "allows filling the gaps after a failed attempt" do
      builder.set(name: "Jane")

      expect { builder.finish }.to raise_error(Dry::Struct::Builder::IncompleteError)

      builder.set(email: nil, address: {city: "NYC"}, friends: [])

      expect(builder.finish).to be_a(Test::User)
    end

    it "evaluates defaults only at finish time" do
      evaluated = []
      stub_const("Test::Event", Class.new(Dry::Struct) do
        attribute :id, Dry::Types["integer"]
        attribute :token, Dry::Types["string"].default { evaluated << :token; "t1" }
      end)

      event_builder = described_class.new(Test::Event)
      event_builder.set(id: 1)

      expect(evaluated).to be_empty
      expect(event_builder.finish.token).to eql("t1")
      expect(evaluated).to eql([:token])
    end

    it "does not re-evaluate defaults that were already resolved by a failed finish" do
      evaluated = []
      stub_const("Test::Event", Class.new(Dry::Struct) do
        attribute :id, Dry::Types["integer"]
        attribute :token, Dry::Types["string"].default { evaluated << :token; "t1" }
      end)

      event_builder = described_class.new(Test::Event)

      expect { event_builder.finish }.to raise_error(Dry::Struct::Builder::IncompleteError)
      expect(evaluated).to eql([:token])

      event_builder.set(id: 1)

      expect(event_builder.finish.token).to eql("t1")
      expect(evaluated).to eql([:token])
    end

    it "evaluates nested defaults once across failed attempts" do
      evaluated = []
      stub_const("Test::Geo", Class.new(Dry::Struct) do
        attribute :lat, Dry::Types["float"]
        attribute :tag, Dry::Types["string"].default { evaluated << :tag; "home" }
      end)
      stub_const("Test::Checkin", Class.new(Dry::Struct) do
        attribute :geo, Test::Geo
      end)

      checkin_builder = described_class.new(Test::Checkin)
      checkin_builder.set(geo: {})

      expect { checkin_builder.finish }.to raise_error(Dry::Struct::Builder::IncompleteError, /geo\.lat/)
      expect(evaluated).to eql([:tag])

      checkin_builder.set(geo: {lat: 1.5})

      expect(checkin_builder.finish.geo.tag).to eql("home")
      expect(evaluated).to eql([:tag])
    end
  end
end
