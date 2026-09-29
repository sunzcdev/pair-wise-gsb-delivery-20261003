# frozen_string_literal: true

require 'test_helper'

# Zero-cost cycles must not send the search into an infinite loop: a cycle
# can never improve a path because costs are non-negative and the extra
# steps only make the tie-breaking key worse.
class CostPathWithZeroCostCycleTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
    @machine.state :parked, :idling, :first_gear, :second_gear
    @machine.event :ignite, cost: 0 do
      transition parked: :idling
    end
    @machine.event :park, cost: 0 do
      transition idling: :parked
    end
    @machine.event :shift_up, cost: 1 do
      transition idling: :first_gear
    end
    @object = @klass.new
    @object.state = 'parked'

    @path = StateMachines::CostPath.new(@object, @machine, to: :first_gear)
  end

  def test_should_terminate_and_find_the_lowest_cost_path
    assert_equal :reachable, @path.status
    assert_equal 1, @path.cost
    assert_equal %i[ignite shift_up], @path.events
  end

  def test_should_not_include_cycle_detours_in_the_path
    assert_equal 2, @path.length
  end

  def test_should_terminate_when_target_is_unreachable_from_a_cycle
    path = StateMachines::CostPath.new(@object, @machine, to: :second_gear)
    assert_equal :unreachable, path.status
  end
end
