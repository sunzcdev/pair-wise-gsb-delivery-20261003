# frozen_string_literal: true

require_relative "test_helper"

# Verifies the ownership and cancellation conventions between request timers and the
# connection's own idle/ping timer, when a connection finishes a request and is immediately
# reused for the next one. A stale timer callback already queued in the selector must not
# interrupt the new request, while the new request's own deadline must still be honored.
class ConnectionTimerOwnershipTest < Minitest::Test
  include HTTPX
  include FakeMonotonicClockHelpers

  def test_stale_request_timer_does_not_interrupt_next_request_on_reused_connection
    with_fake_clock do |clock|
      selector = Selector.new
      connection = build_idle_connection(selector)

      # first request gets a request timer scheduled through the connection
      first_request = build_request(connection, request_timeout: 5)
      first_request.transition(:headers) # arms the request timeout callbacks

      assert_equal 1, first_request.active_timeouts.size
      stale_timer = first_request.active_timeouts.first

      # the first request completes before its deadline; completion cancels its timers
      first_request.complete!

      assert stale_timer.cancelled?
      assert first_request.active_timeouts.empty?
      assert selector_timers_empty?(selector), "cancelled request timers must be pruned"

      # the same connection is immediately reused for the second request
      second_request = build_request(connection, request_timeout: 8)
      second_request.transition(:headers)

      assert_equal 1, second_request.active_timeouts.size
      fresh_timer = second_request.active_timeouts.first
      refute fresh_timer.cancelled?

      # the old deadline "arrives late": advance past the first request's former deadline
      clock.advance(6)
      fire_timers(selector)

      # the second request must be untouched by the stale callback
      assert second_request.response.nil?, "new request must not be affected by the stale timer"
      refute fresh_timer.cancelled?

      # now the second request actively reaches its own deadline
      clock.advance(3)
      fire_timers(selector)

      assert second_request.response.is_a?(ErrorResponse), "the new request's own timer must still fire"
      assert_equal RequestTimeoutError, second_request.response.error.class
      # the fired interval is consumed, leaving no live timers in the selector
      assert selector_timers_empty?(selector)
    end
  end

  def test_request_timers_are_isolated_per_request_on_shared_interval
    with_fake_clock do |clock|
      selector = Selector.new
      connection = build_idle_connection(selector)

      requests = Array.new(2) { build_request(connection, request_timeout: 5) }
      requests.each { |req| req.transition(:headers) }

      # both requests share the same delay, hence the same selector interval
      assert_equal 1, timers_interval_count(selector)

      # the first request finishes and cancels only its own timer
      requests.first.complete!

      clock.advance(5)
      fire_timers(selector)

      assert requests.first.response.nil?
      assert requests.last.response.is_a?(ErrorResponse), "the other request timer must still fire"
      assert_equal RequestTimeoutError, requests.last.response.error.class
    end
  end

  def test_connection_ping_timer_is_owned_by_connection_and_cancelled_on_deactivate
    selector = Selector.new
    connection = build_idle_connection(selector)

    schedule_ping_timer(connection, selector)

    ping_timer = connection.instance_variable_get(:@ping_timer)

    refute_nil ping_timer
    refute ping_timer.cancelled?

    connection.deactivate

    assert ping_timer.cancelled?, "the connection idle/ping timer must be cancelled with the connection"
    assert_nil connection.instance_variable_get(:@ping_timer)
    assert selector_timers_empty?(selector)
  end

  def test_connection_ping_timer_is_cancelled_on_force_close
    selector = Selector.new
    connection = build_idle_connection(selector)

    schedule_ping_timer(connection, selector)
    ping_timer = connection.instance_variable_get(:@ping_timer)

    connection.force_close(true)

    assert ping_timer.cancelled?
    assert_nil connection.instance_variable_get(:@ping_timer)
  end

  private

  def build_idle_connection(selector)
    options = Options.new(timeout: { keep_alive_timeout: nil })
    connection = Connection.new(URI("http://example.com/"), options)
    connection.current_selector = selector
    connection
  end

  def build_request(connection, timeout_overrides)
    options = connection.options.merge(timeout: timeout_overrides)
    request = Request.new("GET", "http://example.com/", options)
    request.connection = connection
    connection.__send__(:set_request_timeouts, request)
    request
  end

  def schedule_ping_timer(connection, selector)
    connection.instance_variable_set(:@ping_timer, selector.after(2) do
      raise "stale ping timer fired after connection teardown"
    end)
  end

  def selector_timers_empty?(selector)
    selector.instance_variable_get(:@timers).empty?
  end

  # drives the selector timer queue the same way Selector#next_tick does after an
  # IO wakeup, without entering the blocking IO select loop.
  def fire_timers(selector)
    selector.instance_variable_get(:@timers).wait_interval
    selector.instance_variable_get(:@timers).fire
  end

  def timers_interval_count(selector)
    selector.instance_variable_get(:@timers).instance_variable_get(:@intervals).size
  end
end
