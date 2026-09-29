# frozen_string_literal: true

require 'test_helper'

# Application-defined :if / :unless conditions cannot be evaluated without
# running arbitrary code, so transitions using them are kept as candidates
# and their guards are flagged as pending ("to be confirmed").
class CostPathWithUnevaluableGuardsTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
    @machine.state :parked, :scrapyard
    @machine.event :tow, cost: 1 do
      transition parked: :scrapyard, if: :hooked_up?
    end
    @object = @klass.new
    @object.state = 'parked'

    @path = StateMachines::CostPath.new(@object, @machine, to: :scrapyard)
  end

  def test_should_have_pending_status
    assert_equal :pending, @path.status
    assert @path.pending?
    refute @path.reachable?
    refute @path.unreachable?
  end

  def test_should_still_return_candidate_path_and_cost
    assert_equal 1, @path.cost
    assert_equal [:tow], @path.events
  end

  def test_should_flag_if_condition_as_pending
    assert_equal [
      { event: :tow, guard: :if, condition: :hooked_up?, status: :pending }
    ], @path.pending_guards
    assert_equal @path.pending_guards, @path.guards
  end

  def test_should_flag_unless_condition_as_pending
    @machine.event :haul, cost: 0 do
      transition parked: :scrapyard, unless: :blocked?
    end

    path = StateMachines::CostPath.new(@object, @machine, to: :scrapyard)

    assert_equal [
      { event: :haul, guard: :unless, condition: :blocked?, status: :pending }
    ], path.pending_guards
  end

  def test_should_not_evaluate_guard_conditions
    @machine.event :crane, cost: 3 do
      transition parked: :scrapyard, if: -> { raise 'guard must not be evaluated' }
    end

    path = StateMachines::CostPath.new(@object, @machine, to: :scrapyard)

    assert path
  end
end
