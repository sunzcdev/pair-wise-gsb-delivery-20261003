require "test_helper"

class FiberPoolTest < Minitest::Test
  Execution = Struct.new(:started, :results, :pause) do
    def perform
      started << true if started
      sleep(pause) if pause
      results << [ Thread.current.object_id, Fiber.current.object_id ] if results
    end
  end

  CancelledExecution = Struct.new(:started) do
    def perform
      started << true if started
      raise Async::Stop.new
    end
  end

  SkippedExecution = Struct.new(:performed) do
    def perform
      self.performed = true
    end
  end

  def test_builds_a_fiber_pool
    pool = mock

    SolidQueue::FiberPool.expects(:new).with(5, on_idle: nil, on_unrecoverable_error: nil).returns(pool)

    assert_equal pool, SolidQueue::Pool.build(type: :fiber, size: 5)
  end

  def test_executes_jobs_as_fibers_on_a_single_reactor_thread
    with_execution_isolation(:fiber) do
      pool = SolidQueue::FiberPool.new(2)
      results = Thread::Queue.new

      pool.post Execution.new(nil, results, 0.05)
      pool.post Execution.new(nil, results, 0.05)

      entries = 2.times.map { Timeout.timeout(1.second) { results.pop } }

      assert_equal 1, entries.map(&:first).uniq.count
      assert_equal 2, entries.map(&:last).uniq.count
      assert_equal 2, pool.available_capacity
    ensure
      pool&.shutdown
      pool&.wait_for_termination(1.second)
    end
  end

  def test_cancels_admissions_waiting_for_a_fiber_without_performing_them
    with_execution_isolation(:fiber) do
      pool = SolidQueue::FiberPool.new(1)
      execution = SkippedExecution.new

      # Simulate a claim admitted to the pool but still waiting in front of the
      # fiber semaphore or reactor queue.
      pool.send(:reserve_capacity!)
      pool.send(:track_execution, execution)
      pool.send(:pending_executions) << execution

      assert_equal 0, pool.available_capacity
      assert_equal [ execution ], pool.cancel_pending_executions
      assert_equal 1, pool.available_capacity

      pool.send(:start_reactor_if_needed)
      pool.shutdown

      assert pool.wait_for_termination(1.second)
      assert_nil execution.performed
      assert_equal 1, pool.available_capacity
    ensure
      pool&.shutdown
      pool&.wait_for_termination(1.second)
    end
  end

  def test_waits_for_in_flight_executions_during_shutdown
    with_execution_isolation(:fiber) do
      pool = SolidQueue::FiberPool.new(1)
      started = Thread::Queue.new

      pool.post Execution.new(started, nil, 0.1)
      Timeout.timeout(1.second) { started.pop }

      pool.shutdown

      assert_nil pool.wait_for_termination(0.01)
      assert pool.wait_for_termination(1.second)
    ensure
      pool&.shutdown
      pool&.wait_for_termination(1.second)
    end
  end

  def test_shutdown_wakes_the_reactor_when_idle
    with_execution_isolation(:fiber) do
      pool = SolidQueue::FiberPool.new(1)
      results = Thread::Queue.new

      pool.post Execution.new(nil, results, nil)
      Timeout.timeout(1.second) { results.pop }

      pool.shutdown

      assert pool.wait_for_termination(1.second)
    ensure
      pool&.shutdown
      pool&.wait_for_termination(1.second)
    end
  end

  def test_starts_the_reactor_lazily_so_the_pool_can_be_built_before_forking
    with_execution_isolation(:fiber) do
      pool = SolidQueue::FiberPool.new(1)

      pid = fork do
        results = Thread::Queue.new
        pool.post Execution.new(nil, results, nil)
        Timeout.timeout(1.second) { results.pop }
        pool.shutdown
        pool.wait_for_termination(1.second)
        exit!(0)
      end

      _, status = Process.waitpid2(pid)
      assert_equal 0, status.exitstatus
    ensure
      pool&.shutdown
      pool&.wait_for_termination(1.second)
    end
  end

  def test_marks_the_pool_as_fatal_when_an_execution_is_cancelled
    with_execution_isolation(:fiber) do
      notifications = Thread::Queue.new
      started = Thread::Queue.new
      reported_errors = []
      original_on_thread_error = SolidQueue.on_thread_error
      SolidQueue.on_thread_error = ->(error) { reported_errors << error.class.name }

      pool = SolidQueue::FiberPool.new(1, on_idle: -> { notifications << :changed })

      pool.post CancelledExecution.new(started)
      Timeout.timeout(1.second) { started.pop }
      Timeout.timeout(1.second) { notifications.pop }

      error = assert_raises(Async::Stop) { pool.available_capacity }
      assert_equal [ error.class.name ], reported_errors
    ensure
      SolidQueue.on_thread_error = original_on_thread_error
      pool&.shutdown
      pool&.wait_for_termination(1.second)
    end
  end
end
