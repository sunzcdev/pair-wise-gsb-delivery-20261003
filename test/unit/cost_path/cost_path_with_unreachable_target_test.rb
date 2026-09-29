# frozen_string_literal: true

require 'test_helper'

class CostPathWithUnreachableTargetTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
    @machine.state :parked, :idling, :scrap
    @machine.event :ignite do
      transition parked: :idling
    end
    @object = @klass.new
    @object.state = 'parked'

    @path = StateMachines::CostPath.new(@object, @machine, to: :scrap)
  end

  def test_should_be_unreachable
    assert_equal :unreachable, @path.status
    assert @path.unreachable?
    refute @path.reachable?
    refute @path.pending?
  end

  def test_should_have_no_cost
    assert_nil @path.cost
  end

  def test_should_have_no_transitions
    assert_empty @path
    assert_empty @path.events
  end

  def test_should_have_no_guards
    assert_empty @path.guards
    assert_empty @path.pending_guards
  end
end
