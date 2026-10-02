# frozen_string_literal: true

require 'ostruct'
require 'json'
require 'blueprinter/extractor'

describe 'field projection' do
  # A minimal extractor that records every read it is asked to perform.
  class CountingExtractor < Blueprinter::Extractor
    @reads = Hash.new(0)

    class << self
      attr_accessor :reads

      def reset!
        @reads = Hash.new(0)
      end
    end

    def extract(field_name, object, _local_options, _options = {})
      self.class.reads[field_name] += 1
      object.public_send(field_name)
    end
  end

  # Raises immediately if read; used to prove an unselected extractor is never invoked.
  class ExplodingExtractor < Blueprinter::Extractor
    def extract(_field_name, _object, _local_options, _options = {})
      raise 'unselected field was extracted'
    end
  end

  # Raises if the association (and therefore its object) is ever read.
  class ExplodingAssociationExtractor < Blueprinter::Extractor
    def extract(_field_name, _object, _local_options, _options = {}, _projection = nil)
      raise 'excluded association was read'
    end
  end

  let(:company_blueprint) do
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name
      field :industry
    end
  end

  let(:owner_blueprint) do
    cb = company_blueprint
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name
      field :email
      association :company, blueprint: cb
    end
  end

  let(:vehicle_blueprint) do
    ob = owner_blueprint
    Class.new(Blueprinter::Base) do
      identifier :id
      field :make
      field :model
      association :owner, blueprint: ob
    end
  end

  let(:blueprint) do
    vb = vehicle_blueprint
    Class.new(Blueprinter::Base) do
      identifier :id
      field :first_name
      field :last_name
      association :vehicle, blueprint: vb, name: :car
    end
  end

  let(:company) { OpenStruct.new(id: 30, name: 'Acme', industry: 'Tech') }
  let(:owner) { OpenStruct.new(id: 5, name: 'Joe', email: 'joe@example.com', company: company) }
  let(:vehicle) { OpenStruct.new(id: 9, make: 'Toyota', model: 'Camry', owner: owner) }
  let(:object) { OpenStruct.new(id: 1, first_name: 'Meg', last_name: 'Ryan', vehicle: vehicle) }

  before do
    Blueprinter.reset_configuration!
    CountingExtractor.reset!
  end
  after { Blueprinter.reset_configuration! }

  describe '.render_as_hash' do
    it 'renders the full view without a projection' do
      expect(blueprint.render_as_hash(object)).to eq(
        id: 1, first_name: 'Meg', last_name: 'Ryan',
        car: {
          id: 9, make: 'Toyota', model: 'Camry',
          owner: {
            id: 5, name: 'Joe', email: 'joe@example.com',
            company: { id: 30, name: 'Acme', industry: 'Tech' }
          }
        }
      )
    end

    it 'projects top-level fields with an array while keeping the identifier' do
      expect(blueprint.render_as_hash(object, fields: [:first_name])).to eq(
        id: 1, first_name: 'Meg'
      )
    end

    it 'accepts hash projections keyed by output field names' do
      expect(blueprint.render_as_hash(object, fields: { first_name: true })).to eq(
        id: 1, first_name: 'Meg'
      )
    end

    it 'projects nested association fields' do
      result = blueprint.render_as_hash(
        object,
        fields: [:id, { car: [:make, { owner: [:name] }] }]
      )

      expect(result).to eq(
        id: 1,
        car: { id: 9, make: 'Toyota', owner: { id: 5, name: 'Joe' } }
      )
    end

    it 'honors identifier toggles at every level' do
      result = blueprint.render_as_hash(
        object,
        fields: {
          identifier: false,
          car: { identifier: false, make: true, owner: { name: true, identifier: false } }
        }
      )

      expect(result).to eq(car: { make: 'Toyota', owner: { name: 'Joe' } })
    end

    it 'allows filtering the identifier out via a false value' do
      expect(blueprint.render_as_hash(object, fields: { id: false, first_name: true })).to eq(
        first_name: 'Meg'
      )
    end

    it 'filters the identifier with the identifier option' do
      expect(blueprint.render_as_hash(object, fields: [:first_name], identifier: false)).to eq(
        first_name: 'Meg'
      )
    end

    it 'keeps the identifier by default even with a projection' do
      result = blueprint.render_as_hash(object, fields: { identifier: false, first_name: true })
      expect(result).to eq(first_name: 'Meg')
    end

    it 'works across collections' do
      result = blueprint.render_as_hash([object, object], fields: [:first_name])
      expect(result).to eq([{ id: 1, first_name: 'Meg' }, { id: 1, first_name: 'Meg' }])
    end
  end

  describe '.render' do
    it 'applies the same projection to JSON output' do
      expect(blueprint.render(object, fields: [:first_name])).to eq('{"id":1,"first_name":"Meg"}')
    end

    it 'produces consistent JSON and Hash output for nested projections' do
      options = { fields: [{ car: [:make, { owner: [:name] }] }] }
      hash = blueprint.render_as_hash(object, **options)

      expect(JSON.parse(blueprint.render(object, **options), symbolize_names: true)).to eq(hash)
    end
  end

  describe 'collections within associations' do
    let(:multi_owner_blueprint) do
      Class.new(Blueprinter::Base) do
        identifier :id
        field :name
      end
    end

    let(:multi_vehicle_blueprint) do
      owner_class = multi_owner_blueprint
      Class.new(Blueprinter::Base) do
        identifier :id
        field :make
        association :owners, blueprint: owner_class
      end
    end

    let(:multi_blueprint) do
      vehicle_class = multi_vehicle_blueprint
      Class.new(Blueprinter::Base) do
        identifier :id
        association :vehicle, blueprint: vehicle_class
      end
    end

    let(:multi_object) do
      owners = [
        OpenStruct.new(id: 5, name: 'Joe'),
        OpenStruct.new(id: 6, name: 'Jane')
      ]
      OpenStruct.new(id: 1, vehicle: OpenStruct.new(id: 9, make: 'Toyota', owners: owners))
    end

    it 'projects each element of an associated collection' do
      result = multi_blueprint.render_as_hash(
        multi_object,
        fields: [{ vehicle: [{ owners: [:name] }, :make] }]
      )

      expect(result).to eq(
        id: 1,
        vehicle: { id: 9, make: 'Toyota', owners: [{ id: 5, name: 'Joe' }, { id: 6, name: 'Jane' }] }
      )
    end

    it 'projects each element and can drop nested identifiers' do
      result = multi_blueprint.render_as_hash(
        multi_object,
        fields: [{ vehicle: [{ owners: { name: true, identifier: false } }] }]
      )

      expect(result).to eq(
        id: 1,
        vehicle: { id: 9, owners: [{ name: 'Joe' }, { name: 'Jane' }] }
      )
    end

    it 'does not read an associated collection the projection leaves out' do
      object_without_owners = OpenStruct.new(id: 1, vehicle: OpenStruct.new(id: 9, make: 'Toyota'))
      def object_without_owners.owners
        raise 'association collection was read'
      end

      expect {
        multi_blueprint.render_as_hash(
          object_without_owners,
          fields: [{ vehicle: [:make] }]
        )
      }.not_to raise_error
    end

  end

  describe 'avoiding work' do
    let(:counting_blueprint) do
      extractor = CountingExtractor
      Class.new(Blueprinter::Base) do
        field :first_name, extractor: extractor
        field :last_name, extractor: extractor
        field :email, extractor: ExplodingExtractor.new
      end
    end

    let(:counting_object) { OpenStruct.new(first_name: 'Meg', last_name: 'Ryan', email: 'a@b.c') }

    it 'never invokes the extractor for unselected fields' do
      counting_blueprint.render_as_hash(counting_object, fields: [:first_name])

      expect(CountingExtractor.reads).to eq(first_name: 1)
    end

    it 'does not read an excluded association object' do
      vb = Class.new(Blueprinter::Base) do
        identifier :id
        field :make
      end
      bp = Class.new(Blueprinter::Base) do
        identifier :id
        association :vehicle, blueprint: vb, extractor: ExplodingAssociationExtractor.new
      end

      expect { bp.render_as_hash(object, fields: [:id]) }.not_to raise_error
    end

    it 'does not read nested associations that the projection leaves out' do
      owner_bp = Class.new(Blueprinter::Base) { identifier :id }
      nested_vehicle_bp = Class.new(Blueprinter::Base) do
        identifier :id
        field :make
        association :owner, blueprint: owner_bp, extractor: ExplodingAssociationExtractor.new
      end
      bp = Class.new(Blueprinter::Base) do
        identifier :id
        association :vehicle, blueprint: nested_vehicle_bp
      end

      result = bp.render_as_hash(object, fields: [{ vehicle: [:make] }])
      expect(result).to eq(id: 1, vehicle: { id: 9, make: 'Toyota' })
    end
  end

  describe 'view intersection' do
    let(:view_blueprint) do
      Class.new(Blueprinter::Base) do
        identifier :id
        field :first_name
        field :last_name

        view :extended do
          field :description
        end

        field :conditional, if: ->(_name, obj, _opts) { obj.show_conditional? }
        field :blocked, unless: ->(_name, _obj, _opts) { true }
      end
    end

    let(:view_object) do
      OpenStruct.new(
        id: 1,
        first_name: 'Meg',
        last_name: 'Ryan',
        description: 'A person',
        conditional: 'c',
        blocked: 'b'
      )
    end

    before { def view_object.show_conditional?; false; end }

    it 'cannot re-open a field the view excludes' do
      expect {
        view_blueprint.render_as_hash(view_object, view: :default, fields: [:description])
      }.to raise_error(
        Blueprinter::Errors::InvalidProjection,
        "Unknown field 'description'"
      )
    end

    it 'projects fields available in the selected view' do
      expect(
        view_blueprint.render_as_hash(view_object, view: :extended, fields: [:description])
      ).to eq(id: 1, description: 'A person')
    end

    it 'does not re-open fields removed by :if / :unless' do
      result = view_blueprint.render_as_hash(
        view_object,
        fields: %i[first_name conditional blocked]
      )
      expect(result).to eq(id: 1, first_name: 'Meg')
    end
  end

  describe 'association features' do
    it 'uses the output (renamed) association name in the projection' do
      result = blueprint.render_as_hash(object, fields: [{ car: [:make] }])
      expect(result).to eq(id: 1, car: { id: 9, make: 'Toyota' })
    end

    it 'rejects the underlying method name for a renamed association' do
      expect {
        blueprint.render_as_hash(object, fields: [{ vehicle: [:make] }])
      }.to raise_error(Blueprinter::Errors::InvalidProjection, "Unknown field 'vehicle'")
    end

    it 'supports dynamic blueprint selection' do
      dynamic_owner_bp = Class.new(Blueprinter::Base) do
        identifier :id
        field :name
      end
      owner_class = dynamic_owner_bp
      dynamic_vehicle_bp = Class.new(Blueprinter::Base) do
        identifier :id
        field :make
        association :owner, blueprint: ->(_value) { owner_class }
      end
      bp = Class.new(Blueprinter::Base) do
        identifier :id
        association :vehicle, blueprint: dynamic_vehicle_bp
      end
      def object.dynamic?; true; end

      result = bp.render_as_hash(object, fields: [{ vehicle: [:make, { owner: [:name] }] }])
      expect(result).to eq(
        id: 1,
        vehicle: { id: 9, make: 'Toyota', owner: { id: 5, name: 'Joe' } }
      )
    end

    it 'passes the nested projection to a custom association extractor via options' do
      received = []
      vb = vehicle_blueprint
      custom_extractor = Class.new(Blueprinter::Extractor) do
        define_method(:extract) do |_name, object, _local_options, options = {}|
          received << options[Blueprinter::FieldProjection::OPTION_KEY]
          object.vehicle
        end
      end

      bp = Class.new(Blueprinter::Base) do
        identifier :id
        association :vehicle, blueprint: vb, extractor: custom_extractor
      end

      bp.render_as_hash(object, fields: [{ vehicle: [:make] }])

      expect(received.size).to eq(1)
      expect(received.first.includes?(:make)).to be(true)
      expect(received.first.includes?(:model)).to be(false)
    end

    it 'reports nested unknown fields for dynamic blueprints with the full path' do
      dynamic_owner_bp = Class.new(Blueprinter::Base) { identifier :id }
      dynamic_vehicle_bp = Class.new(Blueprinter::Base) do
        identifier :id
        association :owner, blueprint: ->(_value) { dynamic_owner_bp }
      end
      bp = Class.new(Blueprinter::Base) do
        identifier :id
        association :vehicle, blueprint: dynamic_vehicle_bp
      end

      expect {
        bp.render_as_hash(object, fields: [{ vehicle: [{ owner: [:name] }] }])
      }.to raise_error(Blueprinter::Errors::InvalidProjection, "Unknown field 'vehicle.owner.name'")
    end
  end

  describe 'shared definitions' do
    it 'does not mutate the shared view definition across differing projections' do
      first = blueprint.render_as_hash(object, fields: [:first_name])
      second = blueprint.render_as_hash(object, fields: { car: [:make] })
      full = blueprint.render_as_hash(object)

      expect(first).to eq(id: 1, first_name: 'Meg')
      expect(second).to eq(id: 1, car: { id: 9, make: 'Toyota' })
      expect(full[:car][:model]).to eq('Camry')
      expect(full[:last_name]).to eq('Ryan')
    end

    it 'does not leak projection state into the view cache' do
      blueprint.render_as_hash(object, fields: [:first_name], identifier: false)
      blueprint.render_as_hash(object, fields: { car: [:make, { owner: [:name] }] })

      names = blueprint.view_collection.fields_for(:default).map(&:name)
      expect(names).to eq(%i[id car first_name last_name])
    end
  end
end
