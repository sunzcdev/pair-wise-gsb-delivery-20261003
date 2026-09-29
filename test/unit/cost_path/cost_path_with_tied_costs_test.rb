# frozen_string_literal: true

require 'test_helper'

# Ties between equal-cost paths are broken by a stable rule: fewest
# transitions first, then the lexicographic order of the event names.
class CostPathWithTiedCostsTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
    @machine.state :parked, :idling, :first_gear
    @object = @klass.new
    @object.state = 'parked'
  end

  def test_should_prefer_fewer_transitions_on_cost_tie
    @machine.event :zip, cost: 2 do
      transition parked: :first_gear
    end
    @machine.event :alpha, cost: 1 do
      transition parked: :idling
    end
    @machine.event :beta, cost: 1 do
      transition idling: :first_gear
    end

    path = StateMachines::CostPath.new(@object, @machine, to: :first_gear)

    assert_equal 2, path.cost
    assert_equal [:zip], path.events
  end

  def test_should_prefer_lexicographically_first_event_sequence_on_full_tie
    @machine.event :gamma, cost: 1 do
      transition parked: :idling
    end
    @machine.event :delta, cost: 1 do
      transition parked: :idling
    end

    path = StateMachines::CostPath.new(@object, @machine, to: :idling)

    assert_equal 1, path.cost
    assert_equal [:delta], path.events
  end

  def test_should_select_same_path_on_repeated_queries
    @machine.event :gamma, cost: 1 do
      transition parked: :idling
    end
    @machine.event :delta, cost: 1 do
      transition parked: :idling
    end

    first = StateMachines::CostPath.new(@object, @machine, to: :idling).events
    second = StateMachines::CostPath.new(@object, @machine, to: :idling).events

    assert_equal first, second
  end
end
