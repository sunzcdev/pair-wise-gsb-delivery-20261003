# frozen_string_literal: true

RSpec.describe "Dry::Struct::Fragment" do
  it "imports attributes from a fragment" do
    module Test
      module Timestamps
        include Dry::Struct::Fragment

        attribute :created_at, "string"
        attribute? :archived_at, "string"
      end

      class Event < Dry::Struct
        attribute :name, "string"
        attributes_from Timestamps
      end
    end

    event = Test::Event.new(name: "launch", created_at: "today")

    expect(event.created_at).to eql("today")
    expect(event.archived_at).to be_nil
    expect(Test::Event.attribute_names).to eql(%i[name created_at archived_at])
  end

  it "exposes the origin of every attribute" do
    module Test
      module Timestamps
        include Dry::Struct::Fragment

        attribute :created_at, "string"
      end

      class Event < Dry::Struct
        attribute :name, "string"
        attributes_from Timestamps
      end
    end

    expect(Test::Event.attribute_source(:created_at).owner).to be(Test::Timestamps)
    expect(Test::Event.attribute_source(:created_at).name).to be(:created_at)
    expect(Test::Event.attribute_source(:name).owner).to be(Test::Event)
  end

  it "imports a shared definition once through multiple paths (diamond)" do
    module Test
      module Timestamps
        include Dry::Struct::Fragment

        attribute :created_at, "string"
      end

      module Auditable
        include Dry::Struct::Fragment

        attributes_from Timestamps
        attribute :actor, "string"
      end

      module Trackable
        include Dry::Struct::Fragment

        attributes_from Timestamps
        attribute :source_ip, "string"
      end

      class Event < Dry::Struct
        attributes_from Auditable
        attributes_from Trackable
      end
    end

    expect(Test::Event.attribute_names).to eql(%i[created_at actor source_ip])
    expect(Test::Event.attribute_source(:created_at).owner).to be(Test::Timestamps)
    expect(Test::Event.attribute_source(:actor).owner).to be(Test::Auditable)
    expect(Test::Event.attribute_source(:source_ip).owner).to be(Test::Trackable)

    event = Test::Event.new(created_at: "today", actor: "matz", source_ip: "127.0.0.1")
    expect(event.created_at).to eql("today")
  end

  it "points at both definitions when same-named attributes come from different fragments" do
    module Test
      module IdentityA
        include Dry::Struct::Fragment

        attribute :id, "integer"
      end

      module IdentityB
        include Dry::Struct::Fragment

        attribute :id, "integer"
      end
    end

    expect {
      Class.new(Dry::Struct) do
        attributes_from Test::IdentityA
        attributes_from Test::IdentityB
      end
    }.to raise_error(Dry::Struct::AttributeConflictError) { |error|
      expect(error.message).to include("Attribute :id is defined by both")
      expect(error.message).to include("Test::IdentityA#id (defined at #{__FILE__}:")
      expect(error.message).to include("Test::IdentityB#id (defined at #{__FILE__}:")
    }
  end

  it "requires explicit handling even when conflicting types are identical" do
    module Test
      module IdentityA
        include Dry::Struct::Fragment

        attribute :id, "integer"
      end

      module IdentityB
        include Dry::Struct::Fragment

        attribute :id, "integer"
      end

      class Event < Dry::Struct
        attributes_from IdentityA
        attributes_from IdentityB, rename: {id: :external_id}
      end
    end

    expect(Test::Event.attribute_names).to eql(%i[id external_id])
    expect(Test::Event.attribute_source(:id).owner).to be(Test::IdentityA)
    expect(Test::Event.attribute_source(:external_id).owner).to be(Test::IdentityB)
    expect(Test::Event.attribute_source(:external_id).name).to be(:id)
  end

  it "resolves conflicts explicitly with resolve:" do
    module Test
      module IdentityA
        include Dry::Struct::Fragment

        attribute :id, "integer"
      end

      module IdentityB
        include Dry::Struct::Fragment

        attribute :id, "string"
      end

      class Event < Dry::Struct
        attributes_from IdentityA
        attributes_from IdentityB, resolve: {id: IdentityA}
      end
    end

    expect(Test::Event.attribute_source(:id).owner).to be(Test::IdentityA)
    expect(Test::Event.new(id: 1).id).to be(1)
  end

  it "rejects resolutions that do not name one of the conflicting sources" do
    module Test
      module IdentityA
        include Dry::Struct::Fragment

        attribute :id, "integer"
      end

      module IdentityB
        include Dry::Struct::Fragment

        attribute :id, "integer"
      end
    end

    expect {
      Class.new(Dry::Struct) do
        attributes_from Test::IdentityA
        attributes_from Test::IdentityB, resolve: {id: String}
      end
    }.to raise_error(ArgumentError, /cannot resolve attribute :id/)
  end

  it "never relaxes requiredness through a resolution" do
    module Test
      module OptionalId
        include Dry::Struct::Fragment

        attribute? :id, "integer"
      end

      module RequiredId
        include Dry::Struct::Fragment

        attribute :id, "integer"
      end
    end

    expect {
      Class.new(Dry::Struct) do
        attributes_from Test::OptionalId
        attributes_from Test::RequiredId, resolve: {id: Test::OptionalId}
      end
    }.to raise_error(Dry::Struct::AttributeConflictError, /would relax requiredness/)
  end

  it "keeps the stricter requiredness when the resolution allows it" do
    module Test
      module OptionalId
        include Dry::Struct::Fragment

        attribute? :id, "integer"
      end

      module RequiredId
        include Dry::Struct::Fragment

        attribute :id, "integer"
      end

      class Event < Dry::Struct
        attributes_from RequiredId
        attributes_from OptionalId, resolve: {id: RequiredId}
      end
    end

    expect(Test::Event.schema.key(:id)).to be_required
    expect(Test::Event.attribute_source(:id).owner).to be(Test::RequiredId)
  end

  it "does not let a parent import rewrite sources already settled in a subclass" do
    module Test
      module CodeA
        include Dry::Struct::Fragment

        attribute :code, "string"
      end

      module CodeB
        include Dry::Struct::Fragment

        attribute :code, "string"
        attribute :label, "string"
      end

      class Parent < Dry::Struct
      end

      class Child < Parent
        attributes_from CodeA
      end
    end

    Test::Parent.attributes_from(Test::CodeB)

    expect(Test::Parent.attribute_source(:code).owner).to be(Test::CodeB)
    expect(Test::Child.attribute_source(:code).owner).to be(Test::CodeA)
    expect(Test::Child.attribute_source(:label).owner).to be(Test::CodeB)
  end

  it "selects attributes with only: and except:" do
    module Test
      module Address
        include Dry::Struct::Fragment

        attribute :city, "string"
        attribute :country, "string"
        attribute :zipcode, "string"
      end

      class User < Dry::Struct
        attributes_from Address, only: %i[city]
      end

      class Guest < Dry::Struct
        attributes_from Address, except: %i[zipcode]
      end
    end

    expect(Test::User.attribute_names).to eql(%i[city])
    expect(Test::Guest.attribute_names).to eql(%i[city country])
  end

  it "raises on unknown attributes in only:, except: and rename:" do
    module Test
      module Address
        include Dry::Struct::Fragment

        attribute :city, "string"
      end
    end

    expect {
      Class.new(Dry::Struct) { attributes_from Test::Address, only: %i[town] }
    }.to raise_error(ArgumentError, /:town/)

    expect {
      Class.new(Dry::Struct) { attributes_from Test::Address, except: %i[town] }
    }.to raise_error(ArgumentError, /:town/)

    expect {
      Class.new(Dry::Struct) { attributes_from Test::Address, rename: {town: :city} }
    }.to raise_error(ArgumentError, /:town/)
  end

  it "imports into a nested namespace preserving sources" do
    module Test
      module Address
        include Dry::Struct::Fragment

        attribute :city, "string"
        attribute :country, "string"
      end

      class User < Dry::Struct
        attribute :name, "string"
        attributes_from Address, namespace: :home
      end
    end

    user = Test::User.new(name: "Jane", home: {city: "NYC", country: "US"})

    expect(user.home.city).to eql("NYC")
    expect(Test::User.attribute_names).to eql(%i[name home])

    home = Test::User.schema.key(:home).type
    expect(home.attribute_source(:city).owner).to be(Test::Address)
  end

  it "imports from plain structs with provenance" do
    module Test
      class Address < Dry::Struct
        attribute :city, "string"
      end

      class User < Dry::Struct
        attributes_from Address
        attributes_from Address
      end
    end

    expect(Test::User.attribute_names).to eql(%i[city])
    expect(Test::User.attribute_source(:city).owner).to be(Test::Address)
  end

  it "raises a conflict when a struct attribute meets a different fragment definition" do
    module Test
      module Timestamps
        include Dry::Struct::Fragment

        attribute :created_at, "string"
      end
    end

    expect {
      Class.new(Dry::Struct) do
        attribute :created_at, "string"
        attributes_from Test::Timestamps
      end
    }.to raise_error(Dry::Struct::AttributeConflictError, /defined by both/)
  end

  it "raises when a fragment itself combines diverging definitions" do
    module Test
      module IdentityA
        include Dry::Struct::Fragment

        attribute :id, "integer"
      end

      module IdentityB
        include Dry::Struct::Fragment

        attribute :id, "integer"
      end
    end

    expect {
      Module.new do
        include Dry::Struct::Fragment

        attributes_from Test::IdentityA
        attributes_from Test::IdentityB
      end
    }.to raise_error(Dry::Struct::AttributeConflictError, /Test::IdentityA#id.*Test::IdentityB#id/)
  end
end
