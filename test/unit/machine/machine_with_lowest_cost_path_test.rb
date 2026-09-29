# frozen_string_literal: true

require 'test_helper'

class MachineWithLowestCostPathTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
    @machine.event :ignite, cost: 1 do
      transition parked: :idling
    end
    @machine.event :shift_up, cost: 2 do
      transition idling: :first_gear
    end

    @object = @klass.new
    @object.state = 'parked'
  end

  def test_should_find_lowest_cost_path
    path = @machine.lowest_cost_path_for(@object, to: :first_gear)
    assert_equal :reachable, path.status
    assert_equal 3, path.cost
    assert_equal %i[ignite shift_up], path.events
  end

  def test_should_allow_custom_from_state
    path = @machine.lowest_cost_path_for(@object, from: :idling, to: :first_gear)
    assert_equal 2, path.cost
    assert_equal %i[shift_up], path.events
  end
end
