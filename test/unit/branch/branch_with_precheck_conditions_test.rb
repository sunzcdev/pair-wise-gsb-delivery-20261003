# frozen_string_literal: true

require 'test_helper'

class BranchWithPrecheckConditionsTest < StateMachinesTest
  def test_should_track_precheckable_conditions
    branch = StateMachines::Branch.new(parked: :idling, on: :ignite, if: :seatbelt_on?, precheck: :seatbelt_on?)

    assert_equal [:seatbelt_on?], branch.precheckable_conditions
  end

  def test_should_accept_multiple_precheckable_conditions
    branch = StateMachines::Branch.new(parked: :idling, on: :ignite, if: %i[seatbelt_on? engine_ok?], precheck: %i[seatbelt_on? engine_ok?])

    assert_equal %i[seatbelt_on? engine_ok?], branch.precheckable_conditions
  end

  def test_should_identify_precheckable_conditions
    branch = StateMachines::Branch.new(parked: :idling, on: :ignite, if: %i[seatbelt_on? engine_ok?], precheck: :seatbelt_on?)

    assert branch.precheckable?(:seatbelt_on?)
    refute branch.precheckable?(:engine_ok?)
  end

  def test_should_have_no_precheckable_conditions_by_default
    branch = StateMachines::Branch.new(parked: :idling, on: :ignite, if: :seatbelt_on?)

    assert_equal [], branch.precheckable_conditions
    refute branch.precheckable?(:seatbelt_on?)
  end

  def test_should_expose_guard_conditions_in_evaluation_order
    branch = StateMachines::Branch.new(parked: :idling, on: :ignite, if: :seatbelt_on?, unless: :alarm_on?)

    assert_equal [[:if, :seatbelt_on?], [:unless, :alarm_on?]], branch.guards
  end

  def test_should_have_no_guards_without_conditionals
    branch = StateMachines::Branch.new(parked: :idling, on: :ignite)

    assert_equal [], branch.guards
  end
end
