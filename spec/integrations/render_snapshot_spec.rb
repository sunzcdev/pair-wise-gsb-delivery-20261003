# frozen_string_literal: true

require 'spec_helper'
require 'ostruct'
require 'timeout'
require 'blueprinter/extractor'

describe 'render snapshot mode' do
  before { reset_blueprinter_config! }
  after { reset_blueprinter_config! }

  # An extractor that blocks the first time it is used, giving the test a
  # controlled synchronization point to mutate view definitions mid-render.
  let(:pausing_extractor) do
    gate = pausing_gate
    resume = resume_gate
    paused = false
    mutex = Mutex.new
    Class.new(Blueprinter::Extractor) do
      define_method(:extract) do |field_name, object, _local_options, _options = {}|
        first_call = mutex.synchronize do
          if paused
            false
          else
            paused = true
            true
          end
        end
        if first_call
          gate << true
          resume.pop
        end
        object.public_send(field_name)
      end
    end.new
  end
  let(:pausing_gate) { Queue.new }
  let(:resume_gate) { Queue.new }

  let(:vehicle_blueprint) do
    extractor = pausing_extractor
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name, extractor: extractor
    end
  end
  let(:user_blueprint) do
    vehicles = vehicle_blueprint
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name
      association :vehicles, blueprint: vehicles
    end
  end
  let(:users) do
    [
      OpenStruct.new(id: 1, name: 'Meg', email: 'meg@example.com',
                     vehicles: [OpenStruct.new(id: 11, name: 'car', speed: 100)]),
      OpenStruct.new(id: 2, name: 'John', email: 'john@example.com',
                     vehicles: [OpenStruct.new(id: 12, name: 'truck', speed: 80)])
    ]
  end
  let(:snapshot_before_redefinition) do
    [
      { id: 1, name: 'Meg', vehicles: [{ id: 11, name: 'car' }] },
      { id: 2, name: 'John', vehicles: [{ id: 12, name: 'truck' }] }
    ]
  end
  let(:snapshot_after_redefinition) do
    [
      { id: 1, name: 'Meg', email: 'meg@example.com', vehicles: [{ id: 11, name: 'car', speed: 100 }] },
      { id: 2, name: 'John', email: 'john@example.com', vehicles: [{ id: 12, name: 'truck', speed: 80 }] }
    ]
  end

  def redefine_views_mid_render
    # wait until the render is paused inside the first object's extractor
    Timeout.timeout(10) { pausing_gate.pop }
    user = user_blueprint
    vehicle = vehicle_blueprint
    user.view(:default) { user.field :email }
    vehicle.view(:default) { vehicle.field :speed }
    resume_gate << true # let the paused render continue with the new definitions in place
  end

  context 'with snapshot: true' do
    it 'pins the root and nested view definitions for the whole render' do
      render_thread = Thread.new { user_blueprint.render_as_hash(users, snapshot: true) }
      redefine_views_mid_render

      expect(render_thread.value).to eq(snapshot_before_redefinition)
    end

    it 'makes definition updates visible to the next top-level render' do
      render_thread = Thread.new { user_blueprint.render_as_hash(users, snapshot: true) }
      redefine_views_mid_render
      render_thread.join

      expect(user_blueprint.render_as_hash(users, snapshot: true)).to eq(snapshot_after_redefinition)
    end

    it 'pins dynamically selected blueprints on first encounter' do
      vehicles = vehicle_blueprint
      dynamic_blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        association :vehicles, blueprint: ->(_value) { vehicles }
      end
      render_thread = Thread.new { dynamic_blueprint.render_as_hash(users, snapshot: true) }
      redefine_views_mid_render

      expect(render_thread.value).to eq(snapshot_before_redefinition.map { |hash| hash.slice(:id, :vehicles) })
    end

    it 'releases the pinned definitions when the render raises' do
      blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        field :name
        field(:boom) do |object, _options|
          raise 'boom' if object.id == 2

          'ok'
        end
      end
      objects = [OpenStruct.new(id: 1, name: 'Meg'), OpenStruct.new(id: 2, name: 'John')]

      expect { blueprint.render_as_hash(objects, snapshot: true) }.to raise_error('boom')

      # The failed render's pinned state is gone: the next top-level render
      # sees the definitions as they are now.
      blueprint.view(:default) { blueprint.field :email }
      expect(blueprint.render_as_hash(objects.first, snapshot: true)).to eq(
        id: 1, name: 'Meg', boom: 'ok', email: nil
      )
    end

    it 'does not let concurrent renders overwrite each other' do
      slow_render = Thread.new { user_blueprint.render_as_hash(users, snapshot: true) }
      Timeout.timeout(10) { pausing_gate.pop }
      user = user_blueprint
      vehicle = vehicle_blueprint
      user.view(:default) { user.field :email }
      vehicle.view(:default) { vehicle.field :speed }

      fast_result = user_blueprint.render_as_hash(users, snapshot: true)

      resume_gate << true
      expect(slow_render.value).to eq(snapshot_before_redefinition)
      expect(fast_result).to eq(snapshot_after_redefinition)
    end

    it 'does not freeze the objects being rendered' do
      vehicle = Class.new(Blueprinter::Base) do
        identifier :id
        field :name
      end
      plain_blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        field :name
        association :vehicles, blueprint: vehicle
      end
      plain_blueprint.render_as_hash(users, snapshot: true)

      expect(users).to all(satisfy { |user| !user.frozen? && user.vehicles.none?(&:frozen?) })
    end
  end

  context 'without snapshot mode' do
    it 'picks up redefinitions made mid-render' do
      render_thread = Thread.new { user_blueprint.render_as_hash(users) }
      redefine_views_mid_render

      expect(render_thread.value.last).to eq(snapshot_after_redefinition.last)
    end
  end
end
