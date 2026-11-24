require "test_helper"

class FiberBatchAttributionTest < ActiveSupport::TestCase
  self.use_transactional_tests = false

  class AttributionJob < ApplicationJob
    def perform(*); end
  end

  teardown do
    SolidQueue::Job.destroy_all
    SolidQueue::Batch.destroy_all
  end

  test "interleaved fibers restore their own batch and delivered ownership survives" do
    batch_one = SolidQueue::Batch.create!
    batch_two = SolidQueue::Batch.create!
    ownership = nil
    restored_job = second_job = independent_job = nil

    first = Fiber.new do
      SolidQueue::Batch.wrap_in_batch_context(batch_one.id) do
        ownership = SolidQueue::Batch.current_batch_ownership

        begin
          SolidQueue::Batch.wrap_in_batch_context("inner") do
            Fiber.yield
            raise "fixture"
          end
        rescue RuntimeError
          # The inner scope must restore batch one before this enqueue.
        end

        restored_job = AttributionJob.perform_later("restored")
      end
    end

    second = Fiber.new do
      SolidQueue::Batch.wrap_in_batch_context(batch_two.id) do
        Fiber.yield

        second_job = AttributionJob.perform_later("second")

        # A child fiber does not inherit batch context unless it is explicitly
        # given and enters an ownership.
        Fiber.new { independent_job = AttributionJob.perform_later("independent") }.resume
      end
    end

    first.resume
    second.resume
    first.resume
    second.resume

    delayed_jobs = [ AttributionJob.new("delayed-one"), AttributionJob.new("delayed-two") ]
    ownership.enqueue_all(delayed_jobs)

    constructed_later = nil
    ownership.wrap do
      constructed_later = AttributionJob.new("constructed-later")
      ActiveJob.perform_all_later([ constructed_later ])
    end

    rebind_job = AttributionJob.new("rebind")
    SolidQueue::Batch.wrap_in_batch_context(batch_two.id) { rebind_job.enqueue }

    option_job = AttributionJob.new("explicit-option")
    option_job.enqueue(batch: ownership)

    assert_equal batch_one.id, persisted_batch_id(restored_job)
    assert_equal batch_two.id, persisted_batch_id(second_job)
    assert_nil persisted_batch_id(independent_job)
    assert_equal [ batch_one.id ] * 2, delayed_jobs.map { |job| persisted_batch_id(job) }
    assert_equal batch_one.id, persisted_batch_id(constructed_later)
    assert_equal batch_two.id, persisted_batch_id(rebind_job)
    assert_equal batch_one.id, persisted_batch_id(option_job)

    assert_equal batch_one.id, ownership.call { SolidQueue::Batch.current_batch_id }
    assert_equal batch_one.id,
      SolidQueue::Batch.with_batch_ownership(ownership) { SolidQueue::Batch.current_batch_id }
    assert_nil SolidQueue::Batch.current_batch_id
  end

  test "explicit bulk attribution still yields to an active batch" do
    batch_one = SolidQueue::Batch.create!
    batch_two = SolidQueue::Batch.create!
    ownership = SolidQueue::Batch::BatchOwnership.new(batch_one.id)
    jobs = [ AttributionJob.new("bulk-rebind-one"), AttributionJob.new("bulk-rebind-two") ]

    SolidQueue::Batch.wrap_in_batch_context(batch_two.id) do
      SolidQueue::Job.enqueue_all(jobs, ownership)
    end

    assert_equal [ batch_two.id ] * 2, jobs.map { |job| persisted_batch_id(job) }
  end

  private

    def persisted_batch_id(active_job)
      SolidQueue::Job.find_by!(active_job_id: active_job.job_id).batch_id
    end
end
