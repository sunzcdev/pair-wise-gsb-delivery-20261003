# frozen_string_literal: true

require "test_helper"

class SolidQueue::RecurringExecutionTest < ActiveSupport::TestCase
  self.use_transactional_tests = false

  class WitnessJob < ApplicationJob
    def perform(value)
      JobBuffer.add(value)
    end
  end

  START = Time.utc(2026, 1, 1)

  setup do
    SolidQueue::RecurringExecution.delete_all
    SolidQueue::Job.delete_all
  end

  teardown do
    SolidQueue::RecurringExecution.delete_all
    SolidQueue::Job.delete_all
  end

  test "record enqueues once for an instant and skips a repeat" do
    assert_difference -> { SolidQueue::Job.count }, +1 do
      assert SolidQueue::RecurringExecution.record("task", START) { WitnessJob.perform_later("first") }
    end

    assert_no_difference -> { SolidQueue::Job.count } do
      assert_equal false, SolidQueue::RecurringExecution.record("task", START) { WitnessJob.perform_later("duplicate") }
    end

    assert_equal [ "first" ], job_values
  end

  test "cleanup folds a dense run into one row and still rejects the folded instants" do
    record_run(task_key: "task", from: START, count: 5, every: 60)
    detach_jobs

    assert_difference -> { SolidQueue::RecurringExecution.count }, -4 do
      SolidQueue::RecurringExecution.clear_in_batches(retain: nil)
    end

    [ 0, 1, 2, 3, 4 ].each do |index|
      at = START + index.minutes
      assert_equal false, SolidQueue::RecurringExecution.record("task", at) { WitnessJob.perform_later("repeat-#{index}") },
        "folded instant #{at.iso8601} should still be rejected"
    end

    assert_empty job_values
  end

  test "cleanup keeps a real hole recordable while folding the runs around it" do
    record_run(task_key: "task", from: START, count: 3, every: 60)
    record_run(task_key: "task", from: START + 4.minutes, count: 3, every: 60)
    detach_jobs

    SolidQueue::RecurringExecution.clear_in_batches(retain: nil)

    assert_equal 2, SolidQueue::RecurringExecution.count
    assert_equal [ START, START + 4.minutes ], SolidQueue::RecurringExecution.order(:run_at).map(&:run_at)

    hole = START + 3.minutes
    assert_difference -> { SolidQueue::Job.count }, +1 do
      assert SolidQueue::RecurringExecution.record("task", hole) { WitnessJob.perform_later("hole") }
    end
    assert_equal [ "hole" ], job_values
  end

  test "cleanup leaves occurrences younger than the retention window at their own rows" do
    record_run(task_key: "task", from: START, count: 3, every: 60)
    detach_jobs

    travel_to START + 3.minutes do
      # With a five-minute window the cutoff sits before the first run, so none
      # of these occurrences is old enough to compact yet.
      SolidQueue::RecurringExecution.clear_in_batches(retain: 5.minutes)

      assert_empty SolidQueue::RecurringExecution.folded
      assert_equal 3, SolidQueue::RecurringExecution.count
    end
  end

  test "cleanup folds history older than the retention window" do
    record_run(task_key: "task", from: START, count: 3, every: 60)
    detach_jobs

    travel_to START + 10.minutes do
      SolidQueue::RecurringExecution.clear_in_batches(retain: 5.minutes)

      assert_equal 1, SolidQueue::RecurringExecution.count
      assert SolidQueue::RecurringExecution.sole.folded?
    end
  end

  test "folding keeps runs continuous across pages" do
    record_run(task_key: "task", from: START, count: 7, every: 60)
    detach_jobs

    SolidQueue::RecurringExecution.clear_in_batches(retain: nil, batch_size: 2)

    assert_equal 1, SolidQueue::RecurringExecution.count
    folded = SolidQueue::RecurringExecution.sole
    assert_equal START, folded.run_at
    assert_equal START + 6.minutes, folded.folded_through
    assert_equal 60, folded.folded_interval
  end

  test "folding does not claim instants that were never recorded" do
    # Two runs of three, spaced by a 120s gap that isn't their 60s cadence, so
    # the folded runs can't swallow the instants in between.
    record_run(task_key: "task", from: START, count: 3, every: 60)
    record_run(task_key: "task", from: START + 4.minutes, count: 3, every: 60)
    detach_jobs

    SolidQueue::RecurringExecution.clear_in_batches(retain: nil)

    assert_not SolidQueue::RecurringExecution.recorded?("task", START + 3.minutes)
    [ START, START + 60, START + 2.minutes, START + 4.minutes, START + 5.minutes, START + 6.minutes ].each do |at|
      assert SolidQueue::RecurringExecution.recorded?("task", at), "#{at.iso8601} was recorded and must stay covered"
    end
  end

  test "two schedulers ticking a folded instant enqueue nothing" do
    record_run(task_key: "task", from: START, count: 3, every: 60)
    detach_jobs
    SolidQueue::RecurringExecution.clear_in_batches(retain: nil)

    folded_at = START + 60
    results = Concurrent::Array.new
    threads = 2.times.map do
      Thread.new do
        SolidQueue::Record.connection_pool.with_connection do
          results << SolidQueue::RecurringExecution.record("task", folded_at) { WitnessJob.perform_later("repeat") }
        end
      end
    end
    threads.each(&:join)

    assert_equal [ false, false ], results
    assert_equal 0, SolidQueue::Job.count
  end

  test "cleanup leaves executions whose jobs are still around untouched" do
    record_run(task_key: "task", from: START, count: 3, every: 60)

    assert_no_difference -> { SolidQueue::RecurringExecution.count } do
      SolidQueue::RecurringExecution.clear_in_batches(retain: nil)
    end
    assert_empty SolidQueue::RecurringExecution.folded
  end

  test "folding doesn't merge a plain row into a folded run of a different spacing" do
    # A 60s run, then a lone row 90s after its last occurrence: the gap isn't
    # the run's spacing, so the lone row mustn't be swallowed by it.
    record_detached START, folded_through: START + 60, folded_interval: 60
    record_detached START + 2.minutes + 30

    SolidQueue::RecurringExecution.clear_in_batches(retain: nil)

    assert_equal [ START, START + 2.minutes + 30 ], SolidQueue::RecurringExecution.order(:run_at).map(&:run_at)
    assert_not SolidQueue::RecurringExecution.recorded?("task", START + 90)
  end

  test "job deletion detaches its recurring execution instead of deleting it" do
    record_run(task_key: "task", from: START, count: 1, every: 60)
    execution = SolidQueue::RecurringExecution.sole
    job = SolidQueue::Job.sole

    job.destroy!

    assert SolidQueue::RecurringExecution.exists?(execution.id)
    assert_nil execution.reload.job_id
  end

  test "bulk job cleanup leaves the recurring execution behind through the foreign key" do
    record_run(task_key: "task", from: START, count: 1, every: 60)
    execution = SolidQueue::RecurringExecution.sole

    SolidQueue::Job.where(id: SolidQueue::Job.pluck(:id)).delete_all

    assert SolidQueue::RecurringExecution.exists?(execution.id)
    assert_nil execution.reload.job_id
  end

  private
    def record_detached(run_at, task_key: "task", folded_through: nil, folded_interval: nil)
      SolidQueue::RecurringExecution.insert!(
        { task_key: task_key, run_at: run_at, folded_through: folded_through, folded_interval: folded_interval, job_id: nil }
      )
    end

    def record_run(task_key:, from:, count:, every:)
      count.times do |index|
        at = from + (index * every)
        SolidQueue::RecurringExecution.record(task_key, at) { WitnessJob.perform_later("run-#{at.to_i}") }
      end
    end

    def detach_jobs
      SolidQueue::Job.destroy_all
    end

    def job_values
      SolidQueue::Job.order(:id).map { |job| job.arguments["arguments"].first }
    end
end
