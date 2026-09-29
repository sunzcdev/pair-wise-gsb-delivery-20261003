# frozen_string_literal: true

require 'test_helper'

class DeferredEventsTest < StateMachinesTest
  class Vehicle
    attr_reader :tracking_log
    attr_accessor :ready, :auto_launch

    state_machine :state, initial: :parked do
      event :ignite do
        transition parked: :idling
      end

      event :park do
        transition idling: :parked
      end

      event :shift_up, defer: true do
        transition idling: :first_gear, first_gear: :second_gear
      end

      event :note, defer: true do
        transition idling: same, first_gear: same, second_gear: same
      end

      event :wait, defer: true do
        transition done: same
      end

      event :launch, defer: true do
        transition idling: :done, if: :ready
      end

      before_transition(on: :note) do |_vehicle, transition|
        raise 'boom' if transition.args.include?(:bad)
      end

      after_transition(on: :note) do |vehicle, transition|
        vehicle.tracking_log << [:note, *transition.args]
      end

      after_transition(on: :ignite) do |vehicle, _transition|
        vehicle.launch if vehicle.auto_launch
      end
    end

    def initialize
      @tracking_log = []
      super()
    end
  end

  class LimitedVehicle
    state_machine :state, initial: :parked, defer_queue_size: 5, defer_replay_limit: 2 do
      event :ignite do
        transition parked: :idling
      end

      event :touch do
        transition idling: same
      end

      event :wait, defer: true do
        transition done: same
      end
    end
  end

  def setup
    @vehicle = Vehicle.new
  end

  def test_should_defer_declared_event_and_return_deferred
    assert_equal :deferred, @vehicle.shift_up
    assert_equal 'parked', @vehicle.state

    assert_equal 1, @vehicle.deferred_events.length
    entry = @vehicle.deferred_events.first
    assert_equal :state, entry.machine_name
    assert_equal :shift_up, entry.event
    assert_equal [], entry.args
  end

  def test_should_save_event_args_with_deferred_event
    assert_equal :deferred, @vehicle.note(:first, 42)
    assert_equal [:first, 42], @vehicle.deferred_events.first.args
  end

  def test_should_replay_deferred_events_in_order_after_successful_transition
    @vehicle.note(:first)
    @vehicle.note(:second)
    @vehicle.shift_up

    assert @vehicle.ignite
    assert_equal 'first_gear', @vehicle.state
    assert_equal [[:note, :first], [:note, :second]], @vehicle.tracking_log
    assert_empty @vehicle.deferred_events

    report = @vehicle.deferred_replay_report
    assert_equal %i[note note shift_up], report.fired.map(&:event)
    assert_equal [[:first], [:second], []], report.fired.map(&:args)
    assert_empty report.redeferred
    assert_empty report.failed
  end

  def test_should_not_defer_undeclared_events
    refute @vehicle.park
    assert_empty @vehicle.deferred_events
  end

  def test_should_raise_original_error_for_undeclared_event_bang
    assert_raises(StateMachines::InvalidTransition) { @vehicle.park! }
  end

  def test_should_raise_original_error_for_unknown_event
    assert_raises(StateMachines::InvalidEvent) { @vehicle.fire_events(:bogus) }
  end

  def test_should_not_raise_for_deferred_event_bang
    assert_equal :deferred, @vehicle.shift_up!
    assert_equal 1, @vehicle.deferred_events.length
  end

  def test_should_limit_queue_size
    vehicle = LimitedVehicle.new
    5.times { assert_equal :deferred, vehicle.wait }

    error = assert_raises(StateMachines::DeferredQueueOverflow) { vehicle.wait }
    assert_equal 5, error.limit
    assert_equal 5, vehicle.deferred_events.length
  end

  def test_should_redefer_events_that_still_cannot_transition
    @vehicle.wait

    assert @vehicle.ignite
    report = @vehicle.deferred_replay_report
    assert_equal %i[wait], report.redeferred.map(&:event)
    assert_empty report.fired
    assert_equal %i[wait], @vehicle.deferred_events.map(&:event)
  end

  def test_should_bound_replay_round_and_not_hang_on_repeatedly_deferred_event
    vehicle = LimitedVehicle.new
    3.times { vehicle.wait }

    assert vehicle.ignite
    report = vehicle.deferred_replay_report
    assert_equal 2, report.processed
    assert_equal 2, report.redeferred.length
    assert_equal 3, vehicle.deferred_events.length

    # The same event keeps being deferred on every round without ever
    # blocking the caller
    5.times { assert vehicle.touch }
    assert_equal %i[wait wait wait], vehicle.deferred_events.map(&:event)
  end

  def test_should_observe_events_produced_by_callbacks
    @vehicle.auto_launch = true

    assert @vehicle.ignite
    assert_equal %i[launch], @vehicle.deferred_events.map(&:event)
    assert_equal %i[launch], @vehicle.deferred_replay_report.redeferred.map(&:event)

    @vehicle.ready = true
    @vehicle.note(:ping)
    assert_equal 'done', @vehicle.state
    assert_equal %i[launch], @vehicle.deferred_replay_report.fired.map(&:event)
    assert_empty @vehicle.deferred_events
  end

  def test_should_record_replay_failures_and_continue
    @vehicle.note(:bad)
    @vehicle.note(:good)

    assert @vehicle.ignite
    report = @vehicle.deferred_replay_report

    assert_equal 1, report.failed.length
    entry, error = report.failed.first
    assert_equal :note, entry.event
    assert_equal [:bad], entry.args
    assert_instance_of RuntimeError, error

    assert_equal %i[note], report.fired.map(&:event)
    assert_equal [[:note, :good]], @vehicle.tracking_log
    assert_empty @vehicle.deferred_events
  end

  def test_should_keep_queues_per_object
    other = Vehicle.new
    @vehicle.shift_up

    assert_equal 1, @vehicle.deferred_events.length
    assert_empty other.deferred_events
  end
end
