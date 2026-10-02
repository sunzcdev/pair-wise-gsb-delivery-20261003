# frozen_string_literal: true

describe 'Association preloading' do
  # Plain in-memory objects. The association readers raise to prove that
  # Blueprinter never falls back to per-object queries once :preload is used.
  let(:user_class) do
    Class.new do
      attr_reader :id, :name

      def initialize(id, name)
        @id = id
        @name = name
      end

      def vehicles
        raise 'per-object query should never happen'
      end
    end
  end

  let(:vehicle_class) do
    Class.new do
      attr_reader :id, :name

      def initialize(id, name)
        @id = id
        @name = name
      end

      def parts
        raise 'per-object query should never happen'
      end
    end
  end

  let(:part_class) do
    Class.new do
      attr_reader :id, :name

      def initialize(id, name)
        @id = id
        @name = name
      end
    end
  end

  let(:part_blueprint) do
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name
    end
  end

  let(:vehicle_blueprint) do
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name
    end
  end

  # In-memory counter: each entry is the array of objects passed to one preload call.
  let(:preload_calls) { [] }
  let(:preload) do
    calls = preload_calls
    data = vehicle_data
    ->(users) do
      calls << users
      users.to_h { |user| [user, data.fetch(user.id)] }
    end
  end

  let(:user_blueprint) do
    blueprint = vehicle_blueprint
    preloader = preload
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name
      association :vehicles, blueprint:, preload: preloader
    end
  end

  let(:car) { vehicle_class.new(10, 'Car') }
  let(:truck) { vehicle_class.new(11, 'Truck') }
  let(:vehicle_data) { { 1 => [car], 2 => [truck] } }
  let(:users) { [user_class.new(1, 'Meg'), user_class.new(2, 'Tom')] }

  it 'bulk-loads the association once per collection render' do
    result = user_blueprint.render_as_hash(users)

    expect(preload_calls).to eq([users])
    expect(result).to eq(
      [
        { id: 1, name: 'Meg', vehicles: [{ id: 10, name: 'Car' }] },
        { id: 2, name: 'Tom', vehicles: [{ id: 11, name: 'Truck' }] }
      ]
    )
  end

  it 'scopes preloading to the single object when rendering one object' do
    user = users.first
    result = user_blueprint.render_as_hash(user)

    expect(preload_calls).to eq([[user]])
    expect(result).to eq({ id: 1, name: 'Meg', vehicles: [{ id: 10, name: 'Car' }] })
  end

  context 'when the same object appears multiple times in the collection' do
    it 'preloads it only once but keeps the duplicate positions in the output' do
      duplicated = [users.first, users.last, users.first]
      result = user_blueprint.render_as_hash(duplicated)

      expect(preload_calls).to eq([users])
      expect(result.size).to eq(3)
      expect(result[0]).to eq(result[2])
      expect(result[0][:vehicles]).to eq([{ id: 10, name: 'Car' }])
    end
  end

  context 'when the association is renamed' do
    let(:user_blueprint) do
      blueprint = vehicle_blueprint
      preloader = preload
      Class.new(Blueprinter::Base) do
        identifier :id
        association :vehicles, name: :cars, blueprint:, preload: preloader
      end
    end

    it 'renders the preloaded values under the alias' do
      result = user_blueprint.render_as_hash(users)

      expect(preload_calls.size).to eq(1)
      expect(result.first).to eq({ id: 1, cars: [{ id: 10, name: 'Car' }] })
    end
  end

  context 'with views and exclusions' do
    let(:user_blueprint) do
      blueprint = vehicle_blueprint
      preloader = preload
      Class.new(Blueprinter::Base) do
        identifier :id
        field :name

        view :with_vehicles do
          association :vehicles, blueprint:, preload: preloader
        end

        view :without_vehicles do
          include_view :with_vehicles
          exclude :vehicles
        end
      end
    end

    it 'does not preload when the rendered view does not contain the association' do
      result = user_blueprint.render_as_hash(users)

      expect(preload_calls).to be_empty
      expect(result).to eq([{ id: 1, name: 'Meg' }, { id: 2, name: 'Tom' }])
    end

    it 'preloads when the rendered view contains the association' do
      user_blueprint.render_as_hash(users, view: :with_vehicles)

      expect(preload_calls).to eq([users])
    end

    it 'does not preload when the association is excluded from the view' do
      result = user_blueprint.render_as_hash(users, view: :without_vehicles)

      expect(preload_calls).to be_empty
      expect(result.first).not_to have_key(:vehicles)
    end
  end

  context 'with :if and :unless conditions' do
    let(:users) { [user_class.new(1, 'Meg'), user_class.new(2, 'Tom'), user_class.new(3, 'Ann')] }
    let(:vehicle_data) { { 1 => [car], 3 => [truck] } }

    it 'only passes objects matching the :if condition to the preload callable' do
      blueprint = vehicle_blueprint
      preloader = preload
      conditional = Class.new(Blueprinter::Base) do
        identifier :id
        association :vehicles, blueprint:, preload: preloader, if: ->(_field, user, _opts) { user.id.odd? }
      end

      result = conditional.render_as_hash(users)

      expect(preload_calls).to eq([[users[0], users[2]]])
      expect(result[0][:vehicles]).to eq([{ id: 10, name: 'Car' }])
      expect(result[1]).not_to have_key(:vehicles)
      expect(result[2][:vehicles]).to eq([{ id: 11, name: 'Truck' }])
    end

    it 'only passes objects not matching the :unless condition to the preload callable' do
      blueprint = vehicle_blueprint
      preloader = preload
      conditional = Class.new(Blueprinter::Base) do
        identifier :id
        association :vehicles, blueprint:, preload: preloader, unless: ->(_field, user, _opts) { user.id.even? }
      end

      conditional.render_as_hash(users)

      expect(preload_calls).to eq([[users[0], users[2]]])
    end
  end

  context 'when the preload result contains a nil entry' do
    let(:vehicle_data) { { 1 => nil, 2 => [truck] } }

    it 'renders it as an empty association' do
      result = user_blueprint.render_as_hash(users)

      expect(result[0][:vehicles]).to be_nil
      expect(result[1][:vehicles]).to eq([{ id: 11, name: 'Truck' }])
    end
  end

  context 'when the preload result is missing an entry for an object' do
    let(:preload) { ->(_users) { {} } }

    it 'raises instead of falling back to per-object queries' do
      expect { user_blueprint.render_as_hash(users) }.
        to raise_error(Blueprinter::Errors::MissingPreloadResult, /:vehicles/)
    end
  end

  context 'with nested preloaded associations' do
    let(:part_calls) { [] }
    let(:wheel) { part_class.new(100, 'Wheel') }
    let(:engine) { part_class.new(101, 'Engine') }
    let(:part_data) { { 10 => [wheel], 11 => [engine] } }
    let(:parts_preload) do
      calls = part_calls
      data = part_data
      ->(vehicles) do
        calls << vehicles
        vehicles.to_h { |vehicle| [vehicle, data.fetch(vehicle.id)] }
      end
    end

    let(:vehicle_blueprint) do
      blueprint = part_blueprint
      preloader = parts_preload
      Class.new(Blueprinter::Base) do
        identifier :id
        field :name
        association :parts, blueprint:, preload: preloader
      end
    end

    it 'preloads each level within the scope of its parent render' do
      result = user_blueprint.render_as_hash(users)

      expect(preload_calls).to eq([users])
      expect(part_calls).to eq([[car], [truck]])
      expect(result[0][:vehicles]).to eq([{ id: 10, name: 'Car', parts: [{ id: 100, name: 'Wheel' }] }])
      expect(result[1][:vehicles]).to eq([{ id: 11, name: 'Truck', parts: [{ id: 101, name: 'Engine' }] }])
    end
  end

  context 'when a render fails' do
    it 're-runs the preload callable on the next render instead of reusing stale data' do
      calls = []
      data = vehicle_data
      flaky_preload = lambda do |objects|
        calls << objects
        calls.size == 1 ? {} : objects.to_h { |user| [user, data.fetch(user.id)] }
      end
      blueprint = vehicle_blueprint
      blueprint_class = Class.new(Blueprinter::Base) do
        identifier :id
        association :vehicles, blueprint:, preload: flaky_preload
      end

      expect { blueprint_class.render_as_hash(users) }.
        to raise_error(Blueprinter::Errors::MissingPreloadResult)

      result = blueprint_class.render_as_hash(users)
      expect(calls.size).to eq(2)
      expect(result).to eq(
        [
          { id: 1, vehicles: [{ id: 10, name: 'Car' }] },
          { id: 2, vehicles: [{ id: 11, name: 'Truck' }] }
        ]
      )
    end

    it 'propagates errors raised by the preload callable and recovers on the next render' do
      calls = []
      data = vehicle_data
      failing_preload = lambda do |objects|
        calls << objects
        raise 'query failed' if calls.size == 1

        objects.to_h { |user| [user, data.fetch(user.id)] }
      end
      blueprint = vehicle_blueprint
      blueprint_class = Class.new(Blueprinter::Base) do
        identifier :id
        association :vehicles, blueprint:, preload: failing_preload
      end

      expect { blueprint_class.render_as_hash(users) }.to raise_error('query failed')
      expect(blueprint_class.render_as_hash(users).first[:vehicles]).to eq([{ id: 10, name: 'Car' }])
      expect(calls.size).to eq(2)
    end
  end

  context 'with an invalid :preload option' do
    it 'raises when the preload option is not callable' do
      blueprint = vehicle_blueprint

      expect do
        Class.new(Blueprinter::Base) { association :vehicles, blueprint:, preload: :vehicles }
      end.to raise_error(ArgumentError, /:preload must be a callable/)
    end

    it 'raises when combined with a custom extractor' do
      blueprint = vehicle_blueprint

      expect do
        Class.new(Blueprinter::Base) do
          association :vehicles, blueprint:, extractor: Blueprinter::AutoExtractor.new, preload: ->(objects) { objects }
        end
      end.to raise_error(ArgumentError, /AssociationExtractor/)
    end
  end
end
