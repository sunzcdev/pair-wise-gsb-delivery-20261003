# frozen_string_literal: true
require File.expand_path("../helper", __FILE__)
require "json"
require "stringio"

class TestRakeExecutionTrace < Rake::TestCase # :nodoc:
  include Rake

  def setup
    super

    Task.clear
    @app = Rake.application
    @trace = @app.execution_trace
    @events = []
    @trace.clear
  end

  def teardown
    @app.thread_pool.join

    super
  end

  def observe
    @trace.add_observer { |event| @events << event }
  end

  def names(event_name)
    @events.select { |event| event[:event] == event_name }.map { |event| event[:task] }
  end

  def test_records_dependency_wait_and_action_lifecycle
    observe
    task(:t) { }
    Task[:t].invoke

    assert_equal [:action_started, :action_succeeded], @events.map { |e| e[:event] }
    started, finished = @events
    assert_equal "t", started[:task]
    assert_equal 0, started[:action_index]
    assert_kind_of Float, started[:time]
    assert_operator finished[:duration], :>=, 0
    assert_equal started[:invocation], finished[:invocation]
  end

  def test_records_dependency_wait_with_prerequisite_names
    observe
    task(:leaf) { }
    task(:top => :leaf) { }
    Task[:top].invoke

    waits = @events.select { |event| event[:event] == :dependency_wait }
    assert_equal ["top"], waits.map { |event| event[:task] }
    assert_equal ["leaf"], waits.first[:prerequisites]
    assert_operator waits.first[:duration], :>=, 0
  end

  def test_action_of_shared_prerequisite_is_counted_once
    observe
    task(:shared) { }
    task(:left => :shared) { }
    task(:right => :shared) { }
    task(:both => [:left, :right]) { }
    Task[:both].invoke

    assert_equal 1, names(:action_started).count("shared")
    assert_equal 1, names(:action_succeeded).count("shared")
    assert_equal 4, names(:action_started).size
  end

  def test_second_invocation_is_skipped_as_already_invoked
    observe
    task(:t) { }
    Task[:t].invoke
    Task[:t].invoke

    assert_equal 1, names(:action_started).size
    assert_equal 1, names(:skipped_already_invoked).size
    invocations = @events.select { |e| e[:event] == :skipped_already_invoked }
    refute_equal @events.first[:invocation], invocations.first[:invocation]
  end

  def test_reenable_is_a_new_invocation_with_its_own_action_count
    observe
    task(:t) { }
    Task[:t].invoke
    Task[:t].reenable
    Task[:t].invoke

    assert_equal 2, names(:action_started).size
    ids = @events.select { |e| e[:event] == :action_started }.map { |e| e[:invocation] }
    assert_equal ids.uniq, ids
  end

  def test_up_to_date_file_task_is_skipped_not_needed
    observe
    target = "artifact"
    create_file(target)
    file(target) { flunk "should not run" }
    Task[target].invoke

    assert_equal [target], names(:skipped_not_needed)
    assert_empty names(:action_started)
  end

  def test_failure_records_only_the_action_that_raised
    observe
    task(:bad) { raise ArgumentError, "boom" }
    task(:middle => :bad) { }
    task(:outer => :middle) { }

    assert_raises(ArgumentError) { Task[:outer].invoke }

    failures = @events.select { |event| event[:event] == :action_failed }
    assert_equal ["bad"], failures.map { |event| event[:task] }
    assert_equal "ArgumentError", failures.first[:exception]
    assert_empty names(:action_succeeded)
  end

  def test_failure_records_exception_cause_chain
    observe
    task(:bad) do
      begin
        raise IOError, "inner"
      rescue IOError
        raise RuntimeError, "outer"
      end
    end

    assert_raises(RuntimeError) { Task[:bad].invoke }

    failure = @events.find { |event| event[:event] == :action_failed }
    assert_equal "RuntimeError", failure[:exception]
    assert_equal ["IOError"], failure[:causes]
  end

  def test_nested_failure_keeps_the_real_failure_node
    observe
    task(:bad) { raise "boom" }
    task(:middle => :bad) { }
    task(:outer => :middle) { }

    assert_raises(RuntimeError) { Task[:outer].invoke }

    assert_equal 1, names(:action_failed).size
    failure = @events.find { |event| event[:event] == :action_failed }
    assert_equal "bad", failure[:task]
    parent = @events.find { |event| event[:invocation] == failure[:parent] }
    assert_equal "middle", parent[:task]
  end

  def test_events_are_frozen_hashes_with_stable_identity
    observe
    task(:t) { }
    Task[:t].invoke

    @events.each do |event|
      assert event.frozen?
      assert_kind_of Hash, event
      assert_kind_of Integer, event[:invocation]
      assert event.key?(:parent)
      assert_kind_of Float, event[:time]
    end
    assert_nil @events.first[:parent]
  end

  def test_event_times_are_monotonic
    observe
    task(:a) { }
    task(:b) { }
    task(:both => [:a, :b]) { }
    Task[:both].invoke

    times = @events.map { |event| event[:time] }
    assert_equal times.sort, times
  end

  def test_observer_error_does_not_repeat_the_action
    runs = 0
    @trace.add_observer { |_event| raise "observer bug" }
    task(:once) { runs += 1 }
    Task[:once].invoke
    Task[:once].invoke

    assert_equal 1, runs
    assert_equal ["RuntimeError"], @trace.observer_failures
    assert_empty @trace.observers
  end

  def test_observer_error_leaves_text_trace_usable
    @trace.add_observer { |_event| raise "observer bug" }
    task(:t) { }
    _, err = capture_output do
      @app.set_default_options # reset trace output IO
      @app.options.trace = true
      Task[:t].invoke
    end

    assert_match(/invoke t/i, err)
    assert_match(/execute t/i, err)
  ensure
    @app.options.trace = false
  end

  def test_json_output_contains_no_environment_or_command
    secret = "top-secret-token"
    old = ENV["RAKE_TRACE_SECRET"]
    ENV["RAKE_TRACE_SECRET"] = secret
    io = StringIO.new
    @trace.output = io
    task(:t) { }
    Task[:t].invoke

    records = io.string.lines.map { |line| JSON.parse(line) }
    assert_equal %w[action_started action_succeeded], records.map { |r| r["event"] }
    allowed = %w[event task invocation parent time duration action_index exception causes prerequisites]
    records.each do |record|
      assert_empty record.keys - allowed
    end
    refute_includes io.string, secret
    ENV.each do |name, value|
      next if value.nil? || value.empty?
      refute_includes io.string, "#{name}=#{value}"
    end
  ensure
    ENV["RAKE_TRACE_SECRET"] = old
  end

  def test_output_only_mode_does_not_accumulate_events
    io = StringIO.new
    @trace.output = io
    task(:t) { }
    Task[:t].invoke

    assert_empty @trace.events
    assert_equal 2, io.string.lines.size
  end

  def test_recording_is_disabled_without_observers_or_output
    task(:t) { }
    Task[:t].invoke

    assert_empty @trace.events
    refute @trace.enabled?
  end

  def test_execute_outside_invoke_records_its_own_invocation
    observe
    task(:t) { }
    Task[:t].execute

    assert_equal [:action_started, :action_succeeded], @events.map { |e| e[:event] }
    assert_nil @events.first[:parent]
    assert_kind_of Integer, @events.first[:invocation]
  end

  def test_clear_discards_recorded_events_without_breaking_tracing
    observe
    task(:t) { }
    Task[:t].invoke
    assert_equal 2, @trace.events.size

    @trace.clear
    assert_empty @trace.events

    Task[:t].reenable
    Task[:t].invoke
    assert_equal 2, @trace.events.size
  end
end
