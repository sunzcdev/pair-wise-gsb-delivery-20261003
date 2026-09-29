# frozen_string_literal: true

require 'test_helper'

class MachineCollectionSnapshotTest < StateMachinesTest
  def setup
    @klass = Class.new do
      attr_accessor :state, :alarm_state
    end

    @machine = StateMachines::Machine.new(@klass, :state, initial: :parked)
    @machine.event(:ignite) { transition parked: :idling }
    @machine.event(:park) { transition idling: :parked }

    @alarm_machine = StateMachines::Machine.new(@klass, :alarm_state, initial: :active)
    @alarm_machine.event(:disable) { transition active: :off }

    @object = @klass.new
  end

  def test_should_export_machine_identifier_version_and_value
    snapshot = @object.state_machines_snapshot

    assert_equal [
      { 'machine' => 'state', 'version' => 1, 'value' => 'parked' },
      { 'machine' => 'alarm_state', 'version' => 1, 'value' => 'active' }
    ], snapshot
  end

  def test_should_export_current_state_values
    @object.ignite

    snapshot = @object.state_machines_snapshot

    assert_equal 'idling', snapshot[0]['value']
  end

  def test_should_export_configured_definition_version
    machine = StateMachines::Machine.new(@klass, :versioned_state, initial: :one, version: 3)
    machine.state :two

    assert_equal 3, machine.version
  end

  def test_should_round_trip_through_json
    require 'json'

    @object.ignite
    snapshot = JSON.parse(JSON.generate(@object.state_machines_snapshot))

    restored = @klass.new
    assert restored.restore_state_machines_snapshot(snapshot)
    assert_equal 'idling', restored.state
    assert_equal 'active', restored.alarm_state
  end

  def test_should_restore_without_firing_events_or_actions
    klass = Class.new do
      attr_accessor :state, :saved

      state_machine :state, initial: :parked, action: :save do
        before_transition { |object| object.saved = :callback_ran }

        event :ignite do
          transition parked: :idling
        end
      end

      def save
        self.saved = :action_ran
        true
      end
    end

    object = klass.new
    object.restore_state_machines_snapshot([{ 'machine' => 'state', 'version' => 1, 'value' => 'idling' }])

    assert_equal 'idling', object.state
    assert_nil object.saved
  end

  def test_should_apply_registered_migrations_for_older_versions
    klass = Class.new do
      attr_accessor :state

      state_machine :state, initial: :parked, version: 3 do
        migrate_version(1) { |value| value == 'waiting' ? 'held' : value }
        migrate_version(2) { |value| value == 'held' ? 'parked' : value }

        event :ignite do
          transition parked: :idling
        end
      end
    end

    object = klass.new
    object.restore_state_machines_snapshot([{ 'machine' => 'state', 'version' => 1, 'value' => 'waiting' }])

    assert_equal 'parked', object.state
  end

  def test_should_raise_if_migration_step_is_missing
    klass = Class.new do
      attr_accessor :state

      state_machine :state, initial: :parked, version: 3 do
        migrate_version(2) { |value| value }

        event :ignite do
          transition parked: :idling
        end
      end
    end

    object = klass.new
    error = assert_raises(StateMachines::SnapshotError) do
      object.restore_state_machines_snapshot([{ 'machine' => 'state', 'version' => 1, 'value' => 'parked' }])
    end
    assert_match(/no migration registered/i, error.message)
    assert_equal 'parked', object.state
  end

  def test_should_raise_if_snapshot_version_is_newer_than_definition
    error = assert_raises(StateMachines::SnapshotError) do
      @object.restore_state_machines_snapshot([{ 'machine' => 'state', 'version' => 2, 'value' => 'parked' }])
    end
    assert_match(/newer definition version/, error.message)
  end

  def test_should_raise_if_machine_is_unknown
    error = assert_raises(StateMachines::SnapshotError) do
      @object.restore_state_machines_snapshot([{ 'machine' => 'unknown', 'version' => 1, 'value' => 'parked' }])
    end
    assert_match(/unknown state machine/i, error.message)
  end

  def test_should_raise_if_state_value_is_unknown
    error = assert_raises(StateMachines::SnapshotError) do
      @object.restore_state_machines_snapshot([{ 'machine' => 'state', 'version' => 1, 'value' => 'flying' }])
    end
    assert_match(/not a known state state value/, error.message)
  end

  def test_should_raise_if_value_is_not_json_compatible
    assert_raises(StateMachines::SnapshotError) do
      @object.restore_state_machines_snapshot([{ 'machine' => 'state', 'version' => 1, 'value' => :parked }])
    end
  end

  def test_should_raise_if_snapshot_is_not_an_array
    assert_raises(StateMachines::SnapshotError) do
      @object.restore_state_machines_snapshot('state' => 'parked')
    end
  end

  def test_should_raise_if_entry_has_symbol_keys
    assert_raises(StateMachines::SnapshotError) do
      @object.restore_state_machines_snapshot([{ machine: 'state', version: 1, value: 'parked' }])
    end
  end

  def test_should_raise_if_entry_has_missing_or_unknown_keys
    assert_raises(StateMachines::SnapshotError) do
      @object.restore_state_machines_snapshot([{ 'machine' => 'state', 'version' => 1 }])
    end

    assert_raises(StateMachines::SnapshotError) do
      @object.restore_state_machines_snapshot([{ 'machine' => 'state', 'version' => 1, 'value' => 'parked', 'extra' => true }])
    end
  end

  def test_should_raise_if_machine_appears_twice
    assert_raises(StateMachines::SnapshotError) do
      @object.restore_state_machines_snapshot([
        { 'machine' => 'state', 'version' => 1, 'value' => 'parked' },
        { 'machine' => 'state', 'version' => 1, 'value' => 'idling' }
      ])
    end
  end

  def test_should_raise_on_export_if_value_is_not_json_compatible
    @object.state = :parked

    assert_raises(StateMachines::SnapshotError) { @object.state_machines_snapshot }
  end

  def test_should_leave_all_machines_unchanged_if_any_entry_is_invalid
    @object.ignite
    @object.disable
    assert_equal 'idling', @object.state
    assert_equal 'off', @object.alarm_state

    snapshot = [
      { 'machine' => 'state', 'version' => 1, 'value' => 'parked' },
      { 'machine' => 'alarm_state', 'version' => 1, 'value' => 'bogus' }
    ]

    assert_raises(StateMachines::SnapshotError) do
      @object.restore_state_machines_snapshot(snapshot)
    end

    assert_equal 'idling', @object.state
    assert_equal 'off', @object.alarm_state
  end

  def test_should_leave_all_machines_unchanged_if_second_machine_is_unknown
    snapshot = [
      { 'machine' => 'state', 'version' => 1, 'value' => 'idling' },
      { 'machine' => 'bogus_machine', 'version' => 1, 'value' => 'active' }
    ]

    assert_raises(StateMachines::SnapshotError) do
      @object.restore_state_machines_snapshot(snapshot)
    end

    assert_equal 'parked', @object.state
    assert_equal 'active', @object.alarm_state
  end

  def test_should_restore_all_machines_when_snapshot_is_valid
    @object.restore_state_machines_snapshot([
      { 'machine' => 'state', 'version' => 1, 'value' => 'idling' },
      { 'machine' => 'alarm_state', 'version' => 1, 'value' => 'off' }
    ])

    assert_equal 'idling', @object.state
    assert_equal 'off', @object.alarm_state
  end

  def test_should_validate_migrated_value_against_known_states
    klass = Class.new do
      attr_accessor :state

      state_machine :state, initial: :parked, version: 2 do
        migrate_version(1) { |_value| 'no_such_state' }

        event :ignite do
          transition parked: :idling
        end
      end
    end

    object = klass.new
    assert_raises(StateMachines::SnapshotError) do
      object.restore_state_machines_snapshot([{ 'machine' => 'state', 'version' => 1, 'value' => 'parked' }])
    end
    assert_equal 'parked', object.state
  end

  def test_should_reject_invalid_version_registration
    machine = StateMachines::Machine.new(@klass, :other_state, initial: :one)

    assert_raises(ArgumentError) { machine.version = 0 }
    assert_raises(ArgumentError) { machine.version = '2' }
    assert_raises(ArgumentError) { machine.migrate_version(1) }
    assert_raises(ArgumentError) { machine.migrate_version(0) { |value| value } }

    machine.migrate_version(1) { |value| value }
    assert_raises(ArgumentError) { machine.migrate_version(1) { |value| value } }
  end

  def test_should_not_share_migrations_with_copied_machines
    machine = StateMachines::Machine.new(@klass, :copy_state, initial: :one, version: 3)
    machine.state :two, :three
    machine.migrate_version(1) { |value| value }

    copy = machine.clone
    copy.migrate_version(2) { |value| value }

    entry = { 'machine' => 'copy_state', 'version' => 2, 'value' => 'one' }
    assert_equal 'one', copy.resolve_snapshot(@object, entry)
    assert_raises(StateMachines::SnapshotError) { machine.resolve_snapshot(@object, entry) }
  end
end
