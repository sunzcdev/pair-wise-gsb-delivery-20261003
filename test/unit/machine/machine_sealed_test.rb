# frozen_string_literal: true

require 'test_helper'

class MachineSealedTest < StateMachinesTest
  def setup
    @klass = Class.new do
      attr_accessor :callback_hit
    end
    @machine = StateMachines::Machine.new(@klass, initial: :parked)
    @machine.event(:ignite) { transition parked: :idling }
    @machine.event(:park) { transition idling: :parked }
    @machine.before_transition(-> {})
    @machine.seal!
  end

  def test_should_be_sealed_after_sealing
    assert_predicate @machine, :sealed?
  end

  def test_should_not_be_sealed_by_default
    machine = StateMachines::Machine.new(Class.new)

    refute_predicate machine, :sealed?
  end

  def test_should_return_self_when_sealed
    machine = StateMachines::Machine.new(Class.new)

    assert_same machine, machine.seal!
  end

  def test_should_be_idempotent
    assert_same @machine, @machine.seal!
  end

  def test_should_raise_error_when_adding_a_new_state
    exception = assert_raises(StateMachines::MachineSealedError) { @machine.state :stalled }

    assert_equal 'states', exception.part
    assert_includes exception.message, 'Cannot modify states of sealed state machine :state'
  end

  def test_should_raise_error_when_reconfiguring_an_existing_state
    exception = assert_raises(StateMachines::MachineSealedError) { @machine.state :parked, value: 'custom' }

    assert_equal 'states', exception.part
  end

  def test_should_raise_error_when_adding_a_state_context
    exception = assert_raises(StateMachines::MachineSealedError) { @machine.state(:parked) { def speed = 0 } }

    assert_equal 'states', exception.part
  end

  def test_should_raise_error_when_adding_a_new_event
    exception = assert_raises(StateMachines::MachineSealedError) { @machine.event(:stall) { transition idling: :stalled } }

    assert_equal 'events', exception.part
    assert_includes exception.message, 'Cannot modify events of sealed state machine :state'
  end

  def test_should_raise_error_when_reconfiguring_an_existing_event
    exception = assert_raises(StateMachines::MachineSealedError) { @machine.event :ignite, human_name: 'Start' }

    assert_equal 'events', exception.part
  end

  def test_should_raise_error_when_adding_an_event_branch
    exception = assert_raises(StateMachines::MachineSealedError) { @machine.events[:ignite].transition idling: :parked }

    assert_equal 'event branches', exception.part
    assert_includes exception.message, 'Cannot modify event branches of sealed state machine :state'
  end

  def test_should_raise_error_when_resetting_an_event
    exception = assert_raises(StateMachines::MachineSealedError) { @machine.events[:ignite].reset }

    assert_equal 'event branches', exception.part
  end

  def test_should_raise_error_when_adding_a_before_callback
    exception = assert_raises(StateMachines::MachineSealedError) { @machine.before_transition(-> {}) }

    assert_equal 'callbacks', exception.part
    assert_includes exception.message, 'Cannot modify callbacks of sealed state machine :state'
  end

  def test_should_raise_error_when_adding_an_after_callback
    exception = assert_raises(StateMachines::MachineSealedError) { @machine.after_transition(-> {}) }

    assert_equal 'callbacks', exception.part
  end

  def test_should_raise_error_when_adding_an_around_callback
    exception = assert_raises(StateMachines::MachineSealedError) { @machine.around_transition(-> {}) }

    assert_equal 'callbacks', exception.part
  end

  def test_should_raise_error_when_adding_a_failure_callback
    exception = assert_raises(StateMachines::MachineSealedError) { @machine.after_failure(-> {}) }

    assert_equal 'callbacks', exception.part
  end

  def test_should_raise_error_when_changing_the_initial_state
    exception = assert_raises(StateMachines::MachineSealedError) { @machine.initial_state = :idling }

    assert_equal 'initial state', exception.part
    assert_includes exception.message, 'Cannot modify initial state of sealed state machine :state'
  end

  def test_should_raise_error_when_adding_to_the_state_collection_directly
    exception = assert_raises(StateMachines::MachineSealedError) { @machine.states << StateMachines::State.new(@machine, :stalled) }

    assert_equal 'states', exception.part
  end

  def test_should_raise_error_when_adding_to_the_event_collection_directly
    event = StateMachines::Event.new(StateMachines::Machine.new(Class.new), :stall)
    exception = assert_raises(StateMachines::MachineSealedError) { @machine.events << event }

    assert_equal 'events', exception.part
  end

  def test_should_seal_state_collection
    assert_predicate @machine.states, :sealed?
  end

  def test_should_seal_event_collection
    assert_predicate @machine.events, :sealed?
  end

  def test_should_freeze_states
    assert @machine.states.all?(&:frozen?)
  end

  def test_should_freeze_events
    assert @machine.events.all?(&:frozen?)
  end

  def test_should_freeze_event_branches
    assert @machine.events.all? { |event| event.branches.frozen? }
  end

  def test_should_freeze_callbacks
    assert @machine.callbacks.frozen?
    assert @machine.callbacks.each_value.all?(&:frozen?)
  end

  def test_should_freeze_internal_state_indices
    indices = @machine.states.instance_variable_get(:@indices)

    assert indices.frozen?
    assert indices.each_value.all?(&:frozen?)
  end

  def test_should_freeze_internal_event_indices
    indices = @machine.events.instance_variable_get(:@indices)

    assert indices.frozen?
    assert indices.each_value.all?(&:frozen?)
  end

  def test_should_still_allow_lookups
    assert_equal :parked, @machine.states[:parked].name
    assert_equal :ignite, @machine.events[:ignite].name
    assert_equal :ignite, @machine.event(:ignite).name
  end

  def test_should_not_freeze_objects_using_the_machine
    object = @klass.new

    refute object.frozen?
  end

  def test_should_still_allow_transitions
    object = @klass.new

    assert object.ignite
    assert_equal 'idling', object.state
    assert object.park
    assert_equal 'parked', object.state
  end

  def test_should_still_run_callbacks
    klass = Class.new do
      attr_accessor :hit
    end
    machine = StateMachines::Machine.new(klass, initial: :parked)
    machine.event(:ignite) { transition parked: :idling }
    machine.before_transition { |object, _transition| object.hit = true }
    machine.seal!

    object = klass.new
    object.ignite

    assert object.hit
  end

  def test_should_allow_explicit_subclass_derivation
    subclass = Class.new(@klass)
    derived_machine = subclass.state_machine do
      state :stalled
    end

    refute_predicate derived_machine, :sealed?
    assert_includes derived_machine.states.map(&:name), :stalled
  end

  def test_should_not_share_mutable_configuration_with_derived_subclass
    subclass = Class.new(@klass)
    derived_machine = subclass.state_machine do
      state :stalled
    end

    refute_same @machine.states, derived_machine.states
    refute_same @machine.events, derived_machine.events
    refute_same @machine.callbacks, derived_machine.callbacks
    refute_same @machine.states[:parked], derived_machine.states[:parked]
    refute_same @machine.events[:ignite], derived_machine.events[:ignite]
  end

  def test_should_not_modify_parent_or_siblings_when_subclasses_extend_the_machine
    # First level of inheritance adds a state and an event
    car = Class.new(@klass)
    car.state_machine do
      state :stalled
      event :stall do
        transition idling: :stalled
      end
    end

    # Second level of inheritance adds a callback
    sports_car = Class.new(car)
    sports_car.state_machine do
      before_transition parked: :idling do |object, _transition|
        object.callback_hit = true
      end
    end

    # A sibling class that does not derive its own machine
    truck = Class.new(@klass)

    # The derived machines work with their extensions
    sports_car_instance = sports_car.new
    sports_car_instance.ignite
    sports_car_instance.stall
    assert_equal 'stalled', sports_car_instance.state
    assert sports_car_instance.callback_hit

    car_instance = car.new
    car_instance.ignite
    car_instance.stall
    assert_equal 'stalled', car_instance.state
    refute car_instance.callback_hit

    # The parent machine definition is unchanged
    assert_equal %i[parked idling], @machine.states.map(&:name)
    assert_equal %i[ignite park], @machine.events.map(&:name)
    assert_equal 1, @machine.callbacks[:before].length

    # Objects of the parent class transition exactly as before
    vehicle = @klass.new
    assert vehicle.ignite
    assert_equal 'idling', vehicle.state
    assert vehicle.park
    assert_equal 'parked', vehicle.state
    refute vehicle.callback_hit
    refute_respond_to vehicle, :stall

    # Objects of the sibling class transition exactly like the parent
    truck_instance = truck.new
    assert truck_instance.ignite
    assert_equal 'idling', truck_instance.state
    assert truck_instance.park
    assert_equal 'parked', truck_instance.state
    refute truck_instance.callback_hit
    refute_respond_to truck_instance, :stall
  end
end
