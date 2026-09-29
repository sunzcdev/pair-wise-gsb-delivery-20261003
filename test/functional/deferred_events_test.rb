# frozen_string_literal: true

require 'test_helper'

class DeferredEventsTest < StateMachinesTest
  def setup
    super
    @fired = []
    fired = @fired

    @klass = Class.new do
      state_machine initial: :parked do
        event :ignite do
          transition parked: :idling
        end

        event :shift_up, defer: true do
          transition idling: :first_gear
        end

        event :shift_down do
          defer
          transition first_gear: :idling
        end

        event :dock, defer: true do
          transition flying: :landed
        end

        event :repair do
          transition stalled: :parked
        end

        after_transition do |_object, transition|
          fired << [transition.event, transition.args]
        end
      end
    end

    @machine = @klass.state_machine
    @object = @klass.new
  end

  def test_should_not_be_deferrable_by_default
    refute @machine.events[:repair].deferrable?
  end

  def test_should_mark_event_deferrable_via_option
    assert @machine.events[:shift_up].deferrable?
  end

  def test_should_mark_event_deferrable_via_context_dsl
    assert @machine.events[:shift_down].deferrable?
  end

  def test_should_defer_event_with_args_when_it_cannot_fire
    assert_equal :deferred, @object.shift_up(:smoothly)
    assert_equal 'parked', @object.state

    queue = @object.state_deferred_events
    assert_equal 1, queue.length
    assert_equal :state, queue.first.machine_name
    assert_equal :shift_up, queue.first.event_name
    assert_equal [:smoothly], queue.first.args
    assert_equal :pending, queue.first.outcome
  end

  def test_should_not_raise_from_bang_version_when_deferred
    assert_equal :deferred, @object.shift_up!
  end

  def test_should_replay_deferred_events_in_order_after_successful_transition
    @object.shift_up(:smoothly)
    @object.shift_down

    assert @object.ignite

    assert_equal 'idling', @object.state
    assert_empty @object.state_deferred_events
    assert_equal [[:ignite, []], [:shift_up, [:smoothly]], [:shift_down, []]], @fired

    results = @object.state_deferred_replay_results
    assert_equal %i[shift_up shift_down], results.map(&:event_name)
    assert_equal %i[performed performed], results.map(&:outcome)
  end

  def test_should_keep_normal_invalid_events_failing
    refute @object.repair
    assert_empty @object.state_deferred_events

    exception = assert_raises(StateMachines::InvalidTransition) { @object.repair! }
    assert_equal :repair, exception.event
  end

  def test_should_raise_when_queue_limit_is_reached
    @machine.defer_queue_limit = 2

    @object.shift_up
    @object.shift_down
    exception = assert_raises(StateMachines::DeferredQueueFull) { @object.shift_up }
    assert_equal @machine, exception.machine
    assert_equal 2, @object.state_deferred_events.length
  end

  def test_should_not_hang_when_event_keeps_deferring
    @machine.defer_replay_limit = 3

    @object.dock
    @object.dock

    assert @object.ignite
    assert_equal 'idling', @object.state

    results = @object.state_deferred_replay_results
    assert_equal 3, results.length
    assert(results.all? { |entry| entry.event_name == :dock && entry.outcome == :deferred })
    assert_equal 2, @object.state_deferred_events.length
  end

  def test_should_observe_events_fired_from_callbacks_during_replay
    klass = Class.new do
      state_machine initial: :a do
        event :go do
          transition a: :b
        end

        event :follow, defer: true do
          transition b: :c
        end

        event :pend, defer: true do
          transition e: :f
        end

        after_transition(on: :follow) do |object, _transition|
          object.pend
        end
      end
    end
    klass.state_machine.defer_replay_limit = 2
    object = klass.new

    assert_equal :deferred, object.follow
    assert object.go

    assert_equal 'c', object.state
    results = object.state_deferred_replay_results
    assert_equal %i[follow pend], results.map(&:event_name)
    assert_equal %i[performed deferred], results.map(&:outcome)
    assert_equal 1, object.state_deferred_events.length
  end

  def test_should_observe_replay_failures
    klass = Class.new do
      state_machine initial: :parked do
        event :ignite do
          transition parked: :idling
        end

        event :shift_up, defer: true do
          transition idling: :first_gear
        end

        before_transition(on: :shift_up) { throw :halt }
      end
    end
    object = klass.new

    assert_equal :deferred, object.shift_up
    assert object.ignite

    assert_equal 'idling', object.state
    results = object.state_deferred_replay_results
    assert_equal %i[shift_up], results.map(&:event_name)
    assert_equal %i[failed], results.map(&:outcome)
    assert_empty object.state_deferred_events
  end

  def test_should_observe_replay_errors
    klass = Class.new do
      state_machine initial: :parked do
        event :ignite do
          transition parked: :idling
        end

        event :shift_up, defer: true do
          transition idling: :first_gear
        end

        before_transition(on: :shift_up) { raise 'boom' }
      end
    end
    object = klass.new

    assert_equal :deferred, object.shift_up
    assert object.ignite

    results = object.state_deferred_replay_results
    assert_equal %i[error], results.map(&:outcome)
    assert_instance_of RuntimeError, results.first.error
    assert_equal 'boom', results.first.error.message
  end
end
