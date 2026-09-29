# frozen_string_literal: true

require 'test_helper'

class TransitionWithCompensationsOnFailureTest < StateMachinesTest
  def setup
    @log = []

    @klass = Class.new do
      def save
        false
      end
    end

    @machine = StateMachines::Machine.new(@klass, initial: :parked, action: :save)
    @machine.state :idling
    @machine.event :ignite

    @machine.before_transition do |_object, transition|
      @log << :prepare_a
      transition.register_compensation(:comp_a) { @log << :comp_a }
    end
    @machine.before_transition do |_object, transition|
      @log << :prepare_b
      transition.register_compensation(:comp_b) { @log << :comp_b }
    end

    @object = @klass.new
    @transition = StateMachines::Transition.new(@object, @machine, :ignite, :parked, :idling)
    @result = @transition.perform
  end

  def test_should_not_succeed
    refute @result
  end

  def test_should_run_compensations_in_reverse_order
    assert_equal %i[prepare_a prepare_b comp_b comp_a], @log
  end

  def test_should_roll_back_state
    assert_equal 'parked', @object.state
  end

  def test_should_drain_compensations
    assert_empty @transition.compensations
  end

  def test_should_not_collect_compensation_errors
    assert_empty @transition.compensation_errors
  end
end
