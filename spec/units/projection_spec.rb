# frozen_string_literal: true

require 'blueprinter/projection'

describe Blueprinter::Projection do
  let(:owner_blueprint) do
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name
      field :email
    end
  end

  let(:vehicle_blueprint) do
    owner_bp = owner_blueprint
    Class.new(Blueprinter::Base) do
      identifier :id
      field :make
      field :model
      association :owner, blueprint: owner_bp

      view :compact do
        field :make
        exclude :model
      end
    end
  end

  describe '.build' do
    it 'returns an unprojected projection when no spec is given' do
      projection = described_class.build

      expect(projection.projected?).to be(false)
      expect(projection.selected?(:anything)).to be(true)
    end

    it 'accepts an array of output field names (symbols and strings)' do
      projection = described_class.build([:id, 'make'])

      expect(projection.projected?).to be(true)
      expect(projection.selected?(:id)).to be(true)
      expect(projection.selected?(:make)).to be(true)
      expect(projection.selected?(:model)).to be(false)
    end

    it 'accepts a single field name' do
      expect(described_class.build(:id).selected?(:id)).to be(true)
      expect(described_class.build('id').selected?(:id)).to be(true)
    end

    it 'accepts a hash with nested association specs' do
      projection = described_class.build([:id, { owner: [:name] }])

      expect(projection.node_for(:owner).nested).to be(true)
      expect(projection.child_for(:owner).selected?(:name)).to be(true)
      expect(projection.child_for(:owner).selected?(:email)).to be(false)
    end

    it 'supports a hash-shaped root spec' do
      projection = described_class.build({ id: true, owner: [:name] })

      expect(projection.selected?(:id)).to be(true)
      expect(projection.node_for(:owner).nested).to be(true)
    end

    it 'defaults include_identifier to true and cascades it to children' do
      projection = described_class.build(include_identifier: false)
      expect(projection.include_identifier).to be(false)

      nested = described_class.build([:id, { owner: [:name] }], include_identifier: false)
      expect(nested.include_identifier).to be(false)
      expect(nested.child_for(:owner).include_identifier).to be(false)
    end

    it 'rejects unsupported spec values' do
      expect { described_class.build(5) }.to raise_error(ArgumentError, /Invalid field projection/)
    end

    it 'rejects false as a nested value' do
      expect { described_class.build(owner: false) }.to raise_error(ArgumentError)
    end

    it 'rejects invalid nested values' do
      expect { described_class.build(owner: 5) }.to raise_error(ArgumentError)
    end
  end

  describe '#validate!' do
    it 'is a no-op for unprojected projections' do
      expect { described_class.build.validate!(vehicle_blueprint, :default) }.not_to raise_error
    end

    it 'accepts fields present in the selected view' do
      projection = described_class.build(%i[id make owner])

      expect { projection.validate!(vehicle_blueprint, :default) }.not_to raise_error
    end

    it 'raises an InvalidProjection for an unknown field with its path' do
      projection = described_class.build(%i[id nope])

      expect { projection.validate!(vehicle_blueprint, :default) }.to raise_error(
        Blueprinter::Errors::InvalidProjection,
        "Unknown field 'nope' for '#{vehicle_blueprint}'"
      )
    end

    it 'does not reopen a field excluded from the selected view' do
      projection = described_class.build(%i[id model])

      expect { projection.validate!(vehicle_blueprint, :compact) }.to raise_error(
        Blueprinter::Errors::InvalidProjection,
        "Field 'model' is excluded from the 'compact' view"
      )
    end

    it 'is rescuable as a BlueprinterError' do
      projection = described_class.build([:nope])

      expect { projection.validate!(vehicle_blueprint, :default) }.to raise_error(Blueprinter::BlueprinterError)
    end

    it 'validates nested association fields with a full path' do
      projection = described_class.build([:id, { owner: %i[id nope] }])

      expect { projection.validate!(vehicle_blueprint, :default) }.to raise_error(
        Blueprinter::Errors::InvalidProjection,
        "Unknown field 'owner.nope' for '#{owner_blueprint}'"
      )
    end

    it 'rejects a nested projection on a scalar field' do
      projection = described_class.build({ id: true, make: [:detail] })

      expect { projection.validate!(vehicle_blueprint, :default) }.to raise_error(
        Blueprinter::Errors::InvalidProjection,
        "Field 'make' is not an association and cannot have a nested projection"
      )
    end

    it 'uses the association view when validating nested fields' do
      custom_owner = Class.new(Blueprinter::Base) do
        identifier :id
        field :name

        view :name_only do
          field :name
        end
      end

      custom_vehicle = Class.new(Blueprinter::Base) do
        identifier :id
        association :owner, blueprint: custom_owner, view: :name_only
      end

      valid = described_class.build({ owner: [:id] })
      invalid = described_class.build({ owner: [:email] })

      expect { valid.validate!(custom_vehicle, :default) }.not_to raise_error
      expect { invalid.validate!(custom_vehicle, :default) }.to raise_error(
        Blueprinter::Errors::InvalidProjection,
        /Unknown field 'owner.email'/
      )
    end

    it 'skips static recursion for dynamic (Proc) blueprints' do
      dynamic = Class.new(Blueprinter::Base) do
        identifier :id
        association :owner, blueprint: ->(value) { owner_blueprint }
      end

      projection = described_class.build({ owner: [:nope] })

      expect { projection.validate!(dynamic, :default) }.not_to raise_error
    end
  end
end

