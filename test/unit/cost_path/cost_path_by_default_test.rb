# frozen_string_literal: true

require 'test_helper'

class CostPathByDefaultTest < StateMachinesTest
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

  def test_should_require_a_target_state
    exception = assert_raises(ArgumentError) { StateMachines::CostPath.new(@object, @machine) }
    assert_equal 'Must specify a :to target state', exception.message
  end

  def test_should_not_allow_invalid_keys
    assert_raises(ArgumentError) { StateMachines::CostPath.new(@object, @machine, to: :idling, invalid: true) }
  end

  def test_should_start_from_the_objects_current_state
    path = StateMachines::CostPath.new(@object, @machine, to: :idling)
    assert_equal :parked, path.from_name
  end

  def test_should_raise_exception_for_invalid_from_state
    assert_raises(IndexError) { StateMachines::CostPath.new(@object, @machine, from: :invalid, to: :idling) }
  end

  def test_should_raise_exception_for_invalid_to_state
    assert_raises(IndexError) { StateMachines::CostPath.new(@object, @machine, to: :invalid) }
  end

  def test_should_find_empty_path_when_already_at_target
    path = StateMachines::CostPath.new(@object, @machine, to: :parked)
    assert_equal :reachable, path.status
    assert_equal 0, path.cost
    assert_empty path
  end
end
