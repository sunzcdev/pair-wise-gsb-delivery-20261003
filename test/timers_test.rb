# frozen_string_literal: true

require_relative "test_helper"

class TimersTest < Minitest::Test
  include FakeMonotonicClockHelpers

  def test_timers_wait_interval
    timers = HTTPX::Timers.new

    assert_nil timers.wait_interval
    assert timers.empty?

    # cancelled timers do not set a wait interval
    [1, 30, 60].each do |seconds|
      timers.after(seconds) { raise "cancelled timer fired" }.cancel
    end

    assert_nil timers.wait_interval
    assert timers.empty?

    timers.after(30) {}

    # the wait interval is the time left until the (absolute monotonic) deadline,
    # which is the configured interval right after scheduling.
    assert_in_delta 30, timers.wait_interval, 1e-3
    assert !timers.empty?
  end

  def test_timers_cancelling_timer_callbacks_keeps_other_callbacks
    with_fake_clock do
      timers = HTTPX::Timers.new
      fired = []
      cancelled = timers.after(0) { raise "cancelled timer fired" }
      timers.after(0) { fired << :active }
      cancelled.cancel

      # the cancelled timer is pruned; the remaining zero-interval timer is due now
      assert_equal 0.0, timers.wait_interval
      timers.fire

      assert_equal [:active], fired
      assert timers.empty?
    end
  end

  def test_timer_cancel_is_idempotent_and_classifiable
    timers = HTTPX::Timers.new
    timer = timers.after(10) { raise "cancelled timer fired" }
    timer.label = :read_timeout

    assert_equal :read_timeout, timer.label

    timer.cancel
    timer.cancel

    assert timer.cancelled?
    assert timers.empty?
  end

  def test_cancelled_timer_never_fires_even_when_its_interval_does
    with_fake_clock do |clock|
      timers = HTTPX::Timers.new
      fired = []

      stale_timer = timers.after(5) { fired << :stale }
      timers.after(10) { fired << :fresh }

      # the first request finishes before its deadline and cancels its timer
      stale_timer.cancel

      # the stale deadline still "arrives" while the selector keeps running...
      clock.advance(6)
      timers.wait_interval
      timers.fire

      # ...but it must not fire, and must not block the second request's timer
      assert_equal [], fired
      assert !timers.empty?

      # the second request now actively reaches its own deadline
      clock.advance(4)
      timers.wait_interval
      timers.fire

      assert_equal [:fresh], fired
      assert timers.empty?
    end
  end

  def test_sibling_timer_cancelled_while_interval_fires_does_not_run
    with_fake_clock do |clock|
      timers = HTTPX::Timers.new
      fired = []
      handles = {}

      handles[:first] = timers.after(3) do
        fired << :first
        handles[:second].cancel
      end
      handles[:second] = timers.after(3) { fired << :second }

      clock.advance(3)
      timers.wait_interval
      timers.fire

      assert_equal [:first], fired
    end
  end

  def test_reregistering_interval_after_cancellation_arms_fresh_deadline
    with_fake_clock do |clock|
      timers = HTTPX::Timers.new
      fired = []

      timers.after(5) { fired << :old }.cancel

      clock.advance(100)
      timers.wait_interval # prunes the emptied interval

      timers.after(5) { fired << :fresh }

      assert_in_delta 5, timers.wait_interval, 1e-9

      clock.advance(5)
      timers.fire

      assert_equal [:fresh], fired
    end
  end

  def test_overdue_timer_reports_negative_wait_and_fires_immediately
    with_fake_clock do |clock|
      timers = HTTPX::Timers.new
      fired = []
      timers.after(2) { fired << :overdue }

      clock.advance(5)

      assert timers.wait_interval.negative?

      timers.fire

      assert_equal [:overdue], fired
    end
  end

  def test_distinct_intervals_fire_at_their_own_deadlines
    with_fake_clock do |clock|
      timers = HTTPX::Timers.new
      fired = []
      timers.after(2) { fired << :short }
      timers.after(8) { fired << :long }

      clock.advance(2)
      timers.wait_interval
      timers.fire
      assert_equal [:short], fired

      clock.advance(6)
      timers.wait_interval
      timers.fire
      assert_equal %i[short long], fired
    end
  end
end
