# frozen_string_literal: true

require 'test_helper'

# The cheapest route relies on a guard condition that cannot be safely
# evaluated without running object code, so it must be marked as pending
# confirmation instead of being pruned or silently trusted.
class CostPathWithUnknownGuardsTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
    @machine.state :parked, :idling, :first_gear
    @machine.event :ignite, cost: 3 do
      transition parked: :idling
    end
    @machine.event :shift_up, cost: 3 do
      transition idling: :first_gear
    end
    @machine.event :turbo, cost: 1 do
      transition parked: :first_gear, if: :ready?
    end
    @object = @klass.new
    @object.state = 'parked'

    @path = StateMachines::CostPath.new(@object, @machine, to: :first_gear)
  end

  def test_should_be_pending_confirmation
    assert_equal :pending, @path.status
    assert @path.pending?
    refute @path.reachable?
    refute @path.unreachable?
  end

  def test_should_find_the_lowest_cost_path_through_the_unknown_guard
    assert_equal 1, @path.cost
    assert_equal %i[turbo], @path.events
  end

  def test_should_not_run_the_guard_condition_on_the_object
    assert_raises(NoMethodError) { @object.ready? }
    assert_equal :pending, @path.status
  end

  def test_should_expose_the_guards_along_the_path
    assert_equal 1, @path.guards.length
    guard = @path.guards.first
    assert_equal :turbo, guard.event
    assert_equal :if, guard.type
    assert_equal :ready?, guard.condition
    assert_equal :unknown, guard.verdict
    assert guard.pending?
  end

  def test_should_list_unverified_guards_as_pending
    assert_equal @path.guards, @path.pending_guards
  end

  def test_should_prefer_verified_path_when_costs_are_tied
    machine = StateMachines::Machine.new(Class.new)
    machine.state :parked, :idling
    machine.event :ignite, cost: 1 do
      transition parked: :idling
    end
    machine.event :turbo, cost: 1 do
      transition parked: :idling, if: :ready?
    end
    object = machine.owner_class.new
    object.state = 'parked'

    path = StateMachines::CostPath.new(object, machine, to: :idling)
    assert_equal :reachable, path.status
    assert_equal %i[ignite], path.events
  end
end
