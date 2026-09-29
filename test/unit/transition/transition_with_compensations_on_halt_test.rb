# frozen_string_literal: true

require 'test_helper'

class TransitionWithCompensationsOnHaltTest < StateMachinesTest
  def setup
    @log = []

    @klass = Class.new do
      def save
        true
      end
    end

    @machine = StateMachines::Machine.new(@klass, initial: :parked, action: :save)
    @machine.state :idling
    @machine.event :ignite

    @machine.before_transition do |_object, transition|
      @log << :prepare_a
      transition.register_compensation(:comp_a) { @log << :comp_a }
    end
    @machine.before_transition do
      @log << :halted
      throw :halt
    end

    @object = @klass.new
    @transition = StateMachines::Transition.new(@object, @machine, :ignite, :parked, :idling)
    @result = @transition.perform
  end

  def test_should_not_succeed
    refute @result
  end

  def test_should_run_compensations_for_completed_preparations
    assert_equal %i[prepare_a halted comp_a], @log
  end

  def test_should_not_change_state
    assert_equal 'parked', @object.state
  end

  def test_should_not_collect_compensation_errors
    assert_empty @transition.compensation_errors
  end
end
