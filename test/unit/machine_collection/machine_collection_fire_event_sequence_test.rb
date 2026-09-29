# frozen_string_literal: true

require 'test_helper'

class MachineCollectionFireEventSequenceTest < StateMachinesTest
  def setup
    @machines = StateMachines::MachineCollection.new

    @klass = Class.new do
      attr_accessor :fail_save
      attr_reader :saves

      def save
        @saves = (@saves || 0) + 1
        !fail_save
      end
    end

    # First machine
    @machines[:state] = @state = StateMachines::Machine.new(@klass, :state, initial: :parked, action: :save)
    @state.event :ignite do
      transition parked: :idling
    end
    @state.event :shift_up do
      transition idling: :first_gear
    end
    @state.event :shift_down do
      transition first_gear: :idling
    end

    # Second machine
    @machines[:alarm_state] = @alarm_state = StateMachines::Machine.new(@klass, :alarm_state, initial: :active, action: :save, namespace: 'alarm')
    @alarm_state.event :disable do
      transition active: :off
    end

    @object = @klass.new
  end

  def test_should_raise_exception_if_no_events_specified
    assert_raises(ArgumentError) { @machines.fire_event_sequence(@object) }
  end

  def test_should_raise_exception_if_invalid_event_specified
    exception = assert_raises(StateMachines::InvalidEvent) { @machines.fire_event_sequence(@object, :invalid) }
    assert_equal :invalid, exception.event

    exception = assert_raises(StateMachines::InvalidEvent) { @machines.fire_event_sequence(@object, :ignite, :invalid) }
    assert_equal :invalid, exception.event

    assert_equal 'parked', @object.state
    assert_nil @object.saves
  end

  def test_should_run_events_in_sequence_with_each_seeing_previous_state
    assert @machines.fire_event_sequence(@object, :ignite, :shift_up)
    assert_equal 'first_gear', @object.state
    assert_equal 1, @object.saves
  end

  def test_should_run_events_across_machines
    assert @machines.fire_event_sequence(@object, :disable_alarm, :ignite)
    assert_equal 'idling', @object.state
    assert_equal 'off', @object.alarm_state
    assert_equal 1, @object.saves
  end

  def test_should_not_run_later_events_and_should_restore_state_when_middle_event_fails
    ran = []
    @state.after_transition { |_object, transition| ran << transition.event }

    @object.state_event = 'ignite'
    @state.before_transition(on: :ignite) { @object.state_event = 'shift_up' }

    exception = assert_raises(StateMachines::InvalidEventSequence) do
      @machines.fire_event_sequence(@object, :ignite, :shift_down, :shift_up)
    end

    # Failure is attributed to the second event
    assert_equal @object, exception.object
    assert_equal %i[ignite shift_down shift_up], exception.events
    assert_equal :shift_down, exception.failed_event
    assert_equal 1, exception.failed_index
    assert_equal %i[success failed pending], exception.steps.map { |step| step[:status] }

    # The third event never ran
    assert_equal %i[ignite], ran

    # State and staged event attribute were restored, nothing was committed
    assert_equal 'parked', @object.state
    assert_equal :ignite, @object.state_event
    assert_nil @object.saves
  end

  def test_should_restore_all_machines_when_any_event_fails
    exception = assert_raises(StateMachines::InvalidEventSequence) do
      @machines.fire_event_sequence(@object, :disable_alarm, :ignite, :shift_down)
    end

    assert_equal :shift_down, exception.failed_event
    assert_equal 'parked', @object.state
    assert_equal 'active', @object.alarm_state
    assert_nil @object.saves
  end

  def test_should_run_failure_callbacks_for_failed_event
    @shift_down_failure_run = false
    @machines[:state].after_failure(on: :shift_down) { @shift_down_failure_run = true }

    assert_raises(StateMachines::InvalidEventSequence) do
      @machines.fire_event_sequence(@object, :ignite, :shift_down)
    end
    assert @shift_down_failure_run
  end

  def test_should_restore_state_if_commit_fails
    @object.fail_save = true

    exception = assert_raises(StateMachines::InvalidEventSequence) do
      @machines.fire_event_sequence(@object, :ignite, :shift_up)
    end

    assert_nil exception.failed_event
    assert_nil exception.failed_index
    assert_equal 'parked', @object.state
  end

  def test_should_raise_exception_if_nested_within_another_sequence
    nested = false
    object = @object
    @state.before_transition(on: :ignite) do
      unless nested
        nested = true
        object.fire_event_sequence(:shift_up)
      end
    end

    exception = assert_raises(StateMachines::InvalidContext) do
      @object.fire_event_sequence(:ignite)
    end
    assert_equal @object, exception.object
    assert_equal 'parked', @object.state

    # The guard is released after the failure, so sequences can run again
    assert @object.fire_event_sequence(:ignite, :shift_up)
    assert_equal 'first_gear', @object.state
  end

  def test_should_allow_caller_to_provide_transaction_boundary
    boundary = []
    result = @machines.fire_event_sequence(@object, :ignite, :shift_up) do |sequence|
      boundary << :begin
      sequence.call.tap { boundary << :commit }
    end

    assert result
    assert_equal %i[begin commit], boundary
    assert_equal 'first_gear', @object.state
    assert_equal 1, @object.saves
  end

  def test_should_propagate_failure_through_transaction_boundary
    rolled_back = false

    assert_raises(StateMachines::InvalidEventSequence) do
      @machines.fire_event_sequence(@object, :ignite, :shift_down) do |sequence|
        begin
          sequence.call
        rescue StandardError
          rolled_back = true
          raise
        end
      end
    end

    assert rolled_back
    assert_equal 'parked', @object.state
    assert_nil @object.saves
  end

  def test_should_be_available_as_instance_method
    assert @object.fire_event_sequence(:ignite, :shift_up)
    assert_equal 'first_gear', @object.state
  end
end
