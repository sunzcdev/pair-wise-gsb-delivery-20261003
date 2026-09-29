# frozen_string_literal: true

require 'test_helper'

class TransitionWithCompensationsTest < StateMachinesTest
  def setup
    @klass = Class.new do
      attr_accessor :save_result, :save_error
      attr_reader :released

      def save
        raise @save_error if @save_error

        @save_result.nil? ? true : @save_result
      end

      def release
        (@released ||= []) << :release
      end
    end

    @machine = StateMachines::Machine.new(@klass, action: :save)
    @machine.state :parked, :idling
    @machine.event :ignite

    @object = @klass.new
    @object.state = 'parked'
    @transition = StateMachines::Transition.new(@object, @machine, :ignite, :parked, :idling)

    # Records resource acquisition and compensation call order
    @log = []
  end

  def register_preparation(transition, name)
    @log << :"prepare_#{name}"
    transition.register_compensation { @log << :"release_#{name}" }
  end

  def test_should_not_run_compensations_when_transition_succeeds
    @machine.before_transition { |_object, transition| register_preparation(transition, :a) }
    @machine.before_transition { |_object, transition| register_preparation(transition, :b) }

    assert @transition.perform
    assert_equal %i[prepare_a prepare_b], @log
    assert_equal 'idling', @object.state
    assert_empty @transition.compensation_errors
  end

  def test_should_run_compensations_in_reverse_order_when_action_fails
    @machine.before_transition { |_object, transition| register_preparation(transition, :a) }
    @machine.before_transition { |_object, transition| register_preparation(transition, :b) }
    @object.save_result = false

    refute @transition.perform
    assert_equal %i[prepare_a prepare_b release_b release_a], @log
    assert_equal 'parked', @object.state
    assert_empty @transition.compensation_errors
  end

  def test_should_run_compensations_when_action_raises
    @machine.before_transition { |_object, transition| register_preparation(transition, :a) }
    @machine.before_transition { |_object, transition| register_preparation(transition, :b) }
    @object.save_error = RuntimeError.new('save failed')

    error = assert_raises(RuntimeError) { @transition.perform }
    assert_equal 'save failed', error.message
    assert_equal %i[prepare_a prepare_b release_b release_a], @log
    assert_equal 'parked', @object.state
  end

  def test_should_only_compensate_preparations_that_completed
    @machine.before_transition { |_object, transition| register_preparation(transition, :a) }
    @machine.before_transition do |_object, _transition|
      @log << :prepare_b
      raise 'prepare b failed'
      # release_b is never registered because prepare_b did not complete
    end

    error = assert_raises(RuntimeError) { @transition.perform }
    assert_equal 'prepare b failed', error.message
    assert_equal %i[prepare_a prepare_b release_a], @log
    assert_empty @transition.compensation_errors
  end

  def test_should_run_compensations_when_before_callback_halts
    @machine.before_transition { |_object, transition| register_preparation(transition, :a) }
    @machine.before_transition { |_object, transition| register_preparation(transition, :b) }
    @machine.before_transition { throw :halt }

    refute @transition.perform
    assert_equal %i[prepare_a prepare_b release_b release_a], @log
  end

  def test_should_run_compensations_when_guard_raises
    @machine.before_transition { |_object, transition| register_preparation(transition, :a) }
    @machine.before_transition(if: ->(_object) { raise 'guard failed' }) { @log << :unreachable }

    error = assert_raises(RuntimeError) { @transition.perform }
    assert_equal 'guard failed', error.message
    assert_equal %i[prepare_a release_a], @log
  end

  def test_should_collect_compensation_errors_and_preserve_original_exception
    @machine.before_transition do |_object, transition|
      register_preparation(transition, :a)
      transition.register_compensation { raise 'release a2 failed' }
    end
    @machine.before_transition do |_object, transition|
      register_preparation(transition, :b)
      transition.register_compensation { raise 'release b2 failed' }
    end
    @object.save_error = RuntimeError.new('save failed')

    error = assert_raises(RuntimeError) { @transition.perform }
    assert_equal 'save failed', error.message
    assert_equal %i[prepare_a prepare_b release_b release_a], @log
    assert_equal ['release b2 failed', 'release a2 failed'], @transition.compensation_errors.map(&:message)
  end

  def test_should_collect_compensation_errors_and_preserve_original_result_when_action_fails
    @machine.before_transition do |_object, transition|
      register_preparation(transition, :a)
      transition.register_compensation { raise 'release a2 failed' }
    end
    @machine.before_transition do |_object, transition|
      register_preparation(transition, :b)
      transition.register_compensation { raise 'release b2 failed' }
    end
    @object.save_result = false

    refute @transition.perform
    assert_equal %i[prepare_a prepare_b release_b release_a], @log
    assert_equal ['release b2 failed', 'release a2 failed'], @transition.compensation_errors.map(&:message)
  end

  def test_should_run_each_compensation_only_once
    @machine.before_transition { |_object, transition| register_preparation(transition, :a) }
    @object.save_result = false

    refute @transition.perform
    @transition.rollback

    assert_equal %i[prepare_a release_a], @log
  end

  def test_should_accept_a_symbol_compensation
    @machine.before_transition do |_object, transition|
      @log << :prepare
      transition.register_compensation(:release)
    end
    @object.save_result = false

    refute @transition.perform
    assert_equal [:release], @object.released
  end

  def test_should_pass_object_and_transition_to_block_compensations
    @machine.before_transition do |_object, transition|
      transition.register_compensation { |*args| @compensation_args = args }
    end
    @object.save_result = false

    refute @transition.perform
    assert_equal [@object, @transition], @compensation_args
  end

  def test_should_require_a_method_or_block
    assert_raises(ArgumentError) { @transition.register_compensation }
  end

  def test_should_clear_pending_compensations_on_reset
    @transition.register_compensation { @log << :release }
    @transition.reset

    assert_empty @transition.compensations
    @transition.rollback
    assert_empty @log
  end
end
