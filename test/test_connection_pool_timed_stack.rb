require_relative "helper"

class TestConnectionPoolTimedStack < Minitest::Test
  def setup
    @stack = ConnectionPool::TimedStack.new { Object.new }
  end

  def test_empty_eh
    stack = ConnectionPool::TimedStack.new(size: 1) { Object.new }

    refute_empty stack

    popped = stack.pop

    assert_empty stack

    stack.push popped

    refute_empty stack
  end

  def test_length
    stack = ConnectionPool::TimedStack.new(size: 1) { Object.new }

    assert_equal 1, stack.length

    popped = stack.pop

    assert_equal 0, stack.length

    stack.push popped

    assert_equal 1, stack.length
  end

  def test_length_after_shutdown_reload_for_no_create_stack
    assert_equal 0, @stack.length

    @stack.push(Object.new)

    assert_equal 1, @stack.length

    @stack.shutdown(reload: true) {}

    assert_equal 0, @stack.length
  end

  def test_length_after_shutdown_reload_with_checked_out_conn
    stack = ConnectionPool::TimedStack.new(size: 1) { Object.new }
    conn = stack.pop
    stack.shutdown(reload: true) {}
    assert_equal 0, stack.length
    stack.push(conn)
    assert_equal 1, stack.length
  end

  def test_idle
    stack = ConnectionPool::TimedStack.new(size: 1) { Object.new }

    assert_equal 0, stack.idle

    popped = stack.pop

    assert_equal 0, stack.idle

    stack.push popped

    assert_equal 1, stack.idle
  end

  def test_object_creation_fails
    stack = ConnectionPool::TimedStack.new(size: 2) { raise "failure" }

    begin
      stack.pop
    rescue => error
      assert_equal "failure", error.message
    end

    begin
      stack.pop
    rescue => error
      assert_equal "failure", error.message
    end

    refute_empty stack
    assert_equal 2, stack.length
  end

  def test_pop
    object = Object.new
    @stack.push object

    popped = @stack.pop

    assert_same object, popped
  end

  def test_pop_empty
    e = assert_raises(ConnectionPool::TimeoutError) { @stack.pop timeout: 0 }
    assert_equal "Waited 0 sec, 0/0 available", e.message

    assert_nil @stack.pop(timeout: 0, exception: false)
  end

  def test_pop_empty_custom_exception
    e = assert_raises(RuntimeError) { @stack.pop(timeout: 0, exception: RuntimeError) }
    assert_equal "Waited 0 sec, 0/0 available", e.message
  end

  def test_pop_full
    stack = ConnectionPool::TimedStack.new(size: 1) { Object.new }

    popped = stack.pop

    refute_nil popped
    assert_empty stack
  end

  def test_pop_full_with_extra_conn_pushed
    stack = ConnectionPool::TimedStack.new(size: 1) { Object.new }
    popped = stack.pop

    stack.push(Object.new)
    stack.push(popped)

    assert_equal 2, stack.length

    stack.shutdown(reload: true) {}

    assert_equal 1, stack.length
    stack.pop
    assert_raises(ConnectionPool::TimeoutError) { stack.pop(timeout: 0) }
  end

  def test_pop_wait
    thread = Thread.start {
      @stack.pop
    }

    Thread.pass while thread.status == "run"

    object = Object.new

    @stack.push object

    assert_same object, thread.value
  end

  def test_pop_shutdown
    @stack.shutdown {}

    assert_raises ConnectionPool::PoolShuttingDownError do
      @stack.pop
    end
  end

  def test_pop_shutdown_reload
    stack = ConnectionPool::TimedStack.new(size: 1) { Object.new }
    object = stack.pop
    stack.push(object)

    stack.shutdown(reload: true) {}

    refute_equal object, stack.pop
  end

  def test_pop_raises_error_if_shutdown_reload_is_run_and_connection_is_still_in_use
    stack = ConnectionPool::TimedStack.new(size: 1) { Object.new }
    stack.pop
    stack.shutdown(reload: true) {}
    assert_raises ConnectionPool::TimeoutError do
      stack.pop(timeout: 0)
    end
  end

  def test_push
    stack = ConnectionPool::TimedStack.new(size: 1) { Object.new }

    conn = stack.pop

    stack.push conn

    refute_empty stack
  end

  def test_push_shutdown
    called = []

    @stack.shutdown do |object|
      called << object
    end

    @stack.push Object.new

    refute_empty called
    assert_empty @stack
  end

  def test_shutdown
    @stack.push Object.new

    called = []

    @stack.shutdown do |object|
      called << object
    end

    refute_empty called
    assert_empty @stack
  end

  def test_shutdown_can_be_called_after_error
    3.times { @stack.push Object.new }

    called = []
    closing_error = "error in closing connection"
    raise_error = true
    shutdown_proc = ->(conn) do
      called << conn
      if raise_error
        raise_error = false
        raise closing_error
      end
    end

    assert_raises(closing_error) do
      @stack.shutdown(&shutdown_proc)
    end

    assert_equal 1, called.size

    @stack.shutdown(&shutdown_proc)
    assert_equal 3, called.size
  end

  def test_reap_can_be_called_after_error
    3.times { @stack.push Object.new }

    called = []
    closing_error = "error in closing connection"
    raise_error = true
    reap_proc = ->(conn) do
      called << conn
      if raise_error
        raise_error = false
        raise closing_error
      end
    end

    assert_raises(closing_error) do
      @stack.reap(idle_seconds: 0, &reap_proc)
    end

    assert_equal 1, called.size

    @stack.reap(idle_seconds: 0, &reap_proc)
    assert_equal 3, called.size
  end

  def test_reap
    @stack.push Object.new

    called = []

    @stack.reap(idle_seconds: 0) do |object|
      called << object
    end

    refute_empty called
    assert_empty @stack
  end

  def test_reap_full_stack
    stack = ConnectionPool::TimedStack.new(size: 1) { Object.new }
    stack.push stack.pop

    stack.reap(idle_seconds: 0) do |object|
      nil
    end

    # Can still pop from the stack after reaping all connections
    refute_nil stack.pop
  end

  def test_reap_large_idle_seconds
    @stack.push Object.new

    called = []

    @stack.reap(idle_seconds: 100) do |object|
      called << object
    end

    assert_empty called
    refute_empty @stack
  end

  def test_reap_no_block
    assert_raises(ArgumentError) do
      @stack.reap(idle_seconds: 0)
    end
  end

  def test_reap_non_numeric_idle_seconds
    assert_raises(ArgumentError) do
      @stack.reap(idle_seconds: "0") { |object| object }
    end
  end

  def test_reap_with_multiple_connections
    stack = ConnectionPool::TimedStack.new(size: 2) { Object.new }
    stubbed_time = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    conn1 = stack.pop
    conn2 = stack.pop

    stack.stub :current_time, stubbed_time do
      stack.push conn1
    end

    stack.stub :current_time, stubbed_time + 1 do
      stack.push conn2
    end

    called = []

    stack.stub :current_time, stubbed_time + 2 do
      stack.reap(idle_seconds: 1.5) do |object|
        called << object
      end
    end

    assert_equal [conn1], called
    refute_empty stack
    assert_equal 1, stack.idle
  end

  def test_reap_with_multiple_connections_and_zero_idle_seconds
    stack = ConnectionPool::TimedStack.new(size: 2) { Object.new }
    stubbed_time = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    conn1 = stack.pop
    conn2 = stack.pop

    stack.stub :current_time, stubbed_time do
      stack.push conn1
    end

    stack.stub :current_time, stubbed_time + 1 do
      stack.push conn2
    end

    called = []

    stack.stub :current_time, stubbed_time + 2 do
      stack.reap(idle_seconds: 0) do |object|
        called << object
      end
    end

    assert_equal [conn1, conn2], called
    assert_equal 0, stack.idle
  end

  def test_reap_with_multiple_connections_and_idle_seconds_outside_range
    stack = ConnectionPool::TimedStack.new(size: 2) { Object.new }
    stubbed_time = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    conn1 = stack.pop
    conn2 = stack.pop

    stack.stub :current_time, stubbed_time do
      stack.push conn1
    end

    stack.stub :current_time, stubbed_time + 1 do
      stack.push conn2
    end

    called = []

    stack.stub :current_time, stubbed_time + 2 do
      stack.reap(idle_seconds: 3) do |object|
        called << object
      end
    end

    assert_empty called
    assert_equal 2, stack.idle
  end

  def test_reap_does_not_loop_continuously
    stack = ConnectionPool::TimedStack.new(size: 2) { Object.new }
    stack.push(Object.new)
    stack.push(Object.new)

    close_attempts = 0
    stack.reap(idle_seconds: 0) do |conn|
      if close_attempts >= 2
        flunk "Reap is stuck in a loop"
      end
      close_attempts += 1
      stack.push(conn)
    end

    assert_equal 2, close_attempts
  end

  def test_pop_runs_health_check_and_returns_healthy_connection
    stack = ConnectionPool::TimedStack.new(size: 1, health_check: ->(conn) { conn.respond_to?(:healthy?) && conn.healthy? }) {
      Object.new.tap { |o| def o.healthy?; true; end }
    }

    conn = stack.pop

    refute_nil conn
    assert_empty stack
  end

  def test_pop_replaces_connection_failing_health_check
    created = []
    stack = ConnectionPool::TimedStack.new(size: 1, health_check: ->(conn) { conn[:healthy] }) do
      o = {id: created.size, healthy: true}
      created << o
      o
    end

    first = stack.pop
    stack.push(first)
    first[:healthy] = false

    replacement = stack.pop

    refute_same first, replacement
    assert_equal 2, created.size
    # The dead connection's slot was released (created back to 0) before the
    # replacement was created, so capacity accounting never leaks a slot.
    assert_equal 1, stack.instance_variable_get(:@created)
    assert_equal 0, stack.length
    assert_empty stack
  end

  def test_pop_treats_raising_health_check_as_failure
    error = RuntimeError.new("socket gone")
    stack = ConnectionPool::TimedStack.new(size: 1, health_check: ->(_conn) { raise error }) { Object.new }

    raised = assert_raises(ConnectionPool::HealthCheckFailedError) do
      stack.pop
    end

    assert_same error, raised.cause
    assert_equal 0, stack.idle
    assert_equal 1, stack.length
  end

  def test_pop_health_check_failure_is_bounded
    creations = 0
    stack = ConnectionPool::TimedStack.new(size: 3, health_check: ->(_conn) { false }) do
      creations += 1
      Object.new
    end

    assert_raises(ConnectionPool::HealthCheckFailedError) do
      stack.pop
    end

    # One checkout may replace at most @max connections, creating @max + 1 total.
    assert_equal 4, creations
    assert_equal 0, stack.instance_variable_get(:@created)
    assert_equal 3, stack.length
  end

  def test_pop_health_check_runs_outside_the_mutex
    stack = ConnectionPool::TimedStack.new(size: 2) { Object.new }
    stack.push(stack.pop) # warm an idle entry in slot 1

    entered = Queue.new
    release = Queue.new
    checker = Thread.new do
      stack.pop(health_check: ->(_conn) {
        entered << true
        release.pop # park while the health check is in progress
        true
      })
    end
    entered.pop

    # Slot 2 is free; it must be acquirable while the health check is parked,
    # which is only possible if the health check does not hold the mutex.
    other = Thread.new { stack.pop }
    acquired = other.join(2)
    refute_nil acquired, "pool was locked while a health check was running"

    release << true
    checker.join(2)
    other.join(2)
  end

  def test_pop_per_call_health_check_overrides_stack_default
    stack = ConnectionPool::TimedStack.new(size: 1, health_check: ->(_conn) { true }) { Object.new }
    original = stack.pop
    stack.push(original)

    replacement = stack.pop(health_check: ->(conn) { !conn.equal?(original) })

    refute_same original, replacement
    stack.push(replacement)
    assert_same replacement, stack.pop
  end

  def test_push_discards_connection_failing_health_check
    cleaned = []
    stack = ConnectionPool::TimedStack.new(
      size: 1,
      cleanup: ->(conn) { cleaned << conn }
    ) { Object.new }

    conn = stack.pop
    stack.push(conn, health_check: ->(_c) { false })

    assert_equal [conn], cleaned
    assert_equal 0, stack.idle
    assert_equal 1, stack.length
  end

  def test_push_health_check_failure_never_raises
    stack = ConnectionPool::TimedStack.new(size: 1) { Object.new }
    conn = stack.pop

    assert_nil stack.push(conn, health_check: ->(_c) { raise "boom" })
    assert_equal 0, stack.idle
    assert_equal 1, stack.length
  end

  def test_push_health_check_runs_outside_the_mutex
    stack = ConnectionPool::TimedStack.new(size: 2) { Object.new }
    conn = stack.pop                    # slot 1 held for the pushing thread
    second = stack.pop                  # occupy slot 2, then return it
    stack.push(second)

    entered = Queue.new
    release = Queue.new
    pusher = Thread.new do
      stack.push(conn, health_check: ->(_c) {
        entered << true
        release.pop # park during the checkin health check
        true
      })
    end
    entered.pop

    # The returned entry must be fetchable while the checkin check is parked,
    # proving the internal lock is not held during the callback.
    getter = Thread.new { stack.pop }
    acquired = getter.join(2)
    refute_nil acquired, "pool was locked while a checkin health check was running"

    release << true
    pusher.join(2)
    getter.join(2)
  end

  def test_cleanup_errors_are_swallowed_but_capacity_is_released
    stack = ConnectionPool::TimedStack.new(
      size: 1,
      cleanup: ->(_conn) { raise "cleanup failed" }
    ) { Object.new }

    conn = stack.pop
    stack.push(conn, health_check: ->(_conn) { false })

    assert_equal 1, stack.length
    assert_equal 0, stack.idle
  end

  def test_discard_releases_capacity_and_runs_cleanup
    cleaned = []
    stack = ConnectionPool::TimedStack.new(size: 1, cleanup: ->(conn) { cleaned << conn }) { Object.new }
    conn = stack.pop

    stack.discard(conn)

    assert_equal [conn], cleaned
    assert_equal 1, stack.length
    assert_equal 0, stack.idle
  end

  def test_discard_swallows_cleanup_errors
    stack = ConnectionPool::TimedStack.new(size: 1) { Object.new }
    conn = stack.pop

    assert_nil stack.discard(conn) { |_| raise "cleanup failed" }
    assert_equal 1, stack.length
  end

  def test_pop_does_not_recreate_connections_while_shutting_down
    creations = 0
    stack = ConnectionPool::TimedStack.new(
      size: 1,
      health_check: ->(_conn) { true }
    ) do
      creations += 1
      Object.new
    end
    stack.push(stack.pop)
    stack.shutdown {}

    assert_raises(ConnectionPool::PoolShuttingDownError) do
      stack.pop
    end
    assert_equal 1, creations
  end
end
