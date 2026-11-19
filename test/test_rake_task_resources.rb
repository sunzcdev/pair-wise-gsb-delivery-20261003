# frozen_string_literal: true
require File.expand_path("../helper", __FILE__)
require "thread"

class TestRakeTaskResources < Rake::TestCase # :nodoc:
  include Rake

  def setup
    super
    Task.clear
    @runs = []
    @mutex = Mutex.new
    @concurrency = 0
    @max_concurrency = 0
  end

  def teardown
    Rake.application.thread_pool.join

    super
  end

  def resource_manager
    Rake.application.resource_manager
  end

  # Record the execution of an action, tracking the maximum number of
  # actions running concurrently.
  def add_run(obj, delay=0.01)
    @mutex.synchronize do
      @concurrency += 1
      @max_concurrency = [@max_concurrency, @concurrency].max
      @runs << [:start, obj]
    end
    sleep delay
    @mutex.synchronize do
      @runs << [:end, obj]
      @concurrency -= 1
    end
  end

  def started
    @mutex.synchronize { @runs.select { |ev, _| ev == :start }.map(&:last) }
  end

  def test_resources_option_is_recorded_on_the_task
    task :a, resources: { compiler: 1 }
    assert_equal({ compiler: 1 }, Task[:a].resources)
  end

  def test_resources_option_combines_with_dependencies_and_order_only
    task :b
    task :c
    task :a => [:b], order_only: [:c], resources: { "compiler" => 2 }
    assert_equal({ compiler: 2 }, Task[:a].resources)
    assert_equal ["b"], Task[:a].prerequisites
    assert_equal ["c"], Task[:a].order_only_prerequisites
  end

  def test_task_without_declaration_has_no_requirements
    task :a
    assert_equal({}, Task[:a].resources)
  end

  def test_resource_limit_dsl_declares_capacity
    resource_limit :compiler, 3
    assert_equal 3, resource_manager.limit(:compiler)
  end

  def test_requirements_are_acquired_after_prerequisites_and_released
    resource_limit :compiler, 1
    task :dep do add_run(:dep) end
    task :a => [:dep], resources: { compiler: 1 } do
      # While the action runs, the quota must be held.
      @mutex.synchronize { @runs << [:available, resource_manager.available(:compiler)] }
      add_run(:a)
    end
    Task[:a].invoke
    assert_equal [[:start, :dep], [:end, :dep], [:available, 0],
                  [:start, :a], [:end, :a]], @runs
    assert_equal 1, resource_manager.available(:compiler)
  end

  def test_illegal_requirement_is_rejected_before_anything_runs
    resource_limit :compiler, 1
    task :dep do add_run(:dep) end
    task :bad => [:dep], resources: { compiler: 2 } do add_run(:bad) end
    assert_raises(Rake::InvalidResourceRequirement) { Task[:bad].invoke }
    assert_equal [], @runs, "neither prerequisites nor actions may run"
  end

  def test_unknown_resource_is_rejected_before_anything_runs
    task :bad, resources: { flux_capacitor: 1 } do add_run(:bad) end
    assert_raises(Rake::InvalidResourceRequirement) { Task[:bad].invoke }
    assert_equal [], @runs
  end

  def test_failed_task_returns_its_resources
    resource_limit :compiler, 1
    task :bad, resources: { compiler: 1 } do raise "boom" end
    assert_raises(RuntimeError) { Task[:bad].invoke }
    assert_equal 1, resource_manager.available(:compiler)

    ran = false
    task :good, resources: { compiler: 1 } do ran = true end
    Task[:good].invoke
    assert ran, "a later task must be able to acquire the returned quota"
  end

  def test_cancelled_task_returns_its_resources
    resource_limit :compiler, 1
    started_q = Queue.new
    task :victim, resources: { compiler: 1 } do
      started_q << true
      sleep 30
    end
    t = Thread.new do
      begin
        Task[:victim].invoke
      rescue Exception
      end
    end
    started_q.pop
    t.kill
    t.join(5)
    assert_equal 1, resource_manager.available(:compiler)
  end

  def test_shared_dependency_with_resources_runs_only_once
    resource_limit :compiler, 1
    task :common, resources: { compiler: 1 } do add_run(:common) end
    task a: [:common]
    task b: [:common]
    multitask all: [:a, :b]
    Task[:all].invoke
    assert_equal 1, started.count(:common)
  end

  def test_resource_limited_tasks_are_admitted_one_at_a_time
    resource_limit :compiler, 1
    Rake.application.options.thread_pool_size = 4
    task :a, resources: { compiler: 1 } do add_run(:a, 0.05) end
    task :b, resources: { compiler: 1 } do add_run(:b, 0.05) end
    task :c, resources: { compiler: 1 } do add_run(:c, 0.05) end
    multitask all: [:a, :b, :c]
    Task[:all].invoke

    assert_equal 3, started.size
    assert_equal 1, @max_concurrency,
      "a single compiler quota must serialize the actions"
    # Each start must be matched by its end before the next start.
    events = @mutex.synchronize { @runs.dup }
    events.each_cons(2) do |first, second|
      assert_equal :end, first.first if second.first == :start
    end
  end

  def test_distinct_resources_are_independent
    resource_limit :compiler, 1
    resource_limit :linker, 1
    Rake.application.options.thread_pool_size = 2
    task :compile, resources: { compiler: 1 } do add_run(:compile, 0.1) end
    task :link, resources: { linker: 1 } do add_run(:link, 0.1) end
    multitask all: [:compile, :link]
    Task[:all].invoke

    assert_equal 2, started.size
    assert_equal 2, @max_concurrency,
      "compiler and linker quotas are independent and run in parallel"
  end

  def test_waiting_for_resources_spawns_no_extra_threads
    resource_limit :compiler, 1
    Rake.application.options.thread_pool_size = 2
    task :a, resources: { compiler: 1 } do add_run(:a, 0.1) end
    task :b, resources: { compiler: 1 } do add_run(:b, 0.1) end
    task :c, resources: { compiler: 1 } do add_run(:c, 0.1) end
    task :d, resources: { compiler: 1 } do add_run(:d, 0.1) end
    multitask all: [:a, :b, :c, :d]
    Task[:all].invoke
    Rake.application.thread_pool.join

    stats = Rake.application.thread_pool.statistics
    assert_equal 4, started.size
    assert stats[:total_threads_in_play] <= 2,
      "expected at most 2 worker threads, got #{stats[:total_threads_in_play]}"
  end

  def test_admission_order_is_serialized_in_request_order
    # With a single quota and a single worker thread, tasks are
    # admitted strictly one at a time; the recording actions show that
    # every admitted task ran exactly once and never overlapped.
    resource_limit :compiler, 1
    Rake.application.options.thread_pool_size = 1
    task :a, resources: { compiler: 1 } do add_run(:a, 0.05) end
    task :b, resources: { compiler: 1 } do add_run(:b, 0.05) end
    multitask all: [:a, :b]
    Task[:all].invoke

    events = @mutex.synchronize { @runs.dup }
    assert_equal 2, started.size
    assert_equal 1, @max_concurrency
    assert_equal :end, events[1].first, "actions must not overlap"
    assert_equal events[0].last, events[1].last,
      "each action must finish before the next is admitted"
  end
end
