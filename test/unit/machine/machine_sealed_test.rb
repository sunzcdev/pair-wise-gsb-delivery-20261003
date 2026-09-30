# frozen_string_literal: true

require 'test_helper'

class MachineSealedTest < StateMachinesTest
  def setup
    @klass = Class.new do
      attr_accessor :state, :events
    end

    @machine = StateMachines::Machine.new(@klass, initial: :parked)
    @machine.event :ignite do
      transition parked: :idling
    end
    @machine.after_transition parked: :idling do |object|
      (object.events ||= []) << :parent
    end

    @machine.seal

    @state_subclass = Class.new(@klass)
    @state_machine = @machine.derive(@state_subclass) do
      event :park do
        transition idling: :parked
      end
      state :cancelled
    end
    @state_machine.seal

    @callback_subclass = Class.new(@state_subclass)
    @callback_machine = @state_machine.derive(@callback_subclass) do
      after_transition parked: :idling do |object|
        (object.events ||= []) << :derived
      end
    end
  end

  def test_seals_states_and_indices
    assert_predicate @machine, :sealed?
    assert_predicate @machine.states, :frozen?
    assert_predicate @machine.states[:parked], :frozen?

    error = assert_raises(FrozenError) { @machine.state :first_gear }
    assert_match(/states/, error.message)
  end

  def test_seals_event_branches_and_indices
    assert_predicate @machine.events, :frozen?
    assert_predicate @machine.events[:ignite], :frozen?
    assert_predicate @machine.events[:ignite].branches.first, :frozen?

    error = assert_raises(FrozenError) do
      @machine.event(:ignite) { transition idling: :parked }
    end
    assert_match(/event branches/, error.message)
  end

  def test_seals_callbacks
    assert_predicate @machine.callbacks, :frozen?
    assert_predicate @machine.callbacks[:after], :frozen?
    assert_predicate @machine.callbacks[:after].first, :frozen?
    assert_predicate @machine.callbacks[:after].first.branch, :frozen?

    error = assert_raises(FrozenError) { @machine.after_transition {} }
    assert_match(/callbacks/, error.message)
  end

  def test_does_not_freeze_business_objects
    object = @klass.new

    refute_predicate object, :frozen?

    object.ignite

    assert_equal 'idling', object.state
    assert_equal %i[parent], object.events
    refute_predicate object, :frozen?
  end

  def test_does_not_freeze_business_objects_used_as_state_values
    business_object = Object.new
    machine_klass = Class.new
    machine = StateMachines::Machine.new(machine_klass)
    machine.state :custom, value: business_object
    machine.seal

    refute_predicate business_object, :frozen?
  end

  def test_derived_machine_has_independent_states
    assert_includes @state_machine.states.keys, :cancelled
    refute_includes @machine.states.keys, :cancelled

    refute_same @machine.states, @state_machine.states
    refute_same @machine.states[:parked], @state_machine.states[:parked]
  end

  def test_derived_machine_has_independent_event_branches
    assert_equal %i[ignite], @machine.events.keys
    assert_equal %i[ignite park], @state_machine.events.keys

    refute_same @machine.events, @state_machine.events
    refute_same @machine.events[:ignite], @state_machine.events[:ignite]
    refute_same(
      @machine.events[:ignite].branches.first,
      @state_machine.events[:ignite].branches.first
    )
  end

  def test_sibling_machines_have_independent_callbacks
    parent_object = @klass.new
    state_object = @state_subclass.new
    derived_object = @callback_subclass.new

    [parent_object, state_object, derived_object].each(&:ignite)

    assert_equal %i[parent], parent_object.events
    assert_equal %i[parent], state_object.events
    assert_equal %i[parent derived], derived_object.events
  end

  def test_two_level_inheritance_keeps_parent_transition
    2.times do
      object = @klass.new

      assert object.ignite
      assert_equal 'idling', object.state
      assert_equal %i[parent], object.events
      refute_respond_to object, :park
    end
  end

  def test_two_level_inheritance_keeps_state_subclass_transition
    2.times do
      object = @state_subclass.new

      assert object.ignite
      assert object.park
      assert_equal 'parked', object.state
      assert_equal %i[parent], object.events
    end
  end

  def test_callback_subclass_keeps_its_additional_transition_behavior
    2.times do
      object = @callback_subclass.new

      assert object.ignite
      assert object.park
      assert_equal 'parked', object.state
      assert_equal %i[parent derived], object.events
    end
  end

  def test_sealed_machine_can_only_derive_for_another_owner_class
    error = assert_raises(ArgumentError) { @machine.derive(@klass) }
    assert_match(/same owner class/, error.message)
  end
end
