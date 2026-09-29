# frozen_string_literal: true

require 'test_helper'
require 'json'

class MachineCollectionSnapshotTest < StateMachinesTest
  def setup
    @machines = StateMachines::MachineCollection.new

    @klass = Class.new do
      attr_accessor :saved, :transitioned

      def save
        @saved = true
      end
    end

    # First machine
    @machines[:state] = @state = StateMachines::Machine.new(@klass, :state, initial: :parked, action: :save)
    @state.event :ignite do
      transition parked: :idling
    end
    @state.after_transition { |object| object.transitioned = true }

    # Second machine
    @machines[:alarm_state] = @alarm_state = StateMachines::Machine.new(@klass, :alarm_state, initial: :active, action: :save)
    @alarm_state.event :disable do
      transition active: :off
    end

    @object = @klass.new
  end

  def test_should_include_machine_name_version_and_value
    snapshot = @machines.snapshot(@object)
    assert_equal %w[state alarm_state], snapshot.keys
    assert_equal({ 'version' => 1, 'value' => 'parked' }, snapshot['state'])
    assert_equal({ 'version' => 1, 'value' => 'active' }, snapshot['alarm_state'])
  end

  def test_should_reflect_current_state_values
    @object.ignite
    assert_equal 'idling', @machines.snapshot(@object)['state']['value']
  end

  def test_should_survive_json_round_trip
    snapshot = @machines.snapshot(@object)
    assert_equal snapshot, JSON.parse(JSON.generate(snapshot))
  end

  def test_should_raise_if_current_value_is_not_json_compatible
    @object.state = Object.new
    assert_raises(StateMachines::InvalidSnapshot) { @machines.snapshot(@object) }
  end

  def test_should_restore_states
    @machines.restore(@object, 'state' => { 'version' => 1, 'value' => 'idling' },
                              'alarm_state' => { 'version' => 1, 'value' => 'off' })
    assert_equal 'idling', @object.state
    assert_equal 'off', @object.alarm_state
  end

  def test_should_restore_snapshot_after_json_round_trip
    snapshot = JSON.parse(JSON.generate('state' => { 'version' => 1, 'value' => 'idling' },
                                        'alarm_state' => { 'version' => 1, 'value' => 'off' }))
    @machines.restore(@object, snapshot)
    assert_equal 'idling', @object.state
    assert_equal 'off', @object.alarm_state
  end

  def test_should_not_run_actions_or_callbacks_on_restore
    @machines.restore(@object, 'state' => { 'version' => 1, 'value' => 'idling' },
                              'alarm_state' => { 'version' => 1, 'value' => 'off' })
    assert_nil @object.saved
    assert_nil @object.transitioned
  end

  def test_should_leave_all_machines_untouched_when_second_snapshot_is_broken
    snapshot = {
      'state' => { 'version' => 1, 'value' => 'idling' },
      'alarm_state' => { 'version' => 1, 'value' => 'invalid' }
    }

    assert_raises(StateMachines::InvalidSnapshot) { @machines.restore(@object, snapshot) }
    assert_equal 'parked', @object.state
    assert_equal 'active', @object.alarm_state
  end

  def test_should_leave_all_machines_untouched_when_first_snapshot_is_broken
    snapshot = {
      'state' => { 'version' => 1, 'value' => 'invalid' },
      'alarm_state' => { 'version' => 1, 'value' => 'off' }
    }

    assert_raises(StateMachines::InvalidSnapshot) { @machines.restore(@object, snapshot) }
    assert_equal 'parked', @object.state
    assert_equal 'active', @object.alarm_state
  end

  def test_should_leave_all_machines_untouched_when_machine_entry_is_missing
    assert_raises(StateMachines::InvalidSnapshot) do
      @machines.restore(@object, 'state' => { 'version' => 1, 'value' => 'idling' })
    end
    assert_equal 'parked', @object.state
    assert_equal 'active', @object.alarm_state
  end

  def test_should_raise_if_snapshot_contains_unknown_machine
    snapshot = @machines.snapshot(@object).merge('unknown' => { 'version' => 1, 'value' => 'x' })
    assert_raises(StateMachines::InvalidSnapshot) { @machines.restore(@object, snapshot) }
  end

  def test_should_raise_if_snapshot_is_not_a_hash
    assert_raises(StateMachines::InvalidSnapshot) { @machines.restore(@object, 'parked') }
  end

  def test_should_raise_if_snapshot_has_symbol_keys
    assert_raises(StateMachines::InvalidSnapshot) do
      @machines.restore(@object, state: { version: 1, value: 'idling' })
    end
  end

  def test_should_raise_if_snapshot_contains_arbitrary_ruby_objects
    snapshot = @machines.snapshot(@object)
    snapshot['state']['value'] = Object.new
    assert_raises(StateMachines::InvalidSnapshot) { @machines.restore(@object, snapshot) }
  end

  def test_should_apply_registered_migrations
    klass = Class.new
    machines = StateMachines::MachineCollection.new
    machines[:state] = machine = StateMachines::Machine.new(klass, :state, initial: :stopped, version: 2)
    machine.event :ignite do
      transition stopped: :idling
    end
    machine.migrate(1) { |value| value == 'parked' ? 'stopped' : value }
    machines[:alarm_state] = StateMachines::Machine.new(klass, :alarm_state, initial: :active)

    object = klass.new
    machines.restore(object, 'state' => { 'version' => 1, 'value' => 'parked' },
                             'alarm_state' => { 'version' => 1, 'value' => 'active' })
    assert_equal 'stopped', object.state
    assert_equal 'active', object.alarm_state
  end

  def test_should_raise_if_migration_is_missing
    klass = Class.new
    machines = StateMachines::MachineCollection.new
    machines[:state] = machine = StateMachines::Machine.new(klass, :state, initial: :stopped, version: 2)
    machine.event :ignite do
      transition stopped: :idling
    end
    machines[:alarm_state] = StateMachines::Machine.new(klass, :alarm_state, initial: :active)

    object = klass.new
    assert_raises(StateMachines::InvalidSnapshot) do
      machines.restore(object, 'state' => { 'version' => 1, 'value' => 'parked' },
                               'alarm_state' => { 'version' => 1, 'value' => 'active' })
    end
    assert_equal 'stopped', object.state
    assert_equal 'active', object.alarm_state
  end
end
