# frozen_string_literal: true

require 'test_helper'

class TransitionWithCompensationErrorsTest < StateMachinesTest
  def setup
    @log = []

    @klass = Class.new do
      def save
        raise ArgumentError, 'save failed'
      end
    end

    @machine = StateMachines::Machine.new(@klass, initial: :parked, action: :save)
    @machine.state :idling
    @machine.event :ignite

    @machine.before_transition do |_object, transition|
      @log << :prepare_a
      transition.register_compensation(:comp_a) do
        @log << :comp_a
        raise 'comp_a failed'
      end
    end
    @machine.before_transition do |_object, transition|
      @log << :prepare_b
      transition.register_compensation(:comp_b) do
        @log << :comp_b
        raise 'comp_b failed'
      end
    end

    @object = @klass.new
    @transition = StateMachines::Transition.new(@object, @machine, :ignite, :parked, :idling)

    begin
      @transition.perform
      @raised = nil
    rescue ArgumentError => e
      @raised = e
    end
  end

  def test_should_preserve_original_failure_reason
    assert_equal 'save failed', @raised&.message
  end

  def test_should_run_all_compensations_in_reverse_order
    assert_equal %i[prepare_a prepare_b comp_b comp_a], @log
  end

  def test_should_collect_compensation_errors_separately
    assert_equal ['comp_b failed', 'comp_a failed'], @transition.compensation_errors.map(&:message)
  end

  def test_should_roll_back_state
    assert_equal 'parked', @object.state
  end

  def test_should_drain_compensations
    assert_empty @transition.compensations
  end
end
