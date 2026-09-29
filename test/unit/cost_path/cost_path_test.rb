# frozen_string_literal: true

require 'test_helper'

class CostPathTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
    @machine.state :parked, :idling
    @machine.event :ignite do
      transition parked: :idling
    end
    @object = @klass.new
    @object.state = 'parked'
  end

  def test_should_raise_exception_if_invalid_option_specified
    exception = assert_raises(ArgumentError) { StateMachines::CostPath.new(@object, @machine, to: :idling, invalid: true) }
    assert_equal 'Unknown key: :invalid. Valid keys are: :from, :to', exception.message
  end

  def test_should_raise_exception_if_to_state_not_specified
    exception = assert_raises(ArgumentError) { StateMachines::CostPath.new(@object, @machine) }
    assert_equal 'Must specify :to state', exception.message
  end

  def test_should_raise_exception_if_invalid_from_state_specified
    exception = assert_raises(IndexError) { StateMachines::CostPath.new(@object, @machine, from: :invalid, to: :idling) }
    assert_equal ':invalid is an invalid name', exception.message
  end

  def test_should_raise_exception_if_invalid_to_state_specified
    exception = assert_raises(IndexError) { StateMachines::CostPath.new(@object, @machine, to: :invalid) }
    assert_equal ':invalid is an invalid name', exception.message
  end

  def test_should_default_from_state_to_current_state
    path = StateMachines::CostPath.new(@object, @machine, to: :idling)

    assert_equal :parked, path.from_name
  end

  def test_should_have_zero_cost_path_when_from_equals_to
    path = StateMachines::CostPath.new(@object, @machine, to: :parked)

    assert_equal 0, path.cost
    assert_empty path.transitions
    assert path.reachable?
  end

  def test_should_be_enumerable_over_transitions
    path = StateMachines::CostPath.new(@object, @machine, to: :idling)

    assert_equal [:ignite], path.map(&:event)
  end

  def test_should_be_available_through_machine_helper
    path = @machine.cost_path_for(@object, to: :idling)

    assert_instance_of StateMachines::CostPath, path
    assert_equal 1, path.cost
    assert_equal [:ignite], path.events
  end

  def test_should_have_object_and_machine
    path = StateMachines::CostPath.new(@object, @machine, to: :idling)

    assert_equal @object, path.object
    assert_equal @machine, path.machine
    assert_equal :idling, path.to_name
  end
end
