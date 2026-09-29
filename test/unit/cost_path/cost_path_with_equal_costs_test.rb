# frozen_string_literal: true

require 'test_helper'

# Equal-cost paths must be resolved deterministically: fewest transitions
# first, then the lexicographically smallest sequence of event names,
# regardless of the order in which events were defined.
class CostPathWithEqualCostsTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
    @machine.state :parked, :charging, :idling, :first_gear
    @machine.event :cruise, cost: 1 do
      transition parked: :idling
    end
    @machine.event :cruise_up, cost: 1 do
      transition idling: :first_gear
    end
    @machine.event :boost, cost: 1 do
      transition parked: :charging
    end
    @machine.event :boost_up, cost: 1 do
      transition charging: :first_gear
    end
    @object = @klass.new
    @object.state = 'parked'

    @path = StateMachines::CostPath.new(@object, @machine, to: :first_gear)
  end

  def test_should_choose_lexicographically_smallest_event_sequence
    assert_equal 2, @path.cost
    assert_equal %i[boost boost_up], @path.events
  end

  def test_should_be_stable_across_searches
    10.times do
      path = StateMachines::CostPath.new(@object, @machine, to: :first_gear)
      assert_equal @path.events, path.events
      assert_equal @path.cost, path.cost
    end
  end

  def test_should_prefer_fewer_transitions_when_costs_are_tied
    @machine.event :jump, cost: 2 do
      transition parked: :first_gear
    end

    path = StateMachines::CostPath.new(@object, @machine, to: :first_gear)
    assert_equal 2, path.cost
    assert_equal %i[jump], path.events
  end
end
