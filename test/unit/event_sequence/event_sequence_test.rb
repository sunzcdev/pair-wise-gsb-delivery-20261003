# frozen_string_literal: true

require 'test_helper'

class EventSequenceTest < StateMachinesTest
  def setup
    @klass = Class.new do
      attr_accessor :allow_shift, :fail_save, :saved_count, :saved_state

      state_machine :state, initial: :parked, action: :save do
        before_transition { |vehicle, transition| vehicle.log << "before_#{transition.event}" }
        after_transition { |vehicle, transition| vehicle.log << "after_#{transition.event}" }

        event :ignite do
          transition parked: :idling
        end

        event :shift_up do
          transition idling: :first_gear, if: :allow_shift
        end

        event :shift_up_again do
          transition first_gear: :second_gear
        end
      end

      def log
        @log ||= []
      end

      def save
        self.saved_count = (saved_count || 0) + 1
        self.saved_state = state
        !fail_save
      end
    end
    @vehicle = @klass.new
    @vehicle.allow_shift = true
  end

  def test_should_fire_each_event_with_the_state_left_by_the_previous_one
    result = @vehicle.fire_event_sequence(:ignite, :shift_up, :shift_up_again)

    assert result.success?
    assert_equal 'second_gear', @vehicle.state
    assert_equal %i[ignite shift_up shift_up_again], result.completed_events
    assert_equal %w[before_ignite before_shift_up before_shift_up_again
                    after_shift_up_again after_shift_up after_ignite], @vehicle.log
  end

  def test_should_commit_the_action_only_once_after_all_events_succeed
    result = @vehicle.fire_event_sequence(:ignite, :shift_up, :shift_up_again)

    assert result.success?
    assert_equal 1, @vehicle.saved_count
    assert_equal 'second_gear', @vehicle.saved_state
  end

  def test_should_not_run_the_third_event_when_the_second_one_fails
    @vehicle.allow_shift = false

    result = @vehicle.fire_event_sequence(:ignite, :shift_up, :shift_up_again)

    refute result.success?
    assert_equal %w[before_ignite], @vehicle.log
    assert_equal 0, @vehicle.saved_count.to_i
    assert_equal 'parked', @vehicle.state
  end

  def test_should_attribute_the_failure_to_the_step_that_failed
    @vehicle.allow_shift = false

    result = @vehicle.fire_event_sequence(:ignite, :shift_up, :shift_up_again)

    refute result.success?
    assert_equal :shift_up, result.failed_event
    assert_equal :invalid_transition, result.failure_reason
    assert result.steps[0].rolled_back?
    assert result.steps[1].failed?
    assert_equal :invalid_transition, result.steps[1].failure_reason
    assert result.steps[2].pending?
    assert_equal [], result.completed_events
  end

  def test_should_restore_staged_event_attributes_on_failure
    @vehicle.allow_shift = false
    @klass.state_machine.before_transition(on: :ignite) { |vehicle, _| vehicle.state_event = 'shift_up' }

    result = @vehicle.fire_event_sequence(:ignite, :shift_up)

    refute result.success?
    assert_nil @vehicle.state_event
    assert_equal 'parked', @vehicle.state
  end

  def test_should_attribute_failure_to_a_halted_before_callback
    @klass.state_machine.before_transition(on: :shift_up) { throw :halt }

    result = @vehicle.fire_event_sequence(:ignite, :shift_up, :shift_up_again)

    refute result.success?
    assert_equal :shift_up, result.failed_event
    assert_equal :callback_halted, result.failure_reason
    assert result.steps[0].rolled_back?
    assert result.steps[2].pending?
    assert_equal 'parked', @vehicle.state
  end

  def test_should_roll_back_every_machine_state_when_the_commit_action_fails
    @vehicle.fail_save = true

    result = @vehicle.fire_event_sequence(:ignite, :shift_up, :shift_up_again)

    refute result.success?
    assert_equal :action_failed, result.failure_reason
    assert_nil result.failed_event
    assert result.steps.all?(&:rolled_back?)
    assert_equal 'parked', @vehicle.state
    assert_equal 1, @vehicle.saved_count
  end

  def test_should_raise_when_sequences_are_nested
    attempt_nest = true
    @klass.state_machine.before_transition(on: :ignite) do |vehicle, _|
      vehicle.fire_event_sequence(:shift_up) if attempt_nest
    end

    exception = assert_raises(StateMachines::InvalidContext) do
      @vehicle.fire_event_sequence(:ignite, :shift_up)
    end
    assert_equal 'Cannot fire an event sequence from within another event sequence on the same object', exception.message
    assert_equal 'parked', @vehicle.state

    # The object is usable again after the nested attempt
    attempt_nest = false
    assert @vehicle.fire_event_sequence(:ignite, :shift_up).success?
    assert_equal 'first_gear', @vehicle.state
  end

  def test_should_allow_the_caller_to_provide_a_transaction_boundary
    result = @vehicle.fire_event_sequence(:ignite, :shift_up) do |run|
      @vehicle.log << :boundary_start
      run.call
      @vehicle.log << :boundary_end
    end

    assert result.success?
    assert_equal 'first_gear', @vehicle.state
    assert_equal :boundary_start, @vehicle.log.first
    assert_equal :boundary_end, @vehicle.log.last
  end

  def test_should_raise_on_unknown_event_without_running_anything
    assert_raises(StateMachines::InvalidEvent) { @vehicle.fire_event_sequence(:ignite, :invalid) }
    assert_equal 'parked', @vehicle.state
    assert_equal [], @vehicle.log
  end

  def test_should_skip_the_commit_action_when_run_action_is_false
    result = @vehicle.fire_event_sequence(:ignite, :shift_up, false)

    assert result.success?
    assert_equal 'first_gear', @vehicle.state
    assert_equal 0, @vehicle.saved_count.to_i
  end
end
