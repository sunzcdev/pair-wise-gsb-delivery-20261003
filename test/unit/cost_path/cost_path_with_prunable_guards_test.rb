# frozen_string_literal: true

require 'test_helper'

# Guard conditions that can be safely evaluated without running object code
# (literal conditionals and state guards) must participate in pruning.
class CostPathWithPrunableGuardsTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
    @machine.state :parked, :idling
    @object = @klass.new
    @object.state = 'parked'
  end

  def test_should_prune_branches_with_false_if_condition
    @machine.event :ignite do
      transition parked: :idling, if: false
    end

    path = StateMachines::CostPath.new(@object, @machine, to: :idling)
    assert_equal :unreachable, path.status
  end

  def test_should_prune_branches_with_true_unless_condition
    @machine.event :ignite do
      transition parked: :idling, unless: true
    end

    path = StateMachines::CostPath.new(@object, @machine, to: :idling)
    assert_equal :unreachable, path.status
  end

  def test_should_keep_branches_with_true_if_condition
    @machine.event :ignite do
      transition parked: :idling, if: true
    end

    path = StateMachines::CostPath.new(@object, @machine, to: :idling)
    assert_equal :reachable, path.status
    assert_equal 1, path.guards.length
    guard = path.guards.first
    assert_equal :ignite, guard.event
    assert_equal :if, guard.type
    assert_equal true, guard.condition
    assert_equal :satisfied, guard.verdict
  end

  def test_should_evaluate_state_guards_against_the_state_being_explored
    @machine.event :ignite do
      transition parked: :idling
    end
    @machine.event :shift_up do
      transition idling: :first_gear, if_state: { state: :parked }
    end

    path = StateMachines::CostPath.new(@object, @machine, to: :first_gear)
    assert_equal :unreachable, path.status
  end

  def test_should_allow_state_guards_that_match_the_state_being_explored
    @machine.event :ignite do
      transition parked: :idling
    end
    @machine.event :shift_up do
      transition idling: :first_gear, if_state: { state: :idling }
    end

    path = StateMachines::CostPath.new(@object, @machine, to: :first_gear)
    assert_equal :reachable, path.status
    assert_equal %i[ignite shift_up], path.events
    assert path.guards.any? { |guard| guard.type == :if_state && guard.verdict == :satisfied }
  end
end
