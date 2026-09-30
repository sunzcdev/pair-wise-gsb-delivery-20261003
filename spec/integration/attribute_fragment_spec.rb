# frozen_string_literal: true

RSpec.describe "Dry::Struct.attribute_fragment" do
  def build_fragment(name, &block)
    Dry::Struct.attribute_fragment(name, &block)
  end

  describe "defining fragments" do
    it "collects attributes without instantiating a struct" do
      audit = build_fragment(:audit) do
        attribute :created_at, "string"
        attribute? :updated_at, "string"
      end

      expect(audit).to be_a(Dry::Struct::AttributeFragment)
      expect(audit.entries.map(&:chomped_name)).to eql(%i[created_at updated_at])
    end

    it "rejects duplicate attributes inside one fragment" do
      expect {
        build_fragment(:dup) do
          attribute :name, "string"
          attribute :name, "integer"
        end
      }.to raise_error(Dry::Struct::RepeatedAttributeError, /:name/)
    end
  end

  describe "importing fragments" do
    before do
      module Test
        Audit = Dry::Struct.attribute_fragment(:audit) do
          attribute :created_at, "string"
          attribute? :updated_at, "string"
        end
      end
    end

    it "imports every attribute in order" do
      module Test
        class Event < Dry::Struct
          attribute :name, "string"
          attributes_from Audit
        end
      end

      expect(Test::Event.attribute_names).to eql(
        %i[name created_at updated_at]
      )
    end

    it "keeps required and omittable attributes as declared" do
      module Test
        class StrictEvent < Dry::Struct
          attributes_from Audit
        end
      end

      expect(Test::StrictEvent.schema.key(:created_at)).to be_required
      expect(Test::StrictEvent.schema.key(:updated_at)).to_not be_required

      expect {
        Test::StrictEvent.new(updated_at: "now")
      }.to raise_error(Dry::Struct::Error)

      event = Test::StrictEvent.new(created_at: "now")
      expect(event.updated_at).to be_nil
    end

    it "does not merge a required and an omittable attribute of distinct origins" do
      module Test
        RequiredNote = Dry::Struct.attribute_fragment(:required_note) do
          attribute :note, "string"
        end

        OptionalNote = Dry::Struct.attribute_fragment(:optional_note) do
          attribute? :note, "string"
        end
      end

      expect {
        Class.new(Dry::Struct) do
          attributes_from Test::RequiredNote
          attributes_from Test::OptionalNote
        end
      }.to raise_error(Dry::Struct::AttributeConflictError, /:note/)
    end

    it "selects a subset with only:" do
      module Test
        class CreatedEvent < Dry::Struct
          attributes_from Audit, only: [:created_at]
        end
      end

      expect(Test::CreatedEvent.attribute_names).to eql([:created_at])
    end

    it "excludes attributes with except:" do
      module Test
        class PartialEvent < Dry::Struct
          attributes_from Audit, except: [:updated_at]
        end
      end

      expect(Test::PartialEvent.attribute_names).to eql([:created_at])
    end

    it "renames attributes while keeping requiredness" do
      module Test
        class RenamedEvent < Dry::Struct
          attributes_from(
            Audit,
            rename: {created_at: :at, updated_at: :changed_at}
          )
        end
      end

      expect(Test::RenamedEvent.attribute_names).to eql(%i[at changed_at])
      expect(Test::RenamedEvent.schema.key(:at)).to be_required
      expect(Test::RenamedEvent.schema.key(:changed_at)).to_not be_required
    end

    it "rejects unknown selection names" do
      audit = Test::Audit

      expect {
        Class.new(Dry::Struct) { attributes_from audit, only: [:nope] }
      }.to raise_error(ArgumentError, /unknown attribute :nope/)
    end

    it "selects and renames whole namespaces of composed fragments" do
      module Test
        Geo = Dry::Struct.attribute_fragment(:geo) do
          attribute :city, "string"
        end

        Envelope = Dry::Struct.attribute_fragment(:envelope) do
          attributes_from Geo, namespace: :address
        end

        class EnvelopeEvent < Dry::Struct
          attributes_from Envelope, rename: {address: :where}
        end
      end

      expect(Test::EnvelopeEvent.attribute_names).to eql([:where])
      expect(Test::EnvelopeEvent.schema.key(:where).attribute_names).to eql([:city])
      expect(
        Test::EnvelopeEvent.schema.key(:where).attribute_origins[:city].source
      ).to equal(Test::Geo)
    end

    it "nests imported attributes under a namespace" do
      module Test
        Address = Dry::Struct.attribute_fragment(:address) do
          attribute :city, "string"
          attribute :zipcode, "coercible.string"
        end

        class User < Dry::Struct
          attribute :name, "string"
          attributes_from Address, namespace: :address
        end
      end

      user = Test::User.new(
        name: "Ana",
        address: {city: "NYC", zipcode: 123}
      )

      expect(user.address.city).to eql("NYC")
      expect(user.address.zipcode).to eql("123")
      expect(Test::User.attribute_names).to eql(%i[name address])
      expect(Test::User.schema.key(:address).type).to be < Dry::Struct
    end

    it "merges into an existing nested namespace" do
      module Test
        CityFragment = Dry::Struct.attribute_fragment(:city_fragment) do
          attribute :city, "string"
        end

        ZipFragment = Dry::Struct.attribute_fragment(:zip_fragment) do
          attribute :zipcode, "coercible.string"
        end

        class MergedUser < Dry::Struct
          attribute :address do
            attribute :street, "string"
          end

          attributes_from CityFragment, namespace: :address
          attributes_from ZipFragment, namespace: :address
        end
      end

      expect(Test::MergedUser.schema.key(:address).attribute_names).to eql(
        %i[street city zipcode]
      )
    end

    it "works inside a nested attribute block" do
      module Test
        class NestedUser < Dry::Struct
          attribute :address do
            attribute :street, "string"
            attributes_from Audit
          end
        end
      end

      expect(Test::NestedUser.schema.key(:address).attribute_names).to eql(
        %i[street created_at updated_at]
      )
    end
  end

  describe "origin identity" do
    it "records the originating fragment for each attribute" do
      module Test
        Audit = Dry::Struct.attribute_fragment(:audit) do
          attribute :created_at, "string"
        end

        class Event < Dry::Struct
          attribute :name, "string"
          attributes_from Audit
        end
      end

      origins = Test::Event.attribute_origins

      expect(origins[:created_at].source).to equal(Test::Audit)
      expect(origins[:created_at].original_name).to be(:created_at)
      expect(origins[:name].source).to be(Test::Event)
    end

    it "preserves origin when attributes are renamed or namespaced" do
      module Test
        Audit = Dry::Struct.attribute_fragment(:audit) do
          attribute :created_at, "string"
        end

        class Event < Dry::Struct
          attributes_from Audit, rename: {created_at: :at}
        end
      end

      expect(Test::Event.attribute_origins[:at].original_name).to be(:created_at)
    end
  end

  describe "diamond reuse" do
    it "counts a shared original definition reached via two paths once" do
      module Test
        Timestamps = Dry::Struct.attribute_fragment(:timestamps) do
          attribute :at, "string"
        end

        Creation = Dry::Struct.attribute_fragment(:creation) do
          attributes_from Timestamps
        end

        Update = Dry::Struct.attribute_fragment(:update) do
          attributes_from Timestamps
        end

        class Event < Dry::Struct
          attributes_from Creation
          attributes_from Update
        end
      end

      expect(Test::Event.attribute_names).to eql([:at])
      expect(Test::Event.attribute_origins[:at].source).to equal(Test::Timestamps)
    end

    it "deduplicates the same definition imported twice with different handling" do
      module Test
        Audit = Dry::Struct.attribute_fragment(:audit) do
          attribute :created_at, "string"
        end

        class Event < Dry::Struct
          attributes_from Audit
          attributes_from Audit, only: [:created_at]
        end
      end

      expect(Test::Event.attribute_names).to eql([:created_at])
    end

    it "deduplicates the same original across struct and fragment paths" do
      module Test
        class Base < Dry::Struct
          attribute :at, "string"
        end

        Copy = Dry::Struct.attribute_fragment(:copy) do
          attributes_from Base
        end

        class Event < Dry::Struct
          attributes_from Base
          attributes_from Copy
        end
      end

      expect(Test::Event.attribute_names).to eql([:at])
      expect(Test::Event.attribute_origins[:at].source).to be(Test::Base)
    end
  end

  describe "real conflicts" do
    let(:identity_fragment) do
      build_fragment(:identity) { attribute :name, "string" }
    end

    let(:billing_fragment) do
      build_fragment(:billing) { attribute :name, "string" }
    end

    it "requires explicit handling for same-named attributes of distinct origin" do
      identity = identity_fragment
      billing = billing_fragment

      expect {
        Class.new(Dry::Struct) do
          attributes_from identity
          attributes_from billing
        end
      }.to raise_error(Dry::Struct::AttributeConflictError) do |error|
        message = error.message
        expect(message).to include("Conflicting definitions of attribute :name")
        expect(message).to include("from identity")
        expect(message).to include("from billing")
        expect(message).to include(__FILE__)
      end
    end

    it "conflicts with a locally defined attribute of the same name" do
      identity = identity_fragment

      expect {
        Class.new(Dry::Struct) do
          attribute :name, "string"
          attributes_from identity
        end
      }.to raise_error(Dry::Struct::AttributeConflictError, /:name/)
    end

    it "allows resolving the collision by renaming" do
      identity = identity_fragment
      billing = billing_fragment

      klass = Class.new(Dry::Struct) do
        attributes_from identity
        attributes_from billing, rename: {name: :billing_name}
      end

      expect(klass.attribute_names).to eql(%i[name billing_name])
    end

    it "allows resolving the collision with namespaces" do
      identity = identity_fragment
      billing = billing_fragment

      klass = Class.new(Dry::Struct) do
        attributes_from identity
        attributes_from billing, namespace: :billing
      end

      expect(klass.attribute_names).to eql(%i[name billing])
    end

    it "allows resolving the collision by selecting one definition" do
      identity = identity_fragment
      billing = billing_fragment

      klass = Class.new(Dry::Struct) do
        attributes_from identity
        attributes_from billing, only: []
      end

      expect(klass.attribute_names).to eql([:name])
    end

    it "reports a conflict between fragments composed into one fragment" do
      left = identity_fragment
      right = billing_fragment

      expect {
        build_fragment(:both) do
          attributes_from left
          attributes_from right
        end
      }.to raise_error(Dry::Struct::AttributeConflictError, /:name/)
    end

    it "points at both definitions inside a nested namespace" do
      module Test
        Left = Dry::Struct.attribute_fragment(:left) do
          attribute :city, "string"
        end

        Right = Dry::Struct.attribute_fragment(:right) do
          attribute :city, "string"
        end
      end

      expect {
        Class.new(Dry::Struct) do
          attributes_from Test::Left, namespace: :address
          attributes_from Test::Right, namespace: :address
        end
      }.to raise_error(Dry::Struct::AttributeConflictError) do |error|
        expect(error.message).to include("attribute :city")
        expect(error.message).to include("from left")
        expect(error.message).to include("from right")
      end
    end
  end

  describe "inheritance" do
    it "does not relax requiredness of imported attributes" do
      module Test
        Strict = Dry::Struct.attribute_fragment(:strict) do
          attribute :id, "integer"
        end

        class Parent < Dry::Struct
          attributes_from Strict
        end

        class Child < Parent
        end
      end

      expect(Test::Child.schema.key(:id)).to be_required
      expect { Test::Child.new({}) }.to raise_error(Dry::Struct::Error)
    end

    it "propagates later parent imports to subclasses without an own origin" do
      module Test
        Audit = Dry::Struct.attribute_fragment(:audit) do
          attribute :created_at, "string"
        end

        class Parent < Dry::Struct
        end

        class Child < Parent
        end

        Parent.attributes_from(Audit)
      end

      expect(Test::Child.attribute_names).to eql([:created_at])
      expect(Test::Child.attribute_origins[:created_at].source).to equal(Test::Audit)
    end

    it "keeps a subclass-fixed origin when the parent later imports another one" do
      module Test
        Local = Dry::Struct.attribute_fragment(:local) do
          attribute :name, "string"
        end

        Other = Dry::Struct.attribute_fragment(:other) do
          attribute :name, "string"
        end

        class Parent < Dry::Struct
        end

        class Child < Parent
          attributes_from Local
        end

        Parent.attributes_from(Other)
      end

      expect(Test::Child.attribute_origins[:name].source).to equal(Test::Local)
      expect(Test::Parent.attribute_origins[:name].source).to equal(Test::Other)
    end

    it "propagates origins through nested inheritance for diamonds" do
      module Test
        Shared = Dry::Struct.attribute_fragment(:shared) do
          attribute :at, "string"
        end

        class Parent < Dry::Struct
          attributes_from Shared
        end

        class Child < Parent
        end

        class GrandChild < Child
          attributes_from Shared
        end
      end

      expect(Test::GrandChild.attribute_names).to eql([:at])
      expect(Test::GrandChild.attribute_origins[:at].source).to equal(Test::Shared)
    end
  end
end
