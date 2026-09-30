# frozen_string_literal: true

RSpec.describe "Dry::Struct.project" do
  # Internal, field-rich event. External views expose only a few of its fields,
  # including nested structs and arrays of nested structs.
  before do
    module Test
      module Types
        include Dry.Types()
      end

      class Address < Dry::Struct
        transform_keys(&:to_sym)

        attribute :city, Types::String
        attribute :zipcode, Types::String
        attribute :country, Types::String
        attribute? :region, Types::String
      end

      class Organizer < Dry::Struct
        transform_keys(&:to_sym)

        attribute :name, Types::String
        attribute :email, Types::String
      end

      class Event < Dry::Struct
        transform_keys(&:to_sym)

        attribute :id, Types::Integer
        attribute :title, Types::String
        attribute? :internal_note, Types::String
        attribute :tags, Types::Array.of(Types::String).default([].freeze)
        attribute :venue, Address
        attribute :organizers, Types::Array.of(Organizer)
        attribute? :backup_venue, Address.optional
      end
    end
  end

  let(:input) do
    {
      "id" => 1,
      "title" => "Ruby Meetup",
      "internal_note" => "do not publish",
      "tags" => %w[ruby oss],
      "venue" => {"city" => "Shanghai", "zipcode" => "200000", "country" => "CN"},
      "organizers" => [
        {"name" => "Matz", "email" => "matz@example.com"},
        {"name" => "Koichi", "email" => "ko1@example.com"}
      ],
      "backup_venue" => {"city" => "Beijing", "zipcode" => "100000", "country" => "CN"}
    }
  end

  def build_view(&block)
    Test::Event.project(&block)
  end

  describe "the generated view" do
    let(:view) do
      build_view do
        attribute :id
        attribute :title
        attribute :tags
        attribute :venue do
          attribute :city
          attribute :country
        end
        attribute :organizers do
          attribute :name
        end
        attribute :backup_venue do
          attribute :city
        end
      end
    end

    subject(:event) { view.new(input) }

    it "exposes only the selected top-level attributes" do
      expect(view.attribute_names).to eql(%i[id title tags venue organizers backup_venue])
      expect(view.has_attribute?(:internal_note)).to be(false)
    end

    it "builds instances without to_h/key deletion" do
      expect(event).to be_a(Dry::Struct)
      expect(event.id).to be(1)
      expect(event.title).to eql("Ruby Meetup")
      expect(event.tags).to eql(%w[ruby oss])
    end

    it "projects nested structs to only the chosen fields" do
      expect(event.venue).to be_a(Dry::Struct)
      expect(event.venue.city).to eql("Shanghai")
      expect(event.venue.country).to eql("CN")
      expect(event.venue).not_to respond_to(:zipcode)
      expect(event.venue).not_to respond_to(:region)
    end

    it "projects structs inside arrays element-wise" do
      expect(event.organizers).to all(be_a(Dry::Struct))
      expect(event.organizers.map(&:name)).to eql(%w[Matz Koichi])
      expect(event.organizers.first).not_to respond_to(:email)
    end

    it "projects optional nested structs" do
      expect(event.backup_venue.city).to eql("Beijing")
      expect(event.backup_venue).not_to respond_to(:country)
      expect(view.new(input.except("backup_venue")).backup_venue).to be_nil
    end

    it "projects a struct hidden behind a custom constructor" do
      nested =
        Class.new(Dry::Struct) do
          attribute :name, Dry::Types["strict.string"]
          attribute :token, Dry::Types["strict.string"]
        end

      wrapped_type = nested.constructor(->(value, type) { type.call(value) })
      container =
        Class.new(Dry::Struct) do
          attribute :item, wrapped_type
        end

      item_view = container.project(item: {name: true})
      item = item_view.new(item: {name: "n", token: "t"}).item

      expect(item.name).to eql("n")
      expect(item).not_to respond_to(:token)
    end

    it "keeps the key transformation rules (string keys accepted)" do
      expect { event }.not_to raise_error
    end

    it "enforces required attributes of the view" do
      expect { view.new(input.except("id")) }.to raise_error(Dry::Struct::Error, /:id is missing/)
      expect {
        view.new(input.merge("venue" => {"city" => "X"}))
      }.to raise_error(Dry::Struct::Error, /:country is missing/)
    end

    it "preserves omittable attributes as omittable" do
      expect(view.schema.key(:backup_venue).required?).to be(false)
      expect(view.schema.key(:id).required?).to be(true)
    end

    it "preserves default values but does not evaluate them eagerly" do
      expect(view.new(input.except("tags")).tags).to eql([])
    end
  end

  describe "two views of the same source class" do
    let(:public_view) do
      build_view do
        attribute :id
        attribute :title
        attribute :venue do
          attribute :city
        end
        attribute :organizers do
          attribute :name
        end
      end
    end

    let(:admin_view) do
      Test::Event.project do
        attribute :id
        attribute :internal_note
        attribute :venue do
          attribute :city
          attribute :zipcode
          attribute :region
        end
      end
    end

    it "are different, independent types" do
      expect(public_view).not_to eql(admin_view)
      expect(public_view.attribute_names).to eql(%i[id title venue organizers])
      expect(admin_view.attribute_names).to eql(%i[id internal_note venue])
    end

    it "build nested types that don't pollute each other" do
      public_venue = public_view.schema.key(:venue).type
      admin_venue = admin_view.schema.key(:venue).type

      expect(public_venue).not_to eql(admin_venue)
      expect(public_venue.attribute_names).to eql(%i[city])
      expect(admin_venue.attribute_names).to eql(%i[city zipcode region])
    end

    it "keep their own constraints when instantiated" do
      public_event = public_view.new(input)
      admin_event = admin_view.new(input)

      expect(public_event).to respond_to(:title)
      expect(public_event).not_to respond_to(:internal_note)
      expect(public_event.venue).not_to respond_to(:zipcode)

      expect(admin_event).not_to respond_to(:title)
      expect(admin_event.internal_note).to eql("do not publish")
      expect(admin_event.venue).to respond_to(:zipcode)
      expect(admin_event.venue.city).to eql("Shanghai")
    end

    it "do not inherit from the concrete source struct" do
      expect(public_view.ancestors).not_to include(Test::Event)
      expect(admin_view.ancestors).not_to include(Test::Event)
      expect(public_view < Dry::Struct).to be(true)
      expect(admin_view < Dry::Struct).to be(true)
    end

    it "leave the source class and nested types untouched" do
      public_view
      admin_view

      expect(Test::Event.attribute_names).to eql(
        %i[id title internal_note tags venue organizers backup_venue]
      )
      expect(Test::Address.attribute_names).to eql(%i[city zipcode country region])
      expect(Test::Organizer.attribute_names).to eql(%i[name email])
      expect(Test::Event.schema.key(:venue).type).to be(Test::Address)
    end
  end

  describe "default values" do
    it "does not evaluate defaults of unselected attributes" do
      evaluated = 0
      stub_event =
        Class.new(Dry::Struct) do
          attribute :id, Dry::Types["strict.integer"]
          default_proc = -> { evaluated += 1 }
          attribute :computed, Dry::Types["strict.integer"].default(default_proc.freeze)
        end

      view = stub_event.project(id: true)
      view.new(id: 1)

      expect(evaluated).to be(0)
    end

    it "coerces a selected nested default through the projected type" do
      address =
        Class.new(Dry::Struct) do
          attribute :city, Dry::Types["strict.string"]
          attribute :secret, Dry::Types["strict.string"]
        end

      event =
        Class.new(Dry::Struct) do
          attribute :id, Dry::Types["strict.integer"]
          attribute :venue, address.default(address.new(city: "X", secret: "S").freeze)
        end

      view = event.project(id: true, venue: {city: true})
      venue = view.new(id: 1).venue

      expect(venue.city).to eql("X")
      expect(venue).not_to respond_to(:secret)
    end

    it "coerces a selected array-of-struct default through the projected type" do
      address =
        Class.new(Dry::Struct) do
          attribute :city, Dry::Types["strict.string"]
          attribute :secret, Dry::Types["strict.string"]
        end

      event =
        Class.new(Dry::Struct) do
          attribute :id, Dry::Types["strict.integer"]
          attribute :venues, Dry::Types["strict.array"].of(address).default(
            [address.new(city: "X", secret: "S").freeze].freeze
          )
        end

      view = event.project(id: true, venues: {city: true})
      venues = view.new(id: 1).venues

      expect(venues.map(&:city)).to eql(%w[X])
      expect(venues.first).not_to respond_to(:secret)
    end
  end

  describe "hash form" do
    it "accepts a nested hash selection" do
      view = Test::Event.project(
        id: true,
        venue: {city: true},
        organizers: {name: true}
      )

      event = view.new(input)
      expect(event.venue.city).to eql("Shanghai")
      expect(event.organizers.map(&:name)).to eql(%w[Matz Koichi])
    end

    it "accepts string keys" do
      view = Test::Event.project("id" => true, "venue" => {"city" => true})
      expect(view.attribute_names).to eql(%i[id venue])
    end

    it "can be projected again, preserving keys and required status" do
      first = Test::Event.project(id: true, title: true, venue: {city: true, country: true})
      second = first.project(id: true, venue: {city: true})

      expect(second.attribute_names).to eql(%i[id venue])
      expect(second.schema.type.key(:id).required?).to be(true)

      event = second.new("id" => 1, "venue" => {"city" => "Shanghai", "country" => "CN"})
      expect(event.id).to be(1)
      expect(event.venue.city).to eql("Shanghai")
      expect(event.venue).not_to respond_to(:country)
    end
  end

  describe "invalid selections" do
    it "points at an unknown top-level path" do
      expect { Test::Event.project { attribute :nope } }
        .to raise_error(
          Dry::Struct::UnknownProjectionPath,
          %r{:nope is not an attribute of Test::Event}
        )
    end

    it "points at an unknown nested path with the full path" do
      expect {
        Test::Event.project { attribute :venue do; attribute :nope; end }
      }.to raise_error(
        Dry::Struct::UnknownProjectionPath,
        %r{:venue -> :nope.*not an attribute of Test::Address}
      )
    end

    it "points at an unknown path inside an array of structs" do
      expect {
        Test::Event.project { attribute :organizers do; attribute :nope; end }
      }.to raise_error(Dry::Struct::UnknownProjectionPath, /:organizers -> :nope/)
    end

    it "refuses to descend into a non-struct attribute" do
      expect {
        Test::Event.project { attribute :id do; attribute :x; end }
      }.to raise_error(Dry::Struct::InvalidProjection, /neither a struct nor an array of structs/)
    end

    it "refuses to descend into an array of plain values" do
      expect {
        Test::Event.project { attribute :tags do; attribute :x; end }
      }.to raise_error(Dry::Struct::InvalidProjection, /neither a struct nor an array of structs/)
    end

    it "refuses an empty root projection" do
      expect { Test::Event.project {} }
        .to raise_error(Dry::Struct::InvalidProjection, /empty projection of Test::Event/)
    end

    it "refuses a nested projection with no selected fields" do
      expect {
        Test::Event.project { attribute :id; attribute :venue do; end }
      }.to raise_error(Dry::Struct::InvalidProjection, /empty projection of Test::Address/)
    end
  end

  describe "Dry::Struct::Value", :suppress_deprecations do
    it "produces frozen value views" do
      value_type =
        Class.new(Dry::Struct::Value) do
          attribute :name, Dry::Types["strict.string"]
          attribute :age, Dry::Types["strict.integer"]
        end

      view = value_type.project(name: true)
      value = view.new(name: "Matz", age: 52)

      expect(value).to be_a(Dry::Struct::Value)
      expect(value).to be_frozen
      expect(value.name).to eql("Matz")
    end
  end
end
