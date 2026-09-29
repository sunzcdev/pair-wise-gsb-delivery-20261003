# frozen_string_literal: true

require 'test_helper'

class CostPathWithUnreachableTargetTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
    @machine.state :parked, :idling, :scrapyard
    @machine.event :ignite do
      transition parked: :idling
    end
    @object = @klass.new
    @object.state = 'parked'

    @path = StateMachines::CostPath.new(@object, @machine, to: :scrapyard)
  end

  def test_should_have_unreachable_status
    assert_equal :unreachable, @path.status
    assert @path.unreachable?
    refute @path.reachable?
    refute @path.pending?
  end

  def test_should_not_have_a_cost
    assert_nil @path.cost
  end

  def test_should_not_have_any_transitions
    assert_empty @path.transitions
    assert_empty @path.events
  end

  def test_should_not_have_any_guards
    assert_empty @path.guards
    assert_empty @path.pending_guards
  end
end
