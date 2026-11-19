# frozen_string_literal: true
require_relative "helper"

class TestRakeRetryPlan < Rake::TestCase
  def setup
    super
    @runs = Hash.new(0)
  end

  def flaky_task(name, fail_times: 1, retryable: true)
    runs = @runs
    t = task(name) do
      runs[name] += 1
      raise "boom #{name}" if runs[name] <= fail_times
    end
    t.retryable! if retryable
    t
  end

  def test_plan_lists_failed_nodes_and_affected_targets
    flaky_task(:shared)
    task a: :shared
    task b: :shared
    task all: [:a, :b]

    assert_raises(RuntimeError) { Rake::Task[:all].invoke }

    plan = Rake.application.retry_plan("all")

    assert_equal ["all"], plan.targets.map(&:name)
    # "b" was never reached because "a" failed first, so only the
    # actually failed nodes are re-admitted.
    assert_equal %w[all a shared].sort, plan.retryable_task_names.sort
    assert_includes plan.skipped.map(&:name), "b"
    assert plan.executable?
  end

  def test_unaffected_targets_are_not_listed
    flaky_task(:bad)
    task good: []
    task bad_parent: :bad
    Rake::Task[:good].invoke

    assert_raises(RuntimeError) { Rake::Task[:bad_parent].invoke }

    plan = Rake.application.retry_plan("good", "bad_parent")

    assert_equal ["bad_parent"], plan.targets.map(&:name)
  end

  def test_successful_nodes_are_not_reexecuted
    flaky_task(:shared)
    task(:ok) { @runs[:ok] += 1 }
    task all: [:ok, :shared]

    assert_raises(RuntimeError) { Rake::Task[:all].invoke }
    assert_equal 1, @runs[:ok]

    plan = Rake.application.retry_plan("all")
    assert_includes plan.skipped.map(&:name), "ok"

    plan.execute

    assert_equal 1, @runs[:ok], "successful node must not run again"
    assert_equal 2, @runs[:shared]
  end

  def test_shared_dependencies_are_retried_only_once
    flaky_task(:shared)
    task a: :shared
    task b: :shared
    task all: [:a, :b]

    assert_raises(RuntimeError) { Rake::Task[:all].invoke }

    plan = Rake.application.retry_plan("all")

    assert_equal 1, plan.retryable_task_names.count("shared"),
      "shared dependency must appear exactly once in the plan"

    plan.execute
    assert_equal 2, @runs[:shared], "shared dependency ran once per build"
  end

  def test_failure_history_is_preserved_across_retries
    t = flaky_task(:flaky, fail_times: 2)

    assert_raises(RuntimeError) { t.invoke }
    assert_equal 1, t.failure_history.size

    plan = Rake.application.retry_plan("flaky")
    assert_raises(RuntimeError) { plan.execute }

    assert_equal 2, t.failure_history.size
    assert_equal ["boom flaky", "boom flaky"],
      t.failure_history.map(&:message)
    assert t.failure_history.all? { |ex| ex.backtrace },
      "historical backtraces must be preserved"
  end

  def test_side_effect_tasks_are_not_retryable_by_default
    task(:danger) { raise "boom" }

    assert_raises(RuntimeError) { Rake::Task[:danger].invoke }

    plan = Rake.application.retry_plan("danger")

    refute plan.executable?
    assert_match(/side effects/, plan.rejections["danger"])

    ex = assert_raises(Rake::RejectedRetryNodesError) { plan.execute }
    assert_match(/danger/, ex.message)
    assert_match(/side effects/, ex.message)
  end

  def test_user_declared_non_retryable_node_reports_reason
    t = task(:deploy) { raise "nope" }
    t.non_retryable!("deploy is not idempotent")

    assert_raises(RuntimeError) { t.invoke }

    plan = Rake.application.retry_plan("deploy")

    assert_equal "deploy is not idempotent", plan.rejections["deploy"]
    ex = assert_raises(Rake::RejectedRetryNodesError) { plan.execute }
    assert_match(/deploy is not idempotent/, ex.message)
  end

  def test_retryable_declaration_allows_retry
    flaky_task(:flaky)

    assert_raises(RuntimeError) { Rake::Task[:flaky].invoke }

    plan = Rake.application.retry_plan("flaky")
    assert plan.executable?
    assert plan.execute

    assert_equal 2, @runs[:flaky]
    refute Rake::Task[:flaky].failed?
  end

  def test_file_tasks_are_retryable_by_default
    file("out.txt") { raise "boom" }
    assert Rake::Task["out.txt"].retryable?
  end

  def test_plain_task_with_actions_is_not_retryable_by_default
    task(:t) { }
    refute Rake::Task[:t].retryable?
  end

  def test_task_without_actions_is_retryable
    task(:t)
    assert Rake::Task[:t].retryable?
  end

  def test_stale_plan_version_is_rejected
    flaky_task(:flaky)
    assert_raises(RuntimeError) { Rake::Task[:flaky].invoke }

    stale = Rake.application.retry_plan("flaky")
    Rake.application.retry_plan("flaky") # newer plan supersedes

    ex = assert_raises(Rake::StaleRetryPlanError) { stale.execute }
    assert_match(/stale/, ex.message)
  end

  def test_plan_cannot_be_executed_twice
    flaky_task(:flaky)
    assert_raises(RuntimeError) { Rake::Task[:flaky].invoke }

    plan = Rake.application.retry_plan("flaky")
    assert plan.execute

    assert_raises(Rake::StaleRetryPlanError) { plan.execute }
  end

  def test_retry_does_not_clear_application_state
    flaky_task(:flaky)
    invoked = false
    task(:other) { invoked = true }
    Rake::Task[:other].invoke

    assert_raises(RuntimeError) { Rake::Task[:flaky].invoke }

    plan = Rake.application.retry_plan("flaky")
    plan.execute

    assert Rake::Task.task_defined?(:other), "tasks must remain defined"
    assert Rake::Task.task_defined?(:flaky)
    assert Rake::Task[:other].already_invoked,
      "unrelated task invocation state must be preserved"
  end

  def test_plan_for_successful_target_is_empty
    task(:ok) { @runs[:ok] += 1 }
    Rake::Task[:ok].invoke

    plan = Rake.application.retry_plan("ok")

    assert_empty plan.targets
    assert_empty plan.entries
    assert plan.execute
    assert_equal 1, @runs[:ok]
  end

  def test_summary_mentions_entries_targets_and_rejections
    task(:danger) { raise "boom" }
    assert_raises(RuntimeError) { Rake::Task[:danger].invoke }

    plan = Rake.application.retry_plan("danger")
    summary = plan.summary

    assert_match(/danger/, summary)
    assert_match(/side effects/, summary)
  end
end
