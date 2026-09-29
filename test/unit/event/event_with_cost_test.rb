# frozen_string_literal: true

require 'test_helper'

class EventWithCostTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
  end

  def test_should_default_to_zero
    event = StateMachines::Event.new(@machine, :ignite)
    assert_equal 0, event.cost
  end

  def test_should_allow_cost_keyword
    event = StateMachines::Event.new(@machine, :ignite, cost: 2)
    assert_equal 2, event.cost
  end

  def test_should_allow_cost_in_options_hash
    event = StateMachines::Event.new(@machine, :ignite, { cost: 3 })
    assert_equal 3, event.cost
  end

  def test_should_allow_cost_through_machine_dsl
    event = @machine.event :ignite, cost: 4
    assert_equal 4, event.cost
  end

  def test_should_allow_fractional_costs
    event = @machine.event :ignite, cost: 1.5
    assert_equal 1.5, event.cost
  end

  def test_should_not_allow_negative_costs
    exception = assert_raises(ArgumentError) { @machine.event :ignite, cost: -1 }
    assert_equal 'cost must be a non-negative number, got -1', exception.message
  end

  def test_should_not_allow_non_numeric_costs
    exception = assert_raises(ArgumentError) { @machine.event :ignite, cost: 'high' }
    assert_equal 'cost must be a non-negative number, got "high"', exception.message
  end

  def test_should_expose_cost_on_transitions
    @machine.event :ignite, cost: 2 do
      transition parked: :idling
    end

    object = @klass.new
    transition = StateMachines::Transition.new(object, @machine, :ignite, :parked, :idling)
    assert_equal 2, transition.cost
  end
end
