# frozen_string_literal: true

require 'test_helper'

# Zero-cost cycles must not send the search into an infinite loop: entries
# are only ever replaced by strictly better ones, and every step taken
# strictly increases the path length.
class CostPathWithZeroCostCycleTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
    @machine.state :parked, :idling, :first_gear
    @machine.event :bounce, cost: 0 do
      transition parked: :idling, idling: :parked
    end
    @machine.event :idle, cost: 0 do
      transition idling: same
    end
    @machine.event :shift_up, cost: 3 do
      transition idling: :first_gear
    end
    @object = @klass.new
    @object.state = 'parked'
  end

  def test_should_terminate_and_find_cheapest_path_across_cycle
    path = StateMachines::CostPath.new(@object, @machine, to: :first_gear)

    assert_equal 3, path.cost
    assert_equal %i[bounce shift_up], path.events
  end

  def test_should_find_zero_cost_path
    path = StateMachines::CostPath.new(@object, @machine, to: :idling)

    assert_equal 0, path.cost
    assert_equal [:bounce], path.events
    assert path.reachable?
  end

  def test_should_not_loop_on_zero_cost_self_loop
    path = StateMachines::CostPath.new(@object, @machine, from: :idling, to: :parked)

    assert_equal 0, path.cost
    assert_equal [:bounce], path.events
  end
end
