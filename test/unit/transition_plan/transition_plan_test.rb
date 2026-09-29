# frozen_string_literal: true

require 'test_helper'

class TransitionPlanTest < StateMachinesTest
  def setup
    @klass = Class.new do
      attr_accessor :state, :log

      def save
        log << :save
        true
      end
    end

    @machine = StateMachines::Machine.new(@klass, action: :save)
    @machine.state :parked, :idling, :towed

    @machine.events << @ignite = StateMachines::Event.new(@machine, :ignite)
    @ignite.transition(parked: :idling)

    @machine.events << @tow = StateMachines::Event.new(@machine, :tow)
    @tow.transition(parked: :towed)

    @object = @klass.new
    @object.state = 'parked'
    @object.log = []
  end

  def test_should_capture_from_and_to_states
    plan = @ignite.plan_for(@object)

    assert_equal :parked, plan.from_name
    assert_equal :idling, plan.to_name
    assert_equal 'parked', plan.from
    assert_equal 'idling', plan.to
  end

  def test_should_capture_selected_branch
    plan = @ignite.plan_for(@object)

    assert_equal @ignite.branches.first, plan.branch
  end

  def test_should_capture_event
    plan = @ignite.plan_for(@object)

    assert_equal @ignite, plan.event
    assert_equal :ignite, plan.event_name
  end

  def test_should_not_change_object_state
    @ignite.plan_for(@object)

    assert_equal 'parked', @object.state
  end

  def test_should_not_run_action
    @ignite.plan_for(@object)

    assert_empty @object.log
  end

  def test_should_list_callback_types_without_running_callbacks
    @machine.before_transition { |object| object.log << :before }
    @machine.around_transition { |_object, _transition, block| block.call }
    @machine.after_transition { |object| object.log << :after }

    plan = @ignite.plan_for(@object)

    assert_equal %i[before around after], plan.callback_types
    assert_empty @object.log
  end

  def test_should_only_list_callback_categories_that_match
    @machine.before_transition parked: :towed, on: :tow do |object|
      object.log << :before
    end

    plan = @ignite.plan_for(@object)

    assert_equal [], plan.callback_types
  end

  def test_should_return_nil_when_no_transition_matches
    @object.state = 'idling'

    assert_nil @ignite.plan_for(@object)
  end

  def test_should_be_available_as_instance_helper
    plan = @object.ignite_plan

    assert_instance_of StateMachines::TransitionPlan, plan
    assert_equal :idling, plan.to_name
  end

  def test_should_execute_plan_and_perform_transition
    plan = @ignite.plan_for(@object)

    assert plan.execute
    assert_equal 'idling', @object.state
    assert_equal [:save], @object.log
  end

  def test_should_not_be_stale_when_state_is_unchanged
    plan = @ignite.plan_for(@object)

    refute plan.stale?
    assert plan.valid?
  end

  def test_should_be_stale_after_another_event_changes_state
    plan = @ignite.plan_for(@object)

    @tow.fire(@object)

    assert plan.stale?
    refute plan.valid?
  end

  def test_should_not_execute_stale_plan_after_another_event_changes_state
    plan = @ignite.plan_for(@object)

    # Another event moves the object away from the planned starting state
    @tow.fire(@object)
    @object.log.clear

    error = assert_raises(StateMachines::StalePlanError) { plan.execute }
    assert_equal plan, error.plan

    # The stale plan must not be forced through
    assert_equal 'towed', @object.state
    assert_empty @object.log
  end

  def test_should_not_execute_plan_when_guard_no_longer_passes
    klass = Class.new do
      attr_accessor :state, :ready
    end
    machine = StateMachines::Machine.new(klass)
    machine.state :parked, :idling
    machine.events << event = StateMachines::Event.new(machine, :ignite)
    event.transition(parked: :idling, if: :ready)

    object = klass.new
    object.state = 'parked'
    object.ready = true

    plan = event.plan_for(object)
    object.ready = false

    refute plan.valid?
    assert_raises(StateMachines::StalePlanError) { plan.execute }
    assert_equal 'parked', object.state
  end
end
