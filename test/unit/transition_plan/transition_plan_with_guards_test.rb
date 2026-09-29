# frozen_string_literal: true

require 'test_helper'

class TransitionPlanWithGuardsTest < StateMachinesTest
  def setup
    @klass = Class.new do
      attr_accessor :state, :seatbelt_on

      def seatbelt_on?
        seatbelt_on
      end
    end

    @machine = StateMachines::Machine.new(@klass)
    @machine.state :parked, :idling

    @machine.events << @event = StateMachines::Event.new(@machine, :ignite)

    @object = @klass.new
    @object.state = 'parked'
    @object.seatbelt_on = true
  end

  def test_should_evaluate_precheckable_guard_while_planning
    @event.transition(parked: :idling, if: :seatbelt_on?, precheck: :seatbelt_on?)

    plan = @event.plan_for(@object)

    refute_nil plan
    assert plan.precheckable?
    assert_empty plan.deferred_guards

    check = plan.guards.first
    assert_equal :if, check.kind
    assert_equal :seatbelt_on?, check.condition
    assert check.precheckable?
    assert check.checked?
    assert check.passed?
  end

  def test_should_not_plan_branch_when_precheckable_guard_fails
    @event.transition(parked: :idling, if: :seatbelt_on?, precheck: :seatbelt_on?)
    @object.seatbelt_on = false

    assert_nil @event.plan_for(@object)
  end

  def test_should_fall_through_to_next_branch_when_precheckable_guard_fails
    @event.transition(parked: :idling, if: :seatbelt_on?, precheck: :seatbelt_on?)
    @event.transition(parked: :parked)
    @object.seatbelt_on = false

    plan = @event.plan_for(@object)

    refute_nil plan
    assert_equal :parked, plan.to_name
  end

  def test_should_not_evaluate_undeclared_guard_while_planning
    evaluations = 0
    guard = lambda do |_object|
      evaluations += 1
      true
    end
    @event.transition(parked: :idling, if: guard)

    plan = @event.plan_for(@object)

    assert_equal 0, evaluations, 'undeclared guard should not be evaluated while planning'
    refute plan.precheckable?

    check = plan.guards.first
    refute check.precheckable?
    refute check.checked?
    assert_nil check.passed?
    assert_equal [check], plan.deferred_guards
  end

  def test_should_verify_deferred_guard_on_execute
    evaluations = 0
    guard = lambda do |_object|
      evaluations += 1
      true
    end
    @event.transition(parked: :idling, if: guard)

    plan = @event.plan_for(@object)
    assert_equal 0, evaluations

    assert plan.execute
    assert evaluations.positive?, 'deferred guard should be verified on execute'
    assert_equal 'idling', @object.state
  end

  def test_should_precheck_state_guards
    klass = Class.new do
      attr_accessor :state, :alarm

      state_machine :state, initial: :parked do
        event :ignite do
          transition parked: :idling, if_state: { alarm: :off }
        end
      end

      state_machine :alarm, initial: :off do
        event :sound do
          transition off: :on
        end
      end
    end

    object = klass.new
    plan = object.ignite_plan

    refute_nil plan
    assert plan.precheckable?

    check = plan.guards.first
    assert_equal :if_state, check.kind
    assert check.precheckable?
    assert check.passed?

    # A state change on the other machine invalidates the plan
    object.sound
    assert plan.stale? == false
    refute plan.valid?
    assert_raises(StateMachines::StalePlanError) { plan.execute }
    assert_equal 'parked', object.state
  end

  def test_should_not_plan_branch_when_state_guard_fails
    klass = Class.new do
      attr_accessor :state, :alarm

      state_machine :state, initial: :parked do
        event :ignite do
          transition parked: :idling, if_state: { alarm: :off }
        end
      end

      state_machine :alarm, initial: :on do
        event :silence do
          transition from: :on, to: :off
        end
      end
    end

    assert_nil klass.new.ignite_plan
  end

  def test_should_defer_all_guards_when_guard_option_is_false
    @event.transition(parked: :idling, if: :seatbelt_on?, precheck: :seatbelt_on?)

    plan = @event.plan_for(@object, guard: false)

    refute_nil plan
    refute plan.precheckable?
    assert_equal 1, plan.deferred_guards.size
  end
end
