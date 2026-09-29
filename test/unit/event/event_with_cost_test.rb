# frozen_string_literal: true

require 'test_helper'

class EventWithCostTest < StateMachinesTest
  def setup
    @klass = Class.new
    @machine = StateMachines::Machine.new(@klass)
  end

  def test_should_default_to_one
    event = StateMachines::Event.new(@machine, :ignite)

    assert_equal 1, event.cost
  end

  def test_should_set_cost_from_options
    event = StateMachines::Event.new(@machine, :ignite, { cost: 2 })

    assert_equal 2, event.cost
  end

  def test_should_set_cost_from_kwargs
    event = StateMachines::Event.new(@machine, :ignite, nil, cost: 2)

    assert_equal 2, event.cost
  end

  def test_should_set_cost_from_machine_dsl
    event = @machine.event(:ignite, cost: 3)

    assert_equal 3, event.cost
  end

  def test_should_allow_zero_cost
    event = StateMachines::Event.new(@machine, :ignite, cost: 0)

    assert_equal 0, event.cost
  end

  def test_should_allow_fractional_cost
    event = StateMachines::Event.new(@machine, :ignite, cost: 1.5)

    assert_equal 1.5, event.cost
  end

  def test_should_raise_exception_if_cost_is_negative
    exception = assert_raises(ArgumentError) { StateMachines::Event.new(@machine, :ignite, cost: -1) }
    assert_equal 'cost must be a non-negative number, got -1', exception.message
  end

  def test_should_raise_exception_if_cost_is_not_a_number
    exception = assert_raises(ArgumentError) { StateMachines::Event.new(@machine, :ignite, cost: '2') }
    assert_equal 'cost must be a non-negative number, got "2"', exception.message
  end

  def test_should_update_cost_through_writer
    event = StateMachines::Event.new(@machine, :ignite)
    event.cost = 5

    assert_equal 5, event.cost
  end

  def test_should_raise_exception_if_updated_cost_is_negative
    event = StateMachines::Event.new(@machine, :ignite)

    assert_raises(ArgumentError) { event.cost = -1 }
  end
end
