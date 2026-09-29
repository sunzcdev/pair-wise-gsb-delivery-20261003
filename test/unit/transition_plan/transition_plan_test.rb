# frozen_string_literal: true

require 'test_helper'

class TransitionPlanTest < StateMachinesTest
  def setup
    @klass = Class.new do
      attr_accessor :seatbelt_on
      attr_reader :saved, :callback_log, :guard_calls

      def save
        @saved = true
      end

      def track_callback(kind)
        (@callback_log ||= []) << kind
      end

      def seatbelt_on?
        (@guard_calls ||= []) << :seatbelt_on?
        !!seatbelt_on
      end
    end

    @machine = StateMachines::Machine.new(@klass, action: :save)
    @machine.state :parked, :idling
    @machine.event :ignite do
      transition parked: :idling
    end
    @machine.event :park do
      transition idling: :parked
    end

    @object = @klass.new
    @object.state = 'parked'
  end

  def test_should_plan_from_and_to_states
    plan = @machine.event(:ignite).plan(@object)

    assert_equal :ignite, plan.event
    assert_equal :parked, plan.from_name
    assert_equal :idling, plan.to_name
    assert_equal 'parked', plan.from
    assert_equal 'idling', plan.to
    assert_same @object, plan.object
    assert_same @machine, plan.machine
  end

  def test_should_expose_selected_branch
    plan = @machine.event(:ignite).plan(@object)

    assert_instance_of StateMachines::Branch, plan.branch
    assert_same @machine.event(:ignite).branches.first, plan.branch
  end

  def test_should_not_change_state_when_planning
    @machine.event(:ignite).plan(@object)

    assert_equal 'parked', @object.state
  end

  def test_should_not_run_action_when_planning
    @machine.event(:ignite).plan(@object)

    refute @object.saved
  end

  def test_should_not_run_transition_callbacks_when_planning
    @machine.before_transition { |object| object.track_callback(:before) }
    @machine.around_transition { |_object, _transition, block| block.call }
    @machine.after_transition { |object| object.track_callback(:after) }

    @machine.event(:ignite).plan(@object)

    assert_nil @object.callback_log
  end

  def test_should_list_callback_kinds_that_would_run
    @machine.before_transition { |object| object.track_callback(:before) }
    @machine.around_transition { |_object, _transition, block| block.call }
    @machine.after_transition { |object| object.track_callback(:after) }

    plan = @machine.event(:ignite).plan(@object)

    assert_equal %i[before around after], plan.callback_kinds
  end

  def test_should_only_list_callback_kinds_matching_the_transition_context
    @machine.before_transition(on: :park) { |object| object.track_callback(:before_park) }
    @machine.after_transition(from: :parked, to: :idling) { |object| object.track_callback(:after) }

    plan = @machine.event(:ignite).plan(@object)

    assert_equal %i[after], plan.callback_kinds
  end

  def test_should_not_evaluate_callback_conditions_when_listing_kinds
    @machine.before_transition(if: :seatbelt_on?) { |object| object.track_callback(:before) }

    plan = @machine.event(:ignite).plan(@object)

    assert_equal %i[before], plan.callback_kinds
    assert_nil @object.guard_calls
  end

  def test_should_return_nil_when_no_transition_can_be_planned
    @object.state = 'idling'

    assert_nil @machine.event(:ignite).plan(@object)
  end

  def test_should_evaluate_pure_guards_when_planning
    @machine.event(:ignite).reset
    @machine.event(:ignite).transition({ parked: :idling, if: :seatbelt_on? })
    @machine.pure_guard :seatbelt_on?
    @object.seatbelt_on = true

    plan = @machine.event(:ignite).plan(@object)

    assert plan
    assert plan.verified?
    assert_empty plan.unchecked_guards
    assert_equal [:seatbelt_on?], @object.guard_calls
  end

  def test_should_not_plan_when_pure_guard_fails
    @machine.event(:ignite).reset
    @machine.event(:ignite).transition({ parked: :idling, if: :seatbelt_on? })
    @machine.pure_guard :seatbelt_on?
    @object.seatbelt_on = false

    assert_nil @machine.event(:ignite).plan(@object)
  end

  def test_should_not_evaluate_undeclared_guards_when_planning
    @machine.event(:ignite).reset
    @machine.event(:ignite).transition({ parked: :idling, if: :seatbelt_on? })
    @object.seatbelt_on = true

    plan = @machine.event(:ignite).plan(@object)

    assert plan
    refute plan.verified?
    assert_equal [{ type: :if, condition: :seatbelt_on? }], plan.unchecked_guards
    assert_nil @object.guard_calls
  end

  def test_should_mark_proc_guards_as_unchecked
    guard = ->(object) { object.seatbelt_on? }
    @machine.event(:ignite).reset
    @machine.event(:ignite).transition({ parked: :idling, if: guard })
    @machine.pure_guard :seatbelt_on?

    plan = @machine.event(:ignite).plan(@object)

    assert plan
    refute plan.verified?
    assert_equal [{ type: :if, condition: guard }], plan.unchecked_guards
    assert_nil @object.guard_calls
  end

  def test_should_perform_planned_transition
    @machine.before_transition { |object| object.track_callback(:before) }
    @machine.after_transition { |object| object.track_callback(:after) }
    plan = @machine.event(:ignite).plan(@object)

    assert plan.perform
    assert_equal 'idling', @object.state
    assert @object.saved
    assert_equal %i[before after], @object.callback_log
  end

  def test_should_verify_unchecked_guards_when_performing
    @machine.event(:ignite).reset
    @machine.event(:ignite).transition({ parked: :idling, if: :seatbelt_on? })
    @object.seatbelt_on = true
    plan = @machine.event(:ignite).plan(@object)

    assert plan.perform
    assert_equal 'idling', @object.state
    assert_equal [:seatbelt_on?], @object.guard_calls
  end

  def test_should_raise_when_guard_no_longer_passes_at_perform_time
    @machine.event(:ignite).reset
    @machine.event(:ignite).transition({ parked: :idling, if: :seatbelt_on? })
    @object.seatbelt_on = true
    plan = @machine.event(:ignite).plan(@object)

    @object.seatbelt_on = false

    exception = assert_raises(StateMachines::StalePlanError) { plan.perform }
    assert_same plan, exception.plan
    assert_equal 'parked', @object.state
    refute @object.saved
  end

  def test_should_not_be_stale_when_state_is_unchanged
    plan = @machine.event(:ignite).plan(@object)

    refute plan.stale?
  end

  def test_should_be_stale_when_another_event_changes_state_after_planning
    plan = @machine.event(:park).plan(@object)
    assert_nil plan

    ignite_plan = @machine.event(:ignite).plan(@object)
    @object.ignite

    assert ignite_plan.stale?
  end

  def test_should_not_force_stale_plan_after_another_event_changes_state
    plan = @machine.event(:ignite).plan(@object)
    assert_equal :parked, plan.from_name
    assert_equal :idling, plan.to_name

    # Another event fires in the meantime, moving the object out from
    # under the plan
    @object.ignite
    assert_equal 'idling', @object.state

    exception = assert_raises(StateMachines::StalePlanError) { plan.perform }
    assert_same plan, exception.plan
    assert_match(/expected state to be :parked, but it is now :idling/, exception.message)

    # The stale plan must not have been applied on top of the new state
    assert_equal 'idling', @object.state
  end

  def test_should_not_force_stale_plan_after_object_moves_back_and_forth
    plan = @machine.event(:ignite).plan(@object)

    @object.ignite
    @object.park
    assert_equal 'parked', @object.state

    # State matches again, so the plan is no longer stale and can be applied
    refute plan.stale?
    assert plan.perform
    assert_equal 'idling', @object.state
  end

  def test_should_define_instance_plan_helper
    plan = @object.ignite_plan

    assert_instance_of StateMachines::TransitionPlan, plan
    assert_equal :idling, plan.to_name
  end

  def test_should_have_inspect_string
    plan = @machine.event(:ignite).plan(@object)

    assert_equal '#<StateMachines::TransitionPlan event=:ignite from_name=:parked to_name=:idling verified=true stale=false>', plan.inspect
  end
end
