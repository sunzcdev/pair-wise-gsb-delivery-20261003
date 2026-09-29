# frozen_string_literal: true

require 'test_helper'

# Graph where the fewest-steps answer differs from the lowest-cost answer:
#
#   parked  --ignite(1)-->  idling --shift_up(1)--> first_gear   (2 steps, cost 2)
#   parked  --------turbo(5)--------> first_gear                 (1 step,  cost 5)
class CostPathWithWeightedEventsTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
    @machine.state :parked, :idling, :first_gear
    @machine.event :ignite, cost: 1 do
      transition parked: :idling
    end
    @machine.event :shift_up, cost: 1 do
      transition idling: :first_gear
    end
    @machine.event :turbo, cost: 5 do
      transition parked: :first_gear
    end
    @object = @klass.new
    @object.state = 'parked'

    @path = StateMachines::CostPath.new(@object, @machine, to: :first_gear)
  end

  def test_should_find_cheapest_path_instead_of_fewest_steps
    # The fewest-steps answer would be [:turbo] (1 step, cost 5)
    fewest_steps = @machine.paths_for(@object, to: :first_gear).map(&:length).min
    assert_equal 1, fewest_steps

    # The lowest-cost answer takes 2 steps at a total cost of 2
    assert_equal 2, @path.cost
    assert_equal %i[ignite shift_up], @path.events
  end

  def test_should_have_reachable_status
    assert_equal :reachable, @path.status
    assert @path.reachable?
    refute @path.pending?
    refute @path.unreachable?
  end

  def test_should_list_transitions_in_order
    assert_equal [
      StateMachines::Transition.new(@object, @machine, :ignite, :parked, :idling),
      StateMachines::Transition.new(@object, @machine, :shift_up, :idling, :first_gear)
    ], @path.transitions
  end

  def test_should_have_no_guards
    assert_empty @path.guards
    assert_empty @path.pending_guards
  end

  def test_should_prefer_cheaper_direct_event_when_costs_are_reversed
    @machine.event(:turbo).cost = 1
    @machine.event(:ignite).cost = 3

    path = StateMachines::CostPath.new(@object, @machine, to: :first_gear)

    assert_equal 1, path.cost
    assert_equal [:turbo], path.events
  end
end
