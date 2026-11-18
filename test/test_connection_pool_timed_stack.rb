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

  # A deterministic hand-off between the thread running the creation block and
  # the test thread. The tests never sleep to "prove" that a thread is blocked:
  # the test thread only looks at the stack after the factory signals that it
  # was entered, and the factory only continues after the stack was observed.
  class Gate
    def initialize
      @mutex = Thread::Mutex.new
      @entered = Thread::ConditionVariable.new
      @continue = Thread::ConditionVariable.new
      @waiting = 0
      @open = false
    end

    def enter
      @mutex.synchronize do
        @waiting += 1
        @entered.broadcast
        @continue.wait(@mutex) until @open
        @waiting -= 1
      end
    end

    # Blocks until +count+ threads are inside the creation block.
    def wait_for_entered(count = 1)
      @mutex.synchronize do
        @entered.wait(@mutex) until @waiting >= count
      end
    end

    def open
      @mutex.synchronize do
        @open = true
        @continue.broadcast
      end
    end
  end

  def test_creation_happens_outside_the_stack_lock
    gate = Gate.new
    entered = Thread::Queue.new
    stack = ConnectionPool::TimedStack.new(size: 1) do
      entered << true
      gate.enter
      Object.new
    end

    creator = Thread.new { stack.pop(timeout: 5) }
    entered.pop

    # The factory is blocked inside the creation block. If it held the stack
    # lock, every call below would block forever. They must return immediately
    # and report the single reserved slot.
    assert_equal 1, stack.reserved
    assert_equal 0, stack.length
    assert_empty stack

    gate.open
    assert_kind_of Object, creator.value
    assert_equal 0, stack.reserved
    # The created connection was handed to the creator, so nothing is idle and
    # no capacity is left.
    assert_equal 0, stack.idle
    assert_equal 0, stack.length
  end

  def test_idle_connections_can_be_used_while_creation_is_slow
    gate = Gate.new
    entered = Thread::Queue.new
    stack = ConnectionPool::TimedStack.new(size: 2) do
      entered << true
      gate.enter
      Object.new
    end

    creator = Thread.new { stack.pop(timeout: 5) }
    entered.pop
    assert_equal 1, stack.reserved

    # A connection checked in while the factory blocks is immediately usable
    # by other threads.
    idle = Object.new
    stack.push(idle)
    assert_equal 1, stack.idle
    assert_same idle, stack.pop(timeout: 0)
    stack.push(idle)

    gate.open
    created = creator.value
    refute_same idle, created
    # The pushed connection is still on the stack and the created one was
    # handed to the creator.
    assert_equal 1, stack.idle
  end
  def test_creation_failure_releases_the_reservation
    gate = Gate.new
    entered = Thread::Queue.new
    attempts = 0
    stack = ConnectionPool::TimedStack.new(size: 1) do
      attempts += 1
      if attempts == 1
        entered << true
        gate.enter
        raise "boom"
      end
      Object.new
    end

    failing = Thread.new do
      stack.pop(timeout: 5)
    rescue => e
      e
    end
    entered.pop

    assert_equal 1, stack.reserved
    gate.open
    assert_equal "boom", failing.value.message
    assert_equal 0, stack.reserved

    # The released capacity can be used again.
    refute_nil stack.pop(timeout: 0)
    assert_equal 2, attempts
  end

  def test_creation_failure_does_not_leave_the_stack_at_capacity
    stack = ConnectionPool::TimedStack.new(size: 2) { raise "boom" }

    assert_raises(RuntimeError) { stack.pop(timeout: 0) }
    assert_raises(RuntimeError) { stack.pop(timeout: 0) }
    assert_equal 0, stack.reserved
    assert_equal 2, stack.length
  end

  def test_creation_failure_after_a_successful_creation_keeps_the_accounting
    calls = 0
    stack = ConnectionPool::TimedStack.new(size: 2) do
      calls += 1
      raise "boom" if calls == 2
      Object.new
    end

    conn = stack.pop(timeout: 0)
    refute_nil conn

    assert_raises(RuntimeError) { stack.pop(timeout: 0) }
    assert_equal 0, stack.reserved

    stack.push(conn)
    assert_equal 1, stack.idle
    assert_equal 2, stack.length
  end

  def test_cancelled_creator_releases_the_reservation_before_creation
    gate = Gate.new
    entered = Thread::Queue.new
    stack = ConnectionPool::TimedStack.new(size: 1) do
      entered << true
      gate.enter
      Object.new
    end

    creator = Thread.new { stack.pop(timeout: 30) }
    entered.pop
    assert_equal 1, stack.reserved

    creator.kill
    creator.join
    assert_equal 0, stack.reserved

    # The slot is free and a fresh connection can be created.
    gate.open
    refute_nil stack.pop(timeout: 0)
    assert_equal 0, stack.reserved
    assert_equal 0, stack.idle
  end

  def test_cancelled_caller_settles_the_reservation_exactly_once
    hold = Gate.new
    in_factory = Thread::Queue.new
    stack = ConnectionPool::TimedStack.new(size: 1) do
      in_factory << true
      hold.enter
      Object.new
    end

    creator = Thread.new do
      stack.pop(timeout: 30)
    rescue Exception => e
      e
    end
    in_factory.pop

    # A raised (not killed) cancellation is the way Timeout.timeout and
    # Thread#raise cancel a caller.
    creator.raise("cancelled")
    creator.join

    # The reservation is neither leaked nor double released, and the pool can
    # still be used afterwards.
    assert_equal 0, stack.reserved
    assert_equal 1, stack.length
    hold.open
    refute_nil stack.pop(timeout: 0)
    assert_equal 0, stack.reserved
    assert_equal 0, stack.idle
  end

  def test_cancelled_creator_leaves_the_created_connection_on_the_stack
    created = Thread::Queue.new
    settled = Thread::Queue.new
    hold = Gate.new
    stack = ConnectionPool::TimedStack.new(size: 1) do
      conn = Object.new
      created << conn
      conn
    end
    # Hold the creating thread right after the connection was admitted to the
    # stack but before it could be handed to the caller.
    stack.define_singleton_method(:admit_created) do |conn|
      super(conn).tap do
        settled << true
        hold.enter
      end
    end

    creator = Thread.new { stack.pop(timeout: 30) }
    conn = created.pop
    settled.pop

    # Cancel the creator while it is between admission and delivery: the caller
    # is unwinding, but the connection is already on the stack, so it stays
    # available instead of being leaked.
    creator.kill
    creator.join

    assert_equal 0, stack.reserved
    assert_equal 1, stack.idle
    assert_same conn, stack.pop(timeout: 0)
  end

  def test_shutdown_during_creation_closes_the_late_connection
    gate = Gate.new
    entered = Thread::Queue.new
    closed = Thread::Queue.new
    stack = ConnectionPool::TimedStack.new(size: 1) do
      entered << true
      gate.enter
      Object.new
    end

    creator = Thread.new do
      stack.pop(timeout: 5)
    rescue => e
      e
    end
    entered.pop
    assert_equal 1, stack.reserved

    stack.shutdown { |conn| closed << conn }

    gate.open
    # The waiter is told the pool shut down and the late connection is closed
    # by the shutdown block rather than being added to the pool.
    assert_instance_of ConnectionPool::PoolShuttingDownError, creator.value
    assert_kind_of Object, closed.pop

    assert_equal 0, stack.reserved
    assert_equal 0, stack.idle
    assert_raises(ConnectionPool::PoolShuttingDownError) { stack.pop(timeout: 0) }
  end

  def test_preload_uses_the_lazy_creation_rules
    created = 0
    stack = ConnectionPool::TimedStack.new(size: 3) do
      created += 1
      Object.new
    end

    assert_equal 3, stack.preload
    assert_equal 3, stack.idle
    assert_equal 0, stack.reserved
    assert_equal 3, stack.length
    refute_empty stack

    # Nothing more is created beyond the pool size.
    assert_equal 0, stack.preload
    assert_equal 3, created
  end

  def test_preload_count
    stack = ConnectionPool::TimedStack.new(size: 5) { Object.new }

    assert_equal 2, stack.preload(count: 2)
    assert_equal 2, stack.idle
    assert_equal 5, stack.length
  end

  def test_preload_shares_capacity_with_checkouts
    gate = Gate.new
    entered = Thread::Queue.new
    stack = ConnectionPool::TimedStack.new(size: 1) do
      entered << true
      gate.enter
      Object.new
    end

    preloader = Thread.new { stack.preload }
    entered.pop
    assert_equal 1, stack.reserved

    # The preload reserves the only slot, so a checkout cannot create a
    # second connection.
    assert_raises(ConnectionPool::TimeoutError) { stack.pop(timeout: 0) }
    assert_equal 1, stack.reserved

    gate.open
    assert_equal 1, preloader.value
    assert_equal 1, stack.idle
    assert_equal 0, stack.reserved
  end

  def test_preload_releases_the_reservation_on_factory_error
    stack = ConnectionPool::TimedStack.new(size: 2) { raise "boom" }

    assert_raises(RuntimeError) { stack.preload }
    assert_equal 0, stack.reserved
    assert_equal 0, stack.idle
    assert_equal 2, stack.length
  end

  def test_preload_raises_after_shutdown
    stack = ConnectionPool::TimedStack.new(size: 1) { Object.new }
    stack.shutdown {}

    assert_raises(ConnectionPool::PoolShuttingDownError) { stack.preload }
  end

  def test_preload_rejects_negative_count
    stack = ConnectionPool::TimedStack.new(size: 1) { Object.new }

    assert_raises(ArgumentError) { stack.preload(count: -1) }
  end

  def test_concurrent_creation_never_exceeds_the_pool_size
    gate = Gate.new
    created = 0
    created_mutex = Thread::Mutex.new
    stack = ConnectionPool::TimedStack.new(size: 3) do
      gate.enter
      created_mutex.synchronize { created += 1 }
      Object.new
    end

    threads = Array.new(10) do
      Thread.new do
        stack.pop(timeout: 0.5)
      rescue => e
        e
      end
    end

    gate.wait_for_entered(3)
    # Exactly three reservations are in flight, so no further creation starts.
    assert_equal 3, stack.reserved
    gate.open
    threads.each(&:join)

    assert_equal 3, created
    assert_equal 0, stack.reserved
    # Three of the ten checkouts received a connection and hold it; the rest
    # timed out. No connection is idle and nothing was created beyond size.
    assert_equal 0, stack.idle
    assert_equal 0, stack.length
  end
end
