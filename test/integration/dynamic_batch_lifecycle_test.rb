# frozen_string_literal: true

require "test_helper"

class DynamicBatchLifecycleTest < ActiveSupport::TestCase
  setup do
    @_on_thread_error = SolidQueue.on_thread_error
    SolidQueue.on_thread_error = silent_on_thread_error_for([ ExpectedTestError ], @_on_thread_error)
    @worker = SolidQueue::Worker.new(queues: "background", threads: 3)
    @dispatcher = SolidQueue::Dispatcher.new(batch_size: 10, polling_interval: 0.2, concurrency_maintenance_interval: 1)
  end

  teardown do
    SolidQueue.on_thread_error = @_on_thread_error
    @worker.stop
    @dispatcher.stop

    JobBuffer.clear

    SolidQueue::Job.destroy_all
    SolidQueue::Batch.destroy_all
  end

  class DynamicBatchCallbackJob < ApplicationJob
    queue_as :background

    def perform
      JobBuffer.add "callback total=#{batch.total_jobs} completed=#{batch.completed_jobs} failed=#{batch.failed_jobs}"
    end
  end

  # A plain member that just records it ran.
  class DynamicBufferJob < ApplicationJob
    queue_as :background

    def perform(label)
      JobBuffer.add "member #{label}"
    end
  end

  # The last initial member: while it is performing, it opens a second database
  # connection, appends another member to the still-open dynamic batch from it,
  # then seals the batch through that same connection.
  class AppendAndSealJob < ApplicationJob
    queue_as :background

    def perform(label)
      JobBuffer.add "member #{label}"

      SolidQueue::Record.connection_pool.with_connection do
        found = SolidQueue::Batch.find(batch_id)
        found.enqueue { DynamicBufferJob.perform_later("appended") }
        found.seal
      end
    end
  end

  test "an unsealed batch accepts a late append from another connection and fires the callback once with real totals" do
    batch = SolidQueue::Batch.enqueue_dynamic(on_finish: DynamicBatchCallbackJob) do
      DynamicBufferJob.perform_later("one")
      AppendAndSealJob.perform_later("two")
    end

    assert_not batch.reload.finished?
    assert_not batch.sealed?

    @dispatcher.start
    @worker.start

    wait_for_batches_to_finish_for(10.seconds)
    wait_for_jobs_to_finish_for(10.seconds)

    finished = batch.reload
    assert finished.finished?
    assert finished.sealed?

    # Two initial members plus the one appended from the second connection.
    assert_equal 3, finished.total_jobs
    assert_equal 3, finished.completed_jobs
    assert_equal 0, finished.failed_jobs

    # Every member really ran, and the callback fired exactly once with totals
    # that include the appended member.
    values = JobBuffer.values
    assert_includes values, "member one"
    assert_includes values, "member two"
    assert_includes values, "member appended"
    assert_equal [ "callback total=3 completed=3 failed=0" ], values.grep(/^callback /)
    assert_equal 1, SolidQueue::Job.where(class_name: DynamicBatchCallbackJob.name).count

    # The append is verifiable in the database, ordered after the initial members.
    member_jobs = SolidQueue::Job.where(batch_id: batch.id).order(:id)
    assert_equal [ "one", "two", "appended" ], member_jobs.map { |job| job.arguments["arguments"] }.flatten
  end

  test "an unsealed dynamic batch never finishes even when all current members complete" do
    batch = SolidQueue::Batch.enqueue_dynamic(on_finish: DynamicBatchCallbackJob) do
      DynamicBufferJob.perform_later("only")
    end

    @dispatcher.start
    @worker.start

    wait_for_jobs_to_finish_for(5.seconds)

    # Give the dispatcher's maintenance sweeper a chance to wrongly finish it.
    sleep 1

    assert_not batch.reload.finished?
    assert_not batch.sealed?
    assert_empty JobBuffer.values.grep(/^callback /)

    SolidQueue::Record.connection_pool.with_connection do
      found = SolidQueue::Batch.find(batch.id)
      found.enqueue { DynamicBufferJob.perform_later("later") }
      found.seal
    end

    wait_for_batches_to_finish_for(5.seconds)
    wait_for_jobs_to_finish_for(5.seconds)

    finished = batch.reload
    assert finished.finished?
    assert_equal 2, finished.total_jobs
    assert_equal 2, finished.completed_jobs
    assert_equal [ "callback total=2 completed=2 failed=0" ], JobBuffer.values.grep(/^callback /)
  end

  class FailingDynamicMemberJob < ApplicationJob
    queue_as :background

    def perform
      raise ExpectedTestError, "boom"
    end
  end

  test "the single callback reflects failed members after sealing" do
    batch = SolidQueue::Batch.enqueue_dynamic(on_finish: DynamicBatchCallbackJob) do
      FailingDynamicMemberJob.perform_later
      DynamicBufferJob.perform_later("ok")
    end

    @dispatcher.start
    @worker.start

    wait_for_jobs_to_finish_for(5.seconds)

    SolidQueue::Record.connection_pool.with_connection do
      found = SolidQueue::Batch.find(batch.id)
      found.seal
    end

    wait_for_batches_to_finish_for(5.seconds)
    wait_for_jobs_to_finish_for(5.seconds)

    finished = batch.reload
    assert finished.finished?
    assert finished.failed?
    assert_equal 2, finished.total_jobs
    assert_equal 1, finished.completed_jobs
    assert_equal 1, finished.failed_jobs
    assert_equal [ "callback total=2 completed=1 failed=1" ], JobBuffer.values.grep(/^callback /)
    assert_equal 1, SolidQueue::Job.where(class_name: DynamicBatchCallbackJob.name).count
  end
end