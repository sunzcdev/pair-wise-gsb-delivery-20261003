require "test_helper"

class SolidQueue::JobDedupTest < ActiveSupport::TestCase
  self.use_transactional_tests = false

  class RaisingReadyExecution < StandardError; end

  class DiscardableDeduplicableJob < ApplicationJob
    queue_as :background

    # Distinct business keys per request, but the same concurrency slot
    dedup_by ->(job_result, tag) { tag }
    limits_concurrency key: ->(job_result, *) { job_result }, on_conflict: :discard

    def perform(job_result, tag)
    end
  end

  setup do
    @result = JobResult.create!(queue_name: "default")
  end

  test "bulk enqueue identifies already existing jobs by their declared dedup key" do
    # First request: one job that will already exist when the request is retried
    first = DeduplicableJob.new("shared")
    ActiveJob.perform_all_later([ first ])

    assert_difference -> { SolidQueue::Job.count }, +2 do
      result = SolidQueue::Job.enqueue_all([
        DeduplicableJob.new("shared"),          # already exists
        DeduplicableJob.new("one"),             # new
        DeduplicableJob.new("two")              # new
      ])

      assert_kind_of SolidQueue::Job::EnqueueAllResult, result
      assert_equal 2, result.enqueued.size
      assert_equal 1, result.duplicated.size
      assert_equal %w[ one two ], result.enqueued.map { |job| job.arguments["arguments"].first }.sort
    end

    jobs = SolidQueue::Job.where(class_name: "DeduplicableJob").order(:id).to_a
    assert_equal [ "shared", "one", "two" ], jobs.map { |job| job.arguments["arguments"].first }
    # The duplicate points back at the job created by the first request
    assert_equal first.provider_job_id, jobs.first.id
    assert jobs.all?(&:ready?)
  end

  test "bulk enqueue marks duplicated active jobs as enqueued but not new" do
    ActiveJob.perform_all_later([ DeduplicableJob.new("shared") ])
    existing_job = SolidQueue::Job.last

    duplicate, fresh = DeduplicableJob.new("shared"), DeduplicableJob.new("fresh")
    result = SolidQueue::Job.enqueue_all([ duplicate, fresh ])

    assert duplicate.successfully_enqueued?
    assert fresh.successfully_enqueued?
    # The duplicated collection holds the pre-existing job, keyed by the same
    # business key as the incoming duplicate.
    assert_equal [ duplicate.solid_queue_dedup_key ], result.duplicated.map(&:dedup_key)
    assert_equal [ fresh.solid_queue_dedup_key ], result.enqueued.map(&:dedup_key)
    # A duplicate maps to the existing job instead of getting a provider id of its own
    assert_equal existing_job.id, duplicate.provider_job_id
    assert_equal fresh.provider_job_id, result.enqueued.first.id
  end

  test "jobs without a declared dedup key are always inserted as new jobs" do
    ActiveJob.perform_all_later([ AddToBufferJob.new(1) ])

    assert_difference -> { SolidQueue::Job.count }, +2 do
      result = SolidQueue::Job.enqueue_all([ AddToBufferJob.new(1), AddToBufferJob.new(1) ])
      assert_equal 2, result.enqueued.size
      assert_empty result.duplicated
    end
  end

  test "a ReadyExecution write failure is raised instead of being reported as success" do
    # Seed the duplicate from the original request
    SolidQueue::Job.enqueue_all([ DeduplicableJob.new("shared") ])

    calls = 0
    SolidQueue::ReadyExecution.stubs(:create_all_from_jobs).with do |jobs|
      calls += 1
      # Fail only when the batch containing the second new item is written
      jobs.map { |job| job.arguments["arguments"].first }.include?("two")
    end.raises(RaisingReadyExecution.new("boom"))

    active_jobs = [
      DeduplicableJob.new("shared"),  # duplicate, skipped before the ready write
      DeduplicableJob.new("one"),     # new job row inserted
      DeduplicableJob.new("two")      # new job row whose ready write fails
    ]

    error = assert_raises(RaisingReadyExecution) do
      SolidQueue::Job.enqueue_all(active_jobs)
    end
    assert_equal "boom", error.message
    assert calls >= 1

    # The whole enqueue ran in a transaction and rolled back: no half-initialized jobs
    stored_keys = SolidQueue::Job.where(class_name: "DeduplicableJob")
      .map { |job| job.arguments["arguments"].first }
    assert_equal [ "shared" ], stored_keys
    assert_equal 1, SolidQueue::ReadyExecution.count
    # Nothing in the failed request may be reported as successfully enqueued
    assert active_jobs.none?(&:successfully_enqueued?)
  end

  test "retrying after a failed enqueue leaves exactly one job per entry" do
    # Original request: one duplicate plus two new items; the second new item fails its ready write
    SolidQueue::Job.enqueue_all([ DeduplicableJob.new("shared") ])

    SolidQueue::ReadyExecution.stubs(:create_all_from_jobs).raises(RaisingReadyExecution.new("boom"))
    assert_raises(RaisingReadyExecution) do
      SolidQueue::Job.enqueue_all([
        DeduplicableJob.new("shared"),
        DeduplicableJob.new("one"),
        DeduplicableJob.new("two")
      ])
    end

    # No half-initialized jobs survived the rolled-back request
    remaining_keys = SolidQueue::Job.where(class_name: "DeduplicableJob")
      .map { |job| job.arguments["arguments"].first }
    assert_equal [ "shared" ], remaining_keys

    SolidQueue::ReadyExecution.unstub(:create_all_from_jobs)

    # Repeating the exact same request now creates only the two missing entries
    assert_difference -> { SolidQueue::Job.count }, +2 do
      result = SolidQueue::Job.enqueue_all([
        DeduplicableJob.new("shared"),
        DeduplicableJob.new("one"),
        DeduplicableJob.new("two")
      ])
      assert_equal 2, result.enqueued.size
      assert_equal 1, result.duplicated.size
    end

    keys = SolidQueue::Job.where(class_name: "DeduplicableJob").map { |job| job.arguments["arguments"].first.to_s }
    assert_equal [ "one", "shared", "two" ], keys.sort
    assert_equal 3, SolidQueue::ReadyExecution.count
    assert_no_half_initialized_jobs
  end

  test "duplicate detection also covers jobs scheduled for the future" do
    ActiveJob.perform_all_later([ DeduplicableJob.new("later").set(wait: 5.minutes) ])

    result = SolidQueue::Job.enqueue_all([
      DeduplicableJob.new("later").set(wait: 5.minutes),
      DeduplicableJob.new("sooner")
    ])

    assert_equal 1, result.enqueued.size
    assert_equal 1, result.duplicated.size
    assert_equal 1, SolidQueue::ScheduledExecution.count
  end

  test "the same business key repeated within one batch is only stored once" do
    assert_difference -> { SolidQueue::Job.count }, +1 do
      result = SolidQueue::Job.enqueue_all([
        DeduplicableJob.new("twice"),
        DeduplicableJob.new("twice")
      ])

      assert_equal 1, result.enqueued.size
      assert_equal 1, result.duplicated.size
      assert_equal "DeduplicableJob/twice", result.duplicated.first.dedup_key
    end

    assert_equal 1, SolidQueue::ReadyExecution.count
  end

  test "perform_all_later marks every accepted job, including duplicates, as successfully enqueued" do
    ActiveJob.perform_all_later([ DeduplicableJob.new("shared") ])

    duplicate, fresh = DeduplicableJob.new("shared"), DeduplicableJob.new("fresh")
    ActiveJob.perform_all_later([ duplicate, fresh ])

    # Both requests are accepted; only one new row exists for the retried key
    assert duplicate.successfully_enqueued?
    assert fresh.successfully_enqueued?
    assert_equal 2, SolidQueue::Job.where(class_name: "DeduplicableJob").count
    # The duplicate is tied to the pre-existing job, not a new provider id
    assert_equal SolidQueue::Job.find_by!(dedup_key: "DeduplicableJob/shared").id, duplicate.provider_job_id
  end

  test "a blank computed dedup key means the job enqueues as a regular, non-deduplicable job" do
    assert_difference -> { SolidQueue::Job.count }, +2 do
      result = SolidQueue::Job.enqueue_all([ BlankKeyJob.new(nil), BlankKeyJob.new(nil) ])
      assert_equal 2, result.enqueued.size
      assert_empty result.duplicated
      assert SolidQueue::Job.last(2).all? { |job| job.dedup_key.nil? }
    end
  end

  test "concurrency-discarded jobs stay out of the enqueued set without affecting dedup" do
    held = DiscardableDeduplicableJob.new(@result, "held")
    conflicting = DiscardableDeduplicableJob.new(@result, "conflicting")

    result = SolidQueue::Job.enqueue_all([ held, conflicting ])

    # Different dedup keys mean both rows are inserted, but the concurrency
    # conflict discards the second job before it gets an execution.
    assert_equal 1, result.enqueued.size
    assert_empty result.duplicated
    assert_equal 1, SolidQueue::ReadyExecution.count
    assert held.successfully_enqueued?
    assert_not conflicting.successfully_enqueued?

    # Once the slot frees up, retrying the discarded request enqueues it:
    # its row was destroyed by the discard, so it isn't recognized as a duplicate.
    SolidQueue::Job.find(held.provider_job_id).destroy!
    SolidQueue::Semaphore.delete_all

    retry_result = SolidQueue::Job.enqueue_all([ DiscardableDeduplicableJob.new(@result, "conflicting") ])
    assert_equal 1, retry_result.enqueued.size
    assert_empty retry_result.duplicated
  end

  private
    def assert_no_half_initialized_jobs
      SolidQueue::Job.where(class_name: "DeduplicableJob").find_each do |job|
        assert job.ready? || job.scheduled? || job.blocked?, "job #{job.id} has no execution"
      end
    end
end
