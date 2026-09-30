# frozen_string_literal: true

RSpec.describe "Dry::Struct.project" do
  before do
    module Test
      class Address < Dry::Struct
        transform_keys(&:to_sym)

        attribute :city, "string"
        attribute :zipcode, Dry::Types["string"].optional
        attribute? :country, Dry::Types["string"].default("us".freeze)
      end

      class Event < Dry::Struct
        transform_keys(&:to_sym)

        attribute :id, "integer"
        attribute :secret, "string"
        attribute? :note, Dry::Types["string"].optional
        attribute :status, Dry::Types["string"].default("draft".freeze)
        attribute :address, Address
        attribute :tags, Dry::Types["array"].of(Dry::Types["string"])
        attribute :guests, Dry::Types["array"].of(Address)
        attribute :main_guest, Address.optional
      end
    end
  end

  let(:input) do
    {
      "id" => 1,
      "secret" => "token",
      "status" => "live",
      "address" => {"city" => "NYC", "zipcode" => "10001"},
      "tags" => %w[a b],
      "guests" => [{"city" => "LA", "zipcode" => "90001"}],
      "main_guest" => {"city" => "SF", "zipcode" => "94101"}
    }
  end

  it "builds a new struct class with only the selected attributes" do
    view = Test::Event.project(:id, :status)

    expect(view).to be < Dry::Struct
    expect(view.attribute_names).to eql(%i[id status])

    event = view.new("id" => 1, "status" => "live")
    expect(event.id).to eql(1)
    expect(event.status).to eql("live")
    expect(event).not_to respond_to(:secret)
  end

  it "preserves required, omittable and optional flags" do
    view = Test::Event.project(:id, :note)

    expect(view.schema.key(:id)).to be_required
    expect(view.schema.key(:note)).not_to be_required

    event = view.new(id: 1)
    expect(event.note).to be_nil
    expect(event.to_h).to eql(id: 1)
  end

  it "preserves default values of selected attributes" do
    view = Test::Event.project(:id, :status)

    expect(view.new(id: 1).status).to eql("draft")
  end

  it "does not evaluate defaults of unselected attributes" do
    evaluated = 0

    klass = Class.new(Dry::Struct) do
      attribute :id, Dry::Types["integer"]
      attribute :token, Dry::Types["string"].default { evaluated += 1; "secret" }
    end

    view = klass.project(:id)
    expect(view.new(id: 1).to_h).to eql(id: 1)
    expect(evaluated).to eql(0)
  end

  it "preserves key transformations" do
    view = Test::Event.project(:id, address: [:city])

    event = view.new("id" => 1, "address" => {"city" => "NYC"})
    expect(event.address.city).to eql("NYC")
  end

  it "preserves key transformations of nested structs" do
    view = Test::Event.project(address: [:city, :country])

    event = view.new(address: {"city" => "NYC"})
    expect(event.address.city).to eql("NYC")
    expect(event.address.country).to eql("us")
  end

  it "projects nested structs" do
    view = Test::Event.project(:id, address: [:city])

    event = view.new(input)
    expect(event.address).to be_a(Dry::Struct)
    expect(event.address.to_h).to eql(city: "NYC")
    expect(event.address).not_to respond_to(:zipcode)
  end

  it "projects structs nested inside arrays" do
    view = Test::Event.project(guests: [:city])

    event = view.new(input)
    expect(event.guests.size).to eql(1)
    expect(event.guests[0].to_h).to eql(city: "LA")
    expect(event.guests[0]).not_to respond_to(:zipcode)
  end

  it "projects optional nested structs" do
    view = Test::Event.project(main_guest: [:city])

    event = view.new(main_guest: {"city" => "SF"})
    expect(event.main_guest.to_h).to eql(city: "SF")

    expect(view.new(main_guest: nil).main_guest).to be_nil
  end

  it "supports deeply nested selections" do
    klass = Class.new(Dry::Struct) do
      attribute :wrapper, Test::Event
    end

    view = klass.project(wrapper: [:id, address: [:city]])

    event = view.new(wrapper: input)
    expect(event.wrapper.id).to eql(1)
    expect(event.wrapper.address.to_h).to eql(city: "NYC")
    expect(event.wrapper).not_to respond_to(:secret)
  end

  it "enforces type constraints on the projected attributes" do
    view = Test::Event.project(:id)

    expect { view.new(id: "one") }.to raise_error(Dry::Struct::Error)
  end

  it "points out unknown attributes" do
    expect { Test::Event.project(:id, :nope) }.to raise_error(
      Dry::Struct::UnknownAttributeError,
      /Unknown attribute :nope at "nope".*:secret/
    )
  end

  it "points out unknown nested attributes with their full path" do
    expect { Test::Event.project(address: [:nope]) }.to raise_error(
      Dry::Struct::UnknownAttributeError,
      /Unknown attribute :nope at "address\.nope".*:zipcode/
    )
  end

  it "points out unknown attributes inside struct arrays" do
    expect { Test::Event.project(guests: [:nope]) }.to raise_error(
      Dry::Struct::UnknownAttributeError,
      /Unknown attribute :nope at "guests\.nope"/
    )
  end

  it "rejects empty nested selections" do
    expect { Test::Event.project(address: []) }.to raise_error(
      Dry::Struct::EmptyProjectionError,
      /Empty projection at "address".*select :address as a whole/
    )
  end

  it "rejects projecting into non-struct attributes" do
    expect { Test::Event.project(tags: [:upcase]) }.to raise_error(
      Dry::Struct::UnsupportedProjectionError,
      /Cannot project into "tags"/
    )
  end

  it "does not modify the source class" do
    schema_before = Test::Event.schema

    Test::Event.project(:id, address: [:city])

    expect(Test::Event.schema).to be(schema_before)
    expect(Test::Event.attribute_names).to eql(
      %i[id secret note status address tags guests main_guest]
    )

    event = Test::Event.new(input)
    expect(event.secret).to eql("token")
    expect(event.address.zipcode).to eql("10001")
  end

  context "with two views of the same source class" do
    let(:public_view) { Test::Event.project(:id, address: [:city]) }
    let(:admin_view) { Test::Event.project(:id, :secret, guests: [:zipcode]) }

    it "keeps attribute sets independent" do
      expect(public_view.attribute_names).to eql(%i[id address])
      expect(admin_view.attribute_names).to eql(%i[id secret guests])
      expect(Test::Event.attribute_names).to eql(
        %i[id secret note status address tags guests main_guest]
      )
    end

    it "keeps nested views independent" do
      public_event = public_view.new(input)
      admin_event = admin_view.new(input)

      expect(public_event.address.to_h).to eql(city: "NYC")
      expect(admin_event.guests[0].to_h).to eql(zipcode: "90001")
    end

    it "keeps inheritance independent" do
      expect(public_view.superclass).to be(Dry::Struct)
      expect(admin_view.superclass).to be(Dry::Struct)

      public_event = public_view.new(input)
      expect(public_event).not_to be_a(admin_view)
      expect(public_event).not_to be_a(Test::Event)
      expect(admin_view.new(input)).not_to be_a(public_view)
    end

    it "enforces constraints per view without cross-contamination" do
      expect { admin_view.new(id: "one", secret: "x", guests: []) }
        .to raise_error(Dry::Struct::Error)
      expect { public_view.new(id: 1, address: {city: "NYC"}) }.not_to raise_error

      expect(admin_view.new(id: 1, secret: "s", guests: []).secret).to eql("s")
      expect(public_view.new(input)).not_to respond_to(:secret)
    end
  end
end
