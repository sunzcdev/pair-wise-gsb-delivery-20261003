# frozen_string_literal: true

require 'test_helper'

class MachineSnapshotTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass, :state, initial: :parked)
    @machine.event :ignite do
      transition parked: :idling
    end
    @object = @klass.new
  end

  def test_should_default_to_version_1
    assert_equal 1, @machine.version
  end

  def test_should_allow_custom_version
    machine = StateMachines::Machine.new(@klass, :state, version: 2)
    assert_equal 2, machine.version
  end

  def test_should_raise_if_version_is_not_a_positive_integer
    assert_raises(ArgumentError) { StateMachines::Machine.new(@klass, :state, version: 0) }
    assert_raises(ArgumentError) { StateMachines::Machine.new(@klass, :state, version: '2') }
  end

  def test_should_snapshot_version_and_value
    assert_equal({ 'version' => 1, 'value' => 'parked' }, @machine.snapshot(@object))
  end

  def test_should_allow_migration_between_adjacent_versions
    @machine.migrate(1) { |value| value }
    @machine.migrate(2, 3) { |value| value }
  end

  def test_should_raise_if_migration_is_not_between_adjacent_versions
    assert_raises(ArgumentError) { @machine.migrate(1, 3) { |value| value } }
    assert_raises(ArgumentError) { @machine.migrate(2, 1) { |value| value } }
  end

  def test_should_raise_if_migration_has_no_block
    assert_raises(ArgumentError) { @machine.migrate(1) }
  end

  def test_should_restore_value_for_current_version
    assert_equal 'idling', @machine.restore_value('version' => 1, 'value' => 'idling')
  end

  def test_should_apply_migration_chain
    machine = StateMachines::Machine.new(Class.new, :state, initial: :stopped, version: 3)
    machine.event :ignite do
      transition stopped: :idling
    end
    machine.migrate(1) { |value| value == 'parked' ? 'standing' : value }
    machine.migrate(2) { |value| value == 'standing' ? 'stopped' : value }

    assert_equal 'stopped', machine.restore_value('version' => 1, 'value' => 'parked')
    assert_equal 'stopped', machine.restore_value('version' => 2, 'value' => 'standing')
    assert_equal 'stopped', machine.restore_value('version' => 3, 'value' => 'stopped')
  end

  def test_should_raise_if_snapshot_version_is_newer_than_definition
    assert_raises(StateMachines::InvalidSnapshot) do
      @machine.restore_value('version' => 2, 'value' => 'parked')
    end
  end

  def test_should_raise_if_snapshot_version_is_invalid
    assert_raises(StateMachines::InvalidSnapshot) do
      @machine.restore_value('version' => '1', 'value' => 'parked')
    end
  end

  def test_should_raise_if_snapshot_entry_is_malformed
    assert_raises(StateMachines::InvalidSnapshot) { @machine.restore_value('value' => 'parked') }
    assert_raises(StateMachines::InvalidSnapshot) { @machine.restore_value('parked') }
  end

  def test_should_raise_if_value_is_unknown
    assert_raises(StateMachines::InvalidSnapshot) do
      @machine.restore_value('version' => 1, 'value' => 'invalid')
    end
  end

  def test_should_match_custom_state_values
    @machine.state :idling, value: 'IDLING'
    assert_equal 'IDLING', @machine.restore_value('version' => 1, 'value' => 'IDLING')
  end
end
