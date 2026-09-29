# frozen_string_literal: true

require 'test_helper'

class TransitionWithCompensationsTest < StateMachinesTest
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
    @machine.before_transition do |_object, transition|
      @log << :prepare_b
      transition.register_compensation(:comp_b) { @log << :comp_b }
    end

    @object = @klass.new
    @transition = StateMachines::Transition.new(@object, @machine, :ignite, :parked, :idling)
  end

  def test_should_require_a_block
    assert_raises(ArgumentError) { @transition.register_compensation(:no_block) }
  end

  def test_should_register_compensations_in_order
    @transition.perform

    assert_equal %i[comp_a comp_b], @transition.compensations.map(&:name)
  end

  def test_should_not_run_compensations_when_perform_succeeds
    @transition.perform

    assert_equal %i[prepare_a prepare_b], @log
  end

  def test_should_not_collect_compensation_errors_when_perform_succeeds
    @transition.perform

    assert_empty @transition.compensation_errors
  end

  def test_should_evaluate_compensation_with_object_and_transition
    received = nil
    @transition.register_compensation(:probe) { |object, transition| received = [object, transition] }
    @transition.rollback

    assert_equal [@object, @transition], received
  end

  def test_should_discard_pending_compensations_on_reset
    @transition.register_compensation(:comp) { @log << :comp }
    @transition.reset

    assert_empty @transition.compensations
  end
end
