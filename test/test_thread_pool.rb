# frozen_string_literal: true

require_relative "helper"

require "puma/thread_pool"

class TestThreadPool < PumaTest

  parallelize_me!

  def teardown
    @pool.shutdown(1) if defined?(@pool)
  end

  def new_pool(min, max, max_io_threads: 0, pool_shutdown_grace_time: nil, &block)
    block = proc { } unless block
    options = {
      min_threads: min,
      max_threads: max,
      max_io_threads: max_io_threads,
      pool_shutdown_grace_time: pool_shutdown_grace_time,
    }
    @pool = Puma::ThreadPool.new("tst", options, &block)
  end

  def mutex_pool(min, max, pool_shutdown_grace_time: nil, &block)
    block = proc { } unless block
    options = {
      min_threads: min,
      max_threads: max,
      pool_shutdown_grace_time: pool_shutdown_grace_time,
    }
    @pool = MutexPool.new("tst", options, &block)
  end

  # Wraps ThreadPool work in mutex for better concurrency control.
  class MutexPool < Puma::ThreadPool
    # Wait until the added work is completed before returning.
    # Array argument is treated as a batch of work items to be added.
    # Block will run after work is added but before it is executed on a worker thread.
    def <<(work, &block)
      work = [work] unless work.is_a?(Array)
      with_mutex do
        work.each {|arg| super arg}
        yield if block
        @not_full.wait(@mutex)
      end
    end

    def signal
      @not_full.signal
    end

    # If +wait+ is true, wait until the trim request is completed before returning.
    def trim(force=false, wait: true)
      super(force)

      return unless wait
      loop do
        # While we wait until @trim_requested is 0, the test might inspect other values
        # such as @spawned which may be modified after this variable is modified in the loop
        # Lock for race safety
        with_mutex do
          return if @trim_requested == 0
        end
        Thread.pass
      end
    end
  end

  def test_append_spawns
    saw = []
    pool = mutex_pool(0, 1) do |_processor, work|
      saw << work
    end

    pool << 1
    assert_equal 1, pool.spawned
    assert_equal [1], saw
  end

  def test_thread_name
    skip "Thread.name not supported" unless Thread.current.respond_to?(:name)
    thread_name = nil
    pool = mutex_pool(0, 1) {thread_name = Thread.current.name}
    pool << 1
    assert_equal("puma tst tp 001", thread_name)
  end

  def test_thread_name_linux
    skip "Thread.name not supported" unless Thread.current.respond_to?(:name)

    task_dir = File.join("", "proc", Process.pid.to_s, "task")
    skip "This test only works under Linux and MRI Ruby with appropriate permissions" if !(File.directory?(task_dir) && File.readable?(task_dir) && Puma::IS_MRI)

    expected_thread_name = "puma tst tp 001"
    found_thread = false
    pool = mutex_pool(0, 1) do
      # Read every /proc/<pid>/task/<tid>/comm file to find the thread name
      Dir.entries(task_dir).select {|tid| File.directory?(File.join(task_dir, tid))}.each do |tid|
        comm_file = File.join(task_dir, tid, "comm")
        next unless File.file?(comm_file) && File.readable?(comm_file)

        if File.read(comm_file).strip == expected_thread_name
          found_thread = true
          break
        end
      end
    end
    pool << 1

    assert(found_thread, "Did not find thread with name '#{expected_thread_name}'")
  end

  def test_converts_pool_sizes
    pool = new_pool("0", "1")

    assert_equal 0, pool.spawned

    pool << 1

    assert_equal 1, pool.spawned
  end

  def test_append_queues_on_max
    pool = new_pool(0, 0) do
      "Hello World!"
    end

    pool << 1
    pool << 2
    pool << 3

    assert_equal 3, pool.backlog
  end

  def test_thread_start_hook
    started = Queue.new
    options = {
      min_threads: 0,
      max_threads: 1,
      before_thread_start: [
        {
          id: nil,
          block: proc { started << 1 },
          cluster_only: false,
        }
      ]
    }
    block = proc { }
    pool = MutexPool.new("tst", options, &block)

    pool << 1

    assert_equal 1, pool.spawned
    assert_equal 1, started.length
  end

  def test_thread_start_hook_with_server
    dummy_server_obj = Struct.new(:thread_pool).new
    started = Queue.new
    options = {
      min_threads: 0,
      max_threads: 1,
      before_thread_start: [
        {
          id: nil,
          block: proc {|server| started << server },
          cluster_only: false,
        }
      ]
    }
    block = proc { }
    pool = MutexPool.new("tst", options, server: dummy_server_obj, &block)

    pool << 1

    assert_equal 1, pool.spawned
    assert_respond_to started.pop, :update_thread_pool_min_max
  end

  def test_out_of_band_hook
    out_of_band_called = Queue.new
    options = {
      min_threads: 0,
      max_threads: 1,
      out_of_band: [
        {
          id: nil,
          block: proc { out_of_band_called << 1 },
          cluster_only: false,
        }
      ]
    }
    block = proc { true }
    pool = MutexPool.new("tst", options, &block)

    pool << 1

    assert_equal 1, pool.spawned
    assert_equal 1, out_of_band_called.length
  end

  def test_out_of_band_parallel_hook_runs_after_request
    parallel_called = Queue.new
    options = {
      min_threads: 0,
      max_threads: 1,
      out_of_band_parallel: [
        {
          id: nil,
          block: proc { parallel_called << 1 },
          cluster_only: false,
        }
      ]
    }
    block = proc { true }
    @pool = Puma::ThreadPool.new("tst", options, &block)

    @pool << 1

    assert_equal 1, Timeout.timeout(2) { parallel_called.pop }
    refute @pool.out_of_band_parallel_running
    refute @pool.out_of_band_parallel_pending?
  end

  # A parallel hook occupies only its own thread; other requests must still be
  # picked up by the remaining pool threads while it is stuck.
  def test_requests_scheduled_while_parallel_hook_is_running
    hook_started = Queue.new
    release_hook = Queue.new
    requests_seen = Queue.new
    options = {
      min_threads: 0,
      max_threads: 2,
      out_of_band_parallel: [
        {
          id: nil,
          block: proc do
            hook_started << 1
            release_hook.pop
          end,
          cluster_only: false,
        }
      ]
    }
    block = proc { |_, work| requests_seen << work; true }
    @pool = Puma::ThreadPool.new("tst", options, &block)

    # Request 1 finishes, then its thread enters the parallel hook and blocks.
    @pool << 1
    hook_started.pop
    assert_predicate @pool, :out_of_band_parallel_running

    # Request 2 must be processed by the other thread even though the hook is stuck.
    @pool << 2
    Timeout.timeout(2) do
      Thread.pass until requests_seen.length == 2
    end
    assert_equal [1, 2], [requests_seen.pop, requests_seen.pop].sort
    assert_equal 2, @pool.spawned
    # The stuck hook still occupies its own thread; request 2's thread is idle again.
    assert_predicate @pool, :out_of_band_parallel_running
    assert_equal 1, @pool.waiting
  ensure
    release_hook << 1
  end

  # A parallel hook must not be reentered: triggers arriving while it runs
  # coalesce into a single follow-up run.
  def test_parallel_hook_is_not_reentered
    hook_started = Queue.new
    release_hook = Queue.new
    hook_completed = Queue.new
    options = {
      min_threads: 0,
      max_threads: 1,
      out_of_band_parallel: [
        {
          id: nil,
          block: proc do
            hook_started << 1
            release_hook.pop
            hook_completed << 1
          end,
          cluster_only: false,
        }
      ]
    }
    block = proc { true }
    @pool = Puma::ThreadPool.new("tst", options, &block)

    @pool << 1
    hook_started.pop

    # Two more requests complete while the hook is still in progress. Both
    # merely (re)assert the single pending flag, so only one follow-up runs.
    @pool << 2
    @pool << 3
    sleep 0.05
    assert_predicate @pool, :out_of_band_parallel_running

    release_hook << 1
    hook_completed.pop

    # Exactly one coalesced follow-up run, then the queue is fully drained.
    hook_started.pop
    release_hook << 1
    hook_completed.pop

    Timeout.timeout(2) do
      Thread.pass until @pool.backlog.zero? &&
        !@pool.out_of_band_parallel_running &&
        !@pool.out_of_band_parallel_pending?
    end
    assert_equal 0, hook_started.length
  ensure
    release_hook << 1 rescue nil
  end

  # The quiescent hook must wait until no request and no parallel hook is
  # running, even when both hook kinds are registered.
  def test_quiescent_hook_waits_for_parallel_hook
    parallel_started = Queue.new
    release_parallel = Queue.new
    quiescent_called = Queue.new
    in_quiescent = Mutex.new
    options = {
      min_threads: 0,
      max_threads: 2,
      out_of_band: [
        {
          id: nil,
          block: proc { in_quiescent.synchronize { quiescent_called << 1 } },
          cluster_only: false,
        }
      ],
      out_of_band_parallel: [
        {
          id: nil,
          block: proc do
            parallel_started << 1
            release_parallel.pop
          end,
          cluster_only: false,
        }
      ]
    }
    block = proc { true }
    @pool = Puma::ThreadPool.new("tst", options, &block)

    @pool << 1
    parallel_started.pop
    sleep 0.05
    # The quiescent hook cannot start while the parallel one is running.
    assert_equal 0, quiescent_called.length
    assert_predicate @pool, :out_of_band_parallel_running

    release_parallel << 1
    assert_equal 1, Timeout.timeout(2) { quiescent_called.pop }
    refute @pool.out_of_band_running
  ensure
    release_parallel << 1 rescue nil
  end

  # A stuck parallel hook must not prevent a forced shutdown: the shutdown
  # raises ForceShutdown into the hook thread and the pool drains fully.
  def test_forced_shutdown_interrupts_stuck_parallel_hook
    hook_started = Queue.new
    release_hook = Queue.new
    options = {
      min_threads: 0,
      max_threads: 1,
      pool_shutdown_grace_time: 0.05,
      out_of_band_parallel: [
        {
          id: nil,
          block: proc do
            hook_started << 1
            # Simulates maintenance that never returns.
            release_hook.pop
          end,
          cluster_only: false,
        }
      ]
    }
    block = proc { true }
    @pool = Puma::ThreadPool.new("tst", options, &block)

    @pool << 1
    hook_started.pop
    assert_predicate @pool, :out_of_band_parallel_running

    Timeout.timeout(2) { @pool.shutdown(0.05) }
    assert_equal 0, @pool.spawned
    assert_equal 0, @pool.instance_variable_get(:@processors).length
    refute @pool.out_of_band_parallel_running
  ensure
    release_hook << 1 rescue nil
  end

  def test_trim
    pool = mutex_pool(0, 1)

    pool << 1

    assert_equal 1, pool.spawned

    pool.trim(wait: true)
    assert_equal 0, pool.spawned
  end

  def test_trim_leaves_min
    pool = mutex_pool(1, 2)

    pool << [1, 2]

    assert_equal 2, pool.spawned

    pool.trim(wait: true)
    assert_equal 1, pool.spawned

    pool.trim(wait: true)
    assert_equal 1, pool.spawned
  end

  def test_force_trim_doesnt_overtrim
    pool = mutex_pool(1, 2)

    pool.<< [1, 2] do
      assert_equal 2, pool.spawned
      pool.trim true, wait: false
      pool.trim true, wait: false
    end

    assert_equal 1, pool.spawned
  end

  def test_trim_is_ignored_if_no_waiting_threads
    pool = mutex_pool(1, 2)

    pool.<< [1, 2] do
      assert_equal 2, pool.spawned
      pool.trim
      pool.trim
    end

    assert_equal 2, pool.spawned
    assert_equal 0, pool.trim_requested
  end

  def test_trim_thread_exit_hook
    exited = Queue.new
    options = {
      min_threads: 0,
      max_threads: 1,
      before_thread_exit: [
        {
          id: nil,
          block: proc { exited << 1 },
          cluster_only: false,
        }
      ]
    }
    block = proc { }
    pool = MutexPool.new("tst", options, &block)

    pool << 1

    assert_equal 1, pool.spawned

    # Thread.pass helps with intermittent tests, JRuby
    pool.trim
    Thread.pass
    sleep 0.1 unless Puma::IS_MRI # intermittent without
    assert_equal 0, pool.spawned
    Thread.pass
    sleep 0.1 unless Puma::IS_MRI # intermittent without
    assert_equal 1, exited.length
  end

  def test_autotrim
    pool = mutex_pool(1, 2)

    timeout = 0
    pool.auto_trim! timeout

    pool.<< [1, 2] do
      assert_equal 2, pool.spawned
    end

    start = Time.now
    Thread.pass until pool.spawned == 1 || Time.now - start > 1

    assert_equal 1, pool.spawned
  end

  def test_reap_only_dead_threads
    pool = mutex_pool(2,2) do
      th = Thread.current
      Thread.new {th.join; pool.signal}
      th.kill
    end

    assert_equal 2, pool.spawned

    pool << 1

    assert_equal 2, pool.spawned

    pool.reap

    assert_equal 1, pool.spawned

    pool << 2

    assert_equal 1, pool.spawned

    pool.reap

    assert_equal 0, pool.spawned
  end

  def test_auto_reap_dead_threads
    pool = mutex_pool(2,2) do
      th = Thread.current
      Thread.new {th.join; pool.signal}
      th.kill
    end

    timeout = 0
    pool.auto_reap! timeout

    assert_equal 2, pool.spawned

    pool << 1
    pool << 2

    start = Time.now
    Thread.pass until pool.spawned == 0 || Time.now - start > 1

    assert_equal 0, pool.spawned
  end

  def test_force_shutdown_immediately
    rescued = false

    pool = mutex_pool(0, 1) do
      begin
        pool.with_force_shutdown do
          pool.signal
          sleep
        end
      rescue Puma::ThreadPool::ForceShutdown
        rescued = true
      end
    end

    pool << 1
    pool.shutdown(0)

    assert_equal 0, pool.spawned
    assert rescued
  end

  def test_waiting_on_startup
    pool = new_pool(1, 2)
    assert_equal 1, pool.waiting
  end

  def test_shutdown_with_grace
    timeout = 0.01
    grace = 0.01

    # JRuby Array#<< is not thread-safe
    rescued = Queue.new
    pool = mutex_pool(2, 2, pool_shutdown_grace_time: grace) do
      begin
        pool.with_force_shutdown do
          pool.signal
          sleep
        end
      rescue Puma::ThreadPool::ForceShutdown
        rescued << Thread.current
        sleep
      end
    end

    pool << 1
    pool << 2

    pool.shutdown(timeout)

    assert_equal 0, pool.spawned
    assert_equal 2, rescued.length
    until rescued.empty?
      thread = rescued.pop
      refute thread.alive?
    end
  end

  def test_correct_waiting_count_for_killed_threads
    pool = new_pool(1, 1) { |_| }
    sleep 1

    # simulate our waiting processor thread getting killed for whatever reason
    pool.instance_eval { @processors[0].kill }
    sleep 1
    pool.reap
    sleep 1

    pool << 0
    sleep 1
    assert_equal 0, pool.backlog
  end

  def test_pool_capacity_never_negative
    pool = mutex_pool(5,5) do
      th = Thread.current
      Thread.new {th.join; pool.signal}
      th.kill
    end

    pool << 1
    pool << 1
    pool << 1
    pool << 1
    pool << 1

    assert_equal 5, pool.spawned

    pool.min = 1
    pool.max = 1

    assert_equal 0, pool.pool_capacity
  end

  def test_io_threads_allowed_over_the_limit
    mutex = Mutex.new
    mutex.lock
    queue = Queue.new
    concurrent_threads = []
    pool = new_pool(0, 4, max_io_threads: 4) do |processor, io_bound_thread|
      processor.mark_as_io_thread! if io_bound_thread
      queue << Thread.current
      mutex.synchronize { }
    end

    8.times do
      pool << true
      concurrent_threads << queue.pop
    end
    assert_equal 8, concurrent_threads.size
    refute_predicate pool, :can_spawn_processor?
  ensure
    mutex.unlock
    pool.shutdown(1)
  end

  def test_io_threads_over_the_limit_count_as_normal_threads
    mutex = Mutex.new
    mutex.lock
    queue = Queue.new
    concurrent_threads = []
    pool = new_pool(0, 2, max_io_threads: 2) do |processor, io_bound_thread|
      processor.mark_as_io_thread! if io_bound_thread
      queue << Thread.current
      mutex.synchronize { }
    end

    [true, true, true, false].each do |io_bound_thread|
      pool << io_bound_thread
      concurrent_threads << queue.pop
    end
    assert_equal 4, concurrent_threads.size
    refute_predicate pool, :can_spawn_processor?
  ensure
    mutex.unlock
    pool.shutdown(1)
  end

  def test_extra_io_threads_release_spawned_count_on_exit
    mutex = Mutex.new
    mutex.lock
    queue = Queue.new
    pool = new_pool(0, 2, max_io_threads: 2) do |processor, io_bound_thread|
      processor.mark_as_io_thread! if io_bound_thread
      queue << Thread.current
      mutex.synchronize { }
    end

    4.times { pool << true }
    4.times { queue.pop }
    assert_equal 4, pool.spawned

    mutex.unlock

    Timeout.timeout(1) { sleep 0.01 until pool.spawned == 2 }
    assert_equal 2, pool.spawned
    assert_equal 2, pool.pool_capacity
  ensure
    mutex.unlock if mutex.owned?
    pool.shutdown(1)
  end
end
