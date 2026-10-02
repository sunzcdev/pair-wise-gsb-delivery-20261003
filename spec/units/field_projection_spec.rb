# frozen_string_literal: true

require 'blueprinter/extractor'
require 'blueprinter/field_projection'
require 'blueprinter/field'
require 'blueprinter/association'

describe Blueprinter::FieldProjection do
  let(:extractor) do
    Class.new(Blueprinter::Extractor) do
      def extract(*); end
    end
  end

  def field(name, options = {})
    Blueprinter::Field.new(name, name, extractor, parent_blueprint, options)
  end

  def association(name, blueprint: child_blueprint, view: :default)
    Blueprinter::Association.new(
      method: name,
      name: name,
      blueprint: blueprint,
      parent_blueprint: parent_blueprint,
      view: view
    )
  end

  let(:parent_blueprint) { Class.new(Blueprinter::Base) }

  let(:child_blueprint) do
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name
      field :email
    end
  end

  describe '.build' do
    it 'returns nil for nil' do
      expect(described_class.build(nil)).to be_nil
    end

    it 'returns the projection unchanged' do
      projection = described_class.new(:id)
      expect(described_class.build(projection)).to be(projection)
    end
  end

  describe '#initialize' do
    it 'accepts a single symbol' do
      projection = described_class.new(:id)
      expect(projection.includes?(:id)).to be(true)
    end

    it 'accepts an array of names' do
      projection = described_class.new(%i[id name])
      expect(projection.includes?(:id)).to be(true)
      expect(projection.includes?(:name)).to be(true)
      expect(projection.includes?(:email)).to be(false)
    end

    it 'accepts a hash with truthy values' do
      projection = described_class.new(id: true, name: true)
      expect(projection.includes?(:id)).to be(true)
      expect(projection.includes?(:name)).to be(true)
    end

    it 'treats a false value as an explicit exclusion' do
      projection = described_class.new(id: false, name: true)
      expect(projection.excluded?(:id)).to be(true)
      expect(projection.includes?(:name)).to be(true)
    end

    it 'builds nested projections from arrays and hashes' do
      projection = described_class.new(owner: [:name])
      expect(projection.nested(:owner)).to be_a(described_class)
      expect(projection.nested(:owner).includes?(:name)).to be(true)

      projection = described_class.new(owner: { name: true })
      expect(projection.nested(:owner).includes?(:name)).to be(true)
    end

    it 'honors the identifier toggle at a nested level' do
      projection = described_class.new(owner: { name: true, identifier: false })
      expect(projection.nested(:owner).include_identifiers).to be(false)
    end

    it 'treats a top-level identifier-only hash as an identifier toggle' do
      projection = described_class.new(identifier: false)
      expect(projection.active?).to be(true)
      expect(projection.include_identifiers).to be(false)
      expect(projection.includes?(:name)).to be(false)
    end

    it 'combines the identifier toggle with regular selections' do
      projection = described_class.new(identifier: false, name: true)
      expect(projection.include_identifiers).to be(false)
      expect(projection.includes?(:name)).to be(true)
    end

    it 'raises ArgumentError for unsupported input' do
      expect { described_class.new(1) }.to raise_error(ArgumentError, /Invalid field projection/)
    end
  end

  describe '#active?' do
    it 'is false when nothing was selected' do
      expect(described_class.new.active?).to be(false)
    end

    it 'is true when fields were selected' do
      expect(described_class.new(:id).active?).to be(true)
    end

    it 'is true when identifiers were explicitly disabled' do
      expect(described_class.new(identifier: false).active?).to be(true)
    end
  end

  describe '#validate!' do
    let(:id_field) { field(:id) }
    let(:name_field) { field(:name) }
    let(:owner_association) { association(:owner) }
    let(:fields) { [id_field, name_field, owner_association] }

    it 'passes when all selected fields exist' do
      projection = described_class.new(%i[id name])
      expect(projection.validate!(fields)).to be(true)
    end

    it 'raises for an unknown top-level field' do
      projection = described_class.new(:missing)
      expect { projection.validate!(fields) }.
        to raise_error(Blueprinter::Errors::InvalidProjection, "Unknown field 'missing'")
    end

    it 'raises for an unknown nested field with a dotted path' do
      projection = described_class.new(owner: [:missing])
      expect { projection.validate!(fields) }.
        to raise_error(Blueprinter::Errors::InvalidProjection, "Unknown field 'owner.missing'")
    end

    it 'raises when nesting onto a plain field' do
      projection = described_class.new(name: [:x])
      expect { projection.validate!(fields) }.
        to raise_error(
          Blueprinter::Errors::InvalidProjection,
          "Cannot project nested fields onto 'name': 'name' is not an association"
        )
    end

    it 'validates recursively through associations' do
      projection = described_class.new(owner: { email: [:deep] })
      expect { projection.validate!(fields) }.
        to raise_error(Blueprinter::Errors::InvalidProjection,
                       "Cannot project nested fields onto 'owner.email': 'email' is not an association")
    end
  end
end
