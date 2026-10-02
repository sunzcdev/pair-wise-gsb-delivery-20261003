# frozen_string_literal: true

require 'blueprinter/extractor'
require 'blueprinter/extractors/association_extractor'
require 'ostruct'

describe 'Field projection' do
  # Records how many times each reader was invoked so we can prove skipped
  # fields (and skipped associations) are never read.
  class CountingOpenStruct
    attr_reader :call_count

    def initialize(attributes = {})
      @table = attributes
      @call_count = Hash.new(0)
    end

    def [](key)
      @table[key] || @table[key.to_s]
    end

    private

    def method_missing(name, *args)
      @call_count[name] += 1
      @table.fetch(name) { @table.fetch(name.to_s) { super } }
    end

    def respond_to_missing?(name, include_private = false)
      @table.key?(name) || @table.key?(name.to_s) || super
    end
  end

  let(:owner_blueprint) do
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name
      field :email

      view :nameless do
        exclude :name
      end
    end
  end

  let(:blueprint) do
    owner_bp = owner_blueprint
    Class.new(Blueprinter::Base) do
      identifier :id
      field :make
      field :model
      field :secret_name, name: :codename
      association :owner, blueprint: owner_bp

      view :compact do
        field :make
        exclude :model
      end

      view :with_owner do
        include_view :compact
        association :owner, blueprint: owner_bp
      end
    end
  end

  let(:owner) { CountingOpenStruct.new(id: 9, name: 'Meg', email: 'meg@example.com') }
  let(:vehicle) do
    CountingOpenStruct.new(id: 1, make: 'Super Car', model: 'ACME',
                           secret_name: 'Ghost', owner: owner)
  end

  before { reset_blueprinter_config! }
  after { reset_blueprinter_config! }

  describe '.render and .render_as_hash' do
    it 'render the same projected fields' do
      projected_hash = blueprint.render_as_hash(vehicle, fields: [:id, :make])

      expect(projected_hash).to eq(id: 1, make: 'Super Car')
      expect(JSON.parse(blueprint.render(vehicle, fields: [:id, :make]))).to eq(
        'id' => 1,
        'make' => 'Super Car'
      )
    end

    it 'keeps the identifier by default' do
      expect(blueprint.render_as_hash(vehicle, fields: [:make])).to eq(
        id: 1,
        make: 'Super Car'
      )
    end

    it 'projects out the identifier with identifier: false' do
      expect(blueprint.render_as_hash(vehicle, fields: [:make], identifier: false)).to eq(
        make: 'Super Car'
      )
    end

    it 'keeps an explicitly selected identifier even with identifier: false' do
      expect(blueprint.render_as_hash(vehicle, fields: %i[id make], identifier: false)).to eq(
        id: 1,
        make: 'Super Car'
      )
    end

    it 'addresses renamed fields by their output name' do
      expect(blueprint.render_as_hash(vehicle, fields: [:codename])).to eq(
        id: 1,
        codename: 'Ghost'
      )
    end
  end

  describe 'intersection with the selected view' do
    it 'cannot reopen a field excluded from the view' do
      expect do
        blueprint.render_as_hash(vehicle, view: :compact, fields: %i[id model])
      end.to raise_error(
        Blueprinter::Errors::InvalidProjection,
        "Field 'model' is excluded from the 'compact' view"
      )
    end

    it 'cannot project a field the view does not contain' do
      narrow = Class.new(Blueprinter::Base) do
        identifier :id
        field :make

        view :narrow do
          field :make
        end
      end

      expect do
        narrow.render_as_hash(vehicle, view: :narrow, fields: %i[id owner])
      end.to raise_error(Blueprinter::Errors::InvalidProjection, /Unknown field 'owner'/)
    end

    it 'projects within a non-default view' do
      expect(blueprint.render_as_hash(vehicle, view: :with_owner, fields: [:id, :make])).to eq(
        id: 1,
        make: 'Super Car'
      )
    end
  end

  describe 'conditional fields (if/unless)' do
    let(:conditional_blueprint) do
      Class.new(Blueprinter::Base) do
        identifier :id
        field :name
        field :secret, if: ->(_field, _object, options) { options[:allow_secret] }
      end
    end

    it 'cannot reopen a field the :if condition disallows' do
      object = OpenStruct.new(id: 1, name: 'Meg', secret: 'shh')

      expect(conditional_blueprint.render_as_hash(object, fields: %i[id name secret])).to eq(
        id: 1,
        name: 'Meg'
      )
    end

    it 'still renders the field when the condition passes' do
      object = OpenStruct.new(id: 1, name: 'Meg', secret: 'shh')

      expect(
        conditional_blueprint.render_as_hash(object, fields: %i[name secret], allow_secret: true)
      ).to eq(id: 1, name: 'Meg', secret: 'shh')
    end
  end

  describe 'unselected fields are never read' do
    it 'does not call extractors for unselected scalar fields' do
      blueprint.render_as_hash(vehicle, fields: [:id, :make])

      expect(vehicle.call_count[:model]).to eq(0)
      expect(vehicle.call_count[:secret_name]).to eq(0)
    end

    it 'does not read an unselected association at all' do
      blueprint.render_as_hash(vehicle, fields: %i[id make])

      expect(vehicle.call_count[:owner]).to eq(0)
    end

    it 'does not invoke blocks of unselected fields' do
      block_called = 0
      block_bp = Class.new(Blueprinter::Base) do
        identifier :id
        field(:computed) { block_called += 1 }
      end

      block_bp.render_as_hash(OpenStruct.new(id: 1), fields: [:id])

      expect(block_called).to eq(0)
    end

    it 'does not invoke a custom extractor for unselected fields' do
      calls = []
      custom_extractor = Class.new(Blueprinter::Extractor) do
        define_method(:extract) do |field_name, object, _options, _field_options|
          calls << field_name
          object.public_send(field_name)
        end
      end

      custom_bp = Class.new(Blueprinter::Base) do
        identifier :id
        field :make, extractor: custom_extractor.new
        field :model, extractor: custom_extractor.new
      end

      custom_bp.render_as_hash(vehicle, fields: [:id, :make])

      expect(calls).to eq([:make])
    end

    it 'skips fields rejected by :if without extracting them' do
      reads = []
      spy_extractor = Class.new(Blueprinter::Extractor) do
        define_method(:extract) do |field_name, object, _options, _field_options|
          reads << field_name
          object.public_send(field_name)
        end
      end

      require 'blueprinter/extractor'
      conditional = Class.new(Blueprinter::Base) do
        identifier :id
        field :make, extractor: spy_extractor.new
        field :model, extractor: spy_extractor.new, if: ->(*) { false }
      end

      conditional.render_as_hash(vehicle, fields: %i[id make model])

      expect(reads).to eq([:make])
    end
  end

  describe 'custom association extractors' do
    it 'does not forward projection to legacy extractors that did not opt in' do
      legacy = Class.new(Blueprinter::Extractor) do
        def extract(association_name, object, _local_options, _options = {})
          associated = object.public_send(association_name)
          { legacy: associated.name }
        end
      end

      owner_bp = owner_blueprint
      custom_bp = Class.new(Blueprinter::Base) do
        identifier :id
        association :owner, blueprint: owner_bp, extractor: legacy.new
      end

      expect(custom_bp.render_as_hash(vehicle, fields: [:id, :owner])).to eq(
        id: 1,
        owner: { legacy: 'Meg' }
      )
    end

    it 'forwards the nested projection when the extractor opts in' do
      received = nil
      optin = Class.new(Blueprinter::Extractor) do
        define_singleton_method(:accepts_projection?) { true }
        define_method(:extract) do |association_name, object, local_options, options = {}, projection: nil|
          received = projection
          Blueprinter::AssociationExtractor.new.extract(
            association_name, object, local_options, options, projection: projection
          )
        end
      end

      owner_bp = owner_blueprint
      custom_bp = Class.new(Blueprinter::Base) do
        identifier :id
        association :owner, blueprint: owner_bp, extractor: optin.new
      end

      result = custom_bp.render_as_hash(vehicle, fields: [:id, owner: [:name]])

      expect(result).to eq(id: 1, owner: { id: 9, name: 'Meg' })
      expect(received).to be_a(Blueprinter::Projection)
      expect(received.selected?(:name)).to be(true)
      expect(received.selected?(:email)).to be(false)
    end
  end

  describe 'nested association projections' do
    it 'projects nested fields while keeping the nested identifier' do
      expect(blueprint.render_as_hash(vehicle, fields: [:id, owner: [:name]])).to eq(
        id: 1,
        owner: { id: 9, name: 'Meg' }
      )
    end

    it 'cascades identifier: false into the association projection' do
      expect(
        blueprint.render_as_hash(
          vehicle,
          fields: [:id, { owner: [:name] }],
          identifier: false
        )
      ).to eq(id: 1, owner: { name: 'Meg' })
    end

    it 're-enables the nested identifier by listing it explicitly' do
      expect(
        blueprint.render_as_hash(
          vehicle,
          fields: [:id, { owner: %i[id name] }],
          identifier: false
        )
      ).to eq(id: 1, owner: { id: 9, name: 'Meg' })
    end

    it 'never reads unselected nested fields' do
      blueprint.render_as_hash(vehicle, fields: [:id, owner: [:name]])

      expect(owner.call_count[:email]).to eq(0)
    end

    it 'fully renders an association selected without a nested spec' do
      expect(blueprint.render_as_hash(vehicle, fields: [:id, :owner])).to eq(
        id: 1,
        owner: { id: 9, name: 'Meg', email: 'meg@example.com' }
      )
    end

    it 'uses the renamed association output name' do
      owner_bp = owner_blueprint
      renamed = Class.new(Blueprinter::Base) do
        identifier :id
        association :owner, name: :person, blueprint: owner_bp
      end

      expect(renamed.render_as_hash(vehicle, fields: [:id, person: [:name]])).to eq(
        id: 1,
        person: { id: 9, name: 'Meg' }
      )
    end

    it 'projects collections of associated objects' do
      owner_bp = owner_blueprint
      garage_bp = Class.new(Blueprinter::Base) do
        identifier :id
        association :vehicles, blueprint: owner_bp
      end
      vehicles = [
        CountingOpenStruct.new(id: 1, name: 'Car', email: 'a@example.com'),
        CountingOpenStruct.new(id: 2, name: 'Truck', email: 'b@example.com')
      ]
      garage = CountingOpenStruct.new(id: 50, vehicles: vehicles)

      expect(garage_bp.render_as_hash(garage, fields: [:id, vehicles: [:name]])).to eq(
        id: 50,
        vehicles: [{ id: 1, name: 'Car' }, { id: 2, name: 'Truck' }]
      )
      expect(vehicles.first.call_count[:email]).to eq(0)
    end

    it 'raises with a full path for an unknown nested field' do
      expect do
        blueprint.render_as_hash(vehicle, fields: [:id, owner: [:nope]])
      end.to raise_error(Blueprinter::Errors::InvalidProjection, /Unknown field 'owner.nope'/)
    end

    it 'raises when a nested projection is applied to a scalar field' do
      expect do
        blueprint.render_as_hash(vehicle, fields: [{ make: [:nope] }])
      end.to raise_error(
        Blueprinter::Errors::InvalidProjection,
        "Field 'make' is not an association and cannot have a nested projection"
      )
    end

    it 'cannot reopen a field excluded in the association view' do
      owner_bp = owner_blueprint
      expect do
        blueprint.render_as_hash(
          vehicle,
          view: :default,
          fields: [:id, owner: [:name]]
        )
      end.not_to raise_error

      nameless = Class.new(Blueprinter::Base) do
        identifier :id
        association :owner, blueprint: owner_bp, view: :nameless
      end

      expect do
        nameless.render_as_hash(vehicle, fields: [:id, owner: [:name]])
      end.to raise_error(
        Blueprinter::Errors::InvalidProjection,
        "Field 'owner.name' is excluded from the 'nameless' view"
      )
    end
  end

  describe 'empty associated values' do
    it 'still applies association defaults for empty values under a nested projection' do
      vehicle_bp = Class.new(Blueprinter::Base) do
        identifier :id
        field :make
      end
      garage_bp = Class.new(Blueprinter::Base) do
        identifier :id
        association :vehicles,
                    blueprint: vehicle_bp,
                    default_if: Blueprinter::EMPTY_COLLECTION,
                    default: 'none'
      end
      garage = CountingOpenStruct.new(id: 5, vehicles: [])

      expect(garage_bp.render_as_hash(garage, fields: [:id, vehicles: [:make]])).to eq(
        id: 5,
        vehicles: 'none'
      )
    end

    it 'renders nil for a nil association with a nested projection' do
      sparse = CountingOpenStruct.new(id: 3, make: 'X', owner: nil)

      expect(blueprint.render_as_hash(sparse, fields: [:id, owner: [:name]])).to eq(
        id: 3,
        owner: nil
      )
    end
  end

  describe 'dynamic blueprints' do
    it 'validates nested projections against the resolved blueprint at render time' do
      owner_bp = owner_blueprint
      dynamic = Class.new(Blueprinter::Base) do
        identifier :id
        association :owner, blueprint: ->(value) { owner_bp }
      end

      expect(dynamic.render_as_hash(vehicle, fields: [:id, owner: [:name]])).to eq(
        id: 1,
        owner: { id: 9, name: 'Meg' }
      )

      expect do
        dynamic.render_as_hash(vehicle, fields: [:id, owner: [:nope]])
      end.to raise_error(Blueprinter::Errors::InvalidProjection, /Unknown field 'owner.nope'/)
    end
  end

  describe 'shared definitions and cache' do
    it 'does not mutate the view definitions or cache across different projections' do
      first = blueprint.render_as_hash(vehicle, fields: [:make])
      second = blueprint.render_as_hash(vehicle, fields: [:id, owner: [:name]])
      third = blueprint.render_as_hash(vehicle)
      fourth = blueprint.render_as_hash(vehicle, fields: [:make])

      expect(first).to eq(id: 1, make: 'Super Car')
      expect(second).to eq(id: 1, owner: { id: 9, name: 'Meg' })
      expect(third).to eq(
        id: 1,
        make: 'Super Car',
        model: 'ACME',
        codename: 'Ghost',
        owner: { id: 9, name: 'Meg', email: 'meg@example.com' }
      )
      expect(fourth).to eq(first)

      expect(blueprint.view_collection.fields_for(:default).map(&:name)).to eq(
        %i[id codename make model owner]
      )
    end

    it 'keeps projections independent for concurrent renders' do
      results = [
        Thread.new { blueprint.render_as_hash(vehicle, fields: [:make]) },
        Thread.new do
          blueprint.render_as_hash(vehicle, fields: [:id, owner: [:email]])
        end
      ].map(&:value)

      expect(results).to contain_exactly(
        { id: 1, make: 'Super Car' },
        { id: 1, owner: { id: 9, email: 'meg@example.com' } }
      )
    end
  end
end
