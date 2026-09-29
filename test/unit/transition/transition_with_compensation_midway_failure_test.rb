# frozen_string_literal: true

require 'test_helper'

class TransitionWithCompensationMidwayFailureTest < StateMachinesTest
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
      @log << :prepare_b
      raise 'prepare_b failed'
    end

    @object = @klass.new
    @transition = StateMachines::Transition.new(@object, @machine, :ignite, :parked, :idling)

    begin
      @transition.perform
      @raised = nil
    rescue RuntimeError => e
      @raised = e
    end
  end

  def test_should_reraise_original_error
    assert_equal 'prepare_b failed', @raised&.message
  end

  def test_should_only_compensate_completed_preparations
    assert_equal %i[prepare_a prepare_b comp_a], @log
  end

  def test_should_not_change_state
    assert_equal 'parked', @object.state
  end

  def test_should_drain_compensations
    assert_empty @transition.compensations
  end

  def test_should_not_collect_compensation_errors
    assert_empty @transition.compensation_errors
  end
end
