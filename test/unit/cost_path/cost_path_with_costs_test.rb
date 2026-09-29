# frozen_string_literal: true

require 'test_helper'

# Graph where the fewest-step answer differs from the lowest-cost answer:
#
#   parked --jump(cost 5)--> first_gear            (1 step,  cost 5)
#   parked --ignite(cost 0)--> idling --shift_up(cost 0)--> first_gear
#                                                       (2 steps, cost 0)
class CostPathWithCostsTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
    @machine.state :parked, :idling, :first_gear
    @machine.event :ignite, cost: 0 do
      transition parked: :idling
    end
    @machine.event :shift_up, cost: 0 do
      transition idling: :first_gear
    end
    @machine.event :jump, cost: 5 do
      transition parked: :first_gear
    end
    @object = @klass.new
    @object.state = 'parked'

    @path = StateMachines::CostPath.new(@object, @machine, to: :first_gear)
  end

  def test_should_be_reachable
    assert_equal :reachable, @path.status
    assert @path.reachable?
    refute @path.pending?
    refute @path.unreachable?
  end

  def test_should_prefer_lowest_cost_over_fewest_steps
    assert_equal [
      StateMachines::Transition.new(@object, @machine, :ignite, :parked, :idling),
      StateMachines::Transition.new(@object, @machine, :shift_up, :idling, :first_gear)
    ], @path
  end

  def test_should_sum_event_costs
    assert_equal 0, @path.cost
  end

  def test_should_list_events_in_order
    assert_equal %i[ignite shift_up], @path.events
  end

  def test_should_have_no_guards
    assert_empty @path.guards
    assert_empty @path.pending_guards
  end

  def test_should_use_more_expensive_path_when_it_is_the_only_option
    machine = StateMachines::Machine.new(Class.new)
    machine.state :parked, :first_gear
    machine.event :jump, cost: 5 do
      transition parked: :first_gear
    end
    object = machine.owner_class.new
    object.state = 'parked'

    path = StateMachines::CostPath.new(object, machine, to: :first_gear)
    assert_equal 5, path.cost
    assert_equal %i[jump], path.events
  end
end
