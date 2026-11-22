# frozen_string_literal: true

require_relative "helper"

require "puma/thread_pool"

class TestThreadPool < PumaTest

  parallelize_me!

  def teardown
    @pool.shutdown(1) if defined?(@pool)
  end

  def new_pool(min, max, max_io_threads: 0, pool_shutdown_grace_time: nil,
      queue_timeout: nil, queue_timeout_handler: nil, &block)
    block = proc { } unless block
    options = {
      min_threads: min,
      max_threads: max,
      max_io_threads: max_io_threads,
      pool_shutdown_grace_time: pool_shutdown_grace_time,
      queue_timeout: queue_timeout,
    }
    @pool = Puma::ThreadPool.new("tst", options,
      queue_timeout_handler: queue_timeout_handler, &block)
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

  def test_queue_timeout_expires_work_while_every_thread_is_busy
    started = Queue.new
    release = Queue.new
    timed_out = Queue.new

    pool = new_pool(1, 1, queue_timeout: 0.05, queue_timeout_handler: ->(work, waited) {
      timed_out << [work, waited]
    }) do |_processor, work|
      started << work
      release.pop if work == :blocker
    end

    pool << :blocker
    assert_equal :blocker, started.pop

    # Nobody is free to take this item, so only the timer can drop it.
    pool << :queued
    work, waited = Timeout.timeout(2) { timed_out.pop }

    assert_equal :queued, work
    assert_operator waited, :>=, 0.05
    assert_equal 0, pool.backlog
  ensure
    release&.close
    pool&.shutdown(1)
  end

  def test_queue_timeout_ignores_work_that_is_not_ready
    started = Queue.new
    release = Queue.new
    timed_out = Queue.new

    pool = new_pool(1, 1, queue_timeout: 0.05, queue_timeout_handler: ->(work, _) {
      timed_out << work
    }) do |_processor, work|
      started << work
      release.pop if work == :blocker
    end

    pool << :blocker
    assert_equal :blocker, started.pop

    # Work that cannot run yet has no deadline, so it must not be dropped.
    pool.add_work :unread, ready: false
    sleep 0.2

    assert_equal 1, pool.backlog
    assert timed_out.empty?, "work that was not ready was expired by the queue timeout"

    release << true
    assert_equal :unread, Timeout.timeout(2) { started.pop }
  ensure
    release&.close
    pool&.shutdown(1)
  end

  def test_queue_timeout_boundary_has_exactly_one_outcome
    now = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    item = Puma::ThreadPool::QueuedWork.new(:item, now, 0.05)

    refute item.expired?(now + 0.049_9), "expired before the deadline"
    assert item.expired?(now + 0.05), "not expired at the deadline"
    assert item.expired?(now + 0.05_1), "not expired after the deadline"

    pool = new_pool(0, 0, queue_timeout: 10, queue_timeout_handler: ->(_work, _waited) {})

    # Stop the timer so this test controls when expiry is observed.
    timer = pool.instance_variable_get(:@queue_timeout_thread)
    timer.kill
    timer.join
    pool.instance_variable_set(:@queue_timeout_thread, nil)

    # A request whose deadline has long passed.
    expired_item = Puma::ThreadPool::QueuedWork.new(:expired, now - 1, 0.05)
    pool.with_mutex { pool.instance_variable_get(:@todo) << expired_item }

    # Taking work at or after the deadline yields the expiration, not the work.
    work = expired = nil
    pool.with_mutex do
      expired = pool.send(:take_expired_locked)
      work = pool.send(:take_work_locked) if expired.empty?
    end
    assert_nil work
    assert_equal [expired_item], expired

    # It has been removed, so a later take sees nothing to expire.
    pool.with_mutex do
      expired = pool.send(:take_expired_locked)
      work = pool.send(:take_work_locked) if expired.empty?
    end
    assert_nil work
    assert_empty expired
  ensure
    pool&.shutdown(1)
  end

  def test_queue_timeout_runs_or_expires_each_item_exactly_once
    runs = Queue.new
    expirations = Queue.new

    pool = new_pool(1, 1, queue_timeout: 0.01, queue_timeout_handler: ->(work, _) {
      expirations << work
    }) do |_processor, work|
      runs << work
    end

    items = (1..50).to_a
    items.each { |item| pool << item }
    Timeout.timeout(10) { sleep 0.001 until runs.size + expirations.size >= items.size }

    handled = []
    handled << runs.pop until runs.empty?
    handled << expirations.pop until expirations.empty?

    assert_equal items.sort, handled.sort
    assert_equal items.size, handled.uniq.size
    assert_equal 0, pool.backlog
  ensure
    pool&.shutdown(1)
  end

  def test_queue_timeout_timer_is_woken_by_new_work
    started = Queue.new
    release = Queue.new
    timed_out = Queue.new

    pool = new_pool(1, 1, queue_timeout: 5, queue_timeout_handler: ->(work, _) {
      timed_out << work
    }) do |_processor, work|
      started << work
      release.pop if work == :blocker
    end

    pool << :blocker
    assert_equal :blocker, started.pop

    pool << :later
    sleep 0.05

    assert_equal 1, pool.backlog
    assert timed_out.empty?
  ensure
    release&.close
    pool&.shutdown(1)
  end

  def test_queue_timeout_drains_instead_of_expiring_during_shutdown
    started = Queue.new
    release = Queue.new
    timed_out = Queue.new
    served = Queue.new

    # Long enough that shutdown always wins the race against the timer.
    pool = new_pool(1, 1, queue_timeout: 30, queue_timeout_handler: ->(work, _) {
      timed_out << work
    }) do |_processor, work|
      started << work
      release.pop if work == :blocker
      served << work
    end

    pool << :blocker
    assert_equal :blocker, started.pop
    pool << :queued

    # Shutdown drains the queue rather than expiring it.
    release << true
    pool.shutdown(1)

    assert timed_out.empty?, "queue timeout handler ran during shutdown"
    assert_equal [:blocker, :queued], [served.pop, served.pop].sort
    assert_nil pool.instance_variable_get(:@queue_timeout_thread)
  ensure
    release&.close
  end

  def test_queue_timeout_expires_only_ready_items_when_interleaved
    timed_out = Queue.new
    ran = Queue.new

    # No threads, so the queue is only drained by the explicit takes below.
    pool = new_pool(0, 0, queue_timeout: 0.1, queue_timeout_handler: ->(work, _) {
      timed_out << work
    }) do |_processor, work|
      ran << work
    end

    pool.add_work :unread, ready: false
    pool.add_work :ready_1

    # Stop the timer so this test controls when expiry is observed.
    timer = pool.instance_variable_get(:@queue_timeout_thread)
    timer.kill
    timer.join
    pool.instance_variable_set(:@queue_timeout_thread, nil)

    # Wait past the deadline, then read from the queue in order: only ready
    # work may be dropped.
    sleep 0.15

    work = expired = nil
    pool.with_mutex do
      expired = pool.send(:take_expired_locked)
      work = pool.send(:take_work_locked) if expired.empty?
    end

    assert_equal [:ready_1], expired.map(&:work)
    assert_nil work
    assert_equal [:unread], pool.with_mutex { pool.instance_variable_get(:@todo).map(&:work) }
    assert ran.empty?
  ensure
    pool&.shutdown(1)
  end

  def test_queue_timeout_requires_a_handler
    assert_raises(ArgumentError) do
      Puma::ThreadPool.new("tst", { min_threads: 0, max_threads: 1, queue_timeout: 1 })
    end
  end

  def test_queue_timeout_rejects_non_positive_values
    assert_raises(ArgumentError) do
      Puma::ThreadPool.new("tst", { min_threads: 0, max_threads: 1, queue_timeout: 0 },
        queue_timeout_handler: ->(_work, _waited) {})
    end
  end
end
