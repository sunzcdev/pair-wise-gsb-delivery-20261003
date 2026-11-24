# frozen_string_literal: true

require "test_helper"

class SolidQueue::DynamicBatchTest < ActiveSupport::TestCase
  self.use_transactional_tests = false

  teardown do
    SolidQueue::Job.destroy_all
    SolidQueue::Batch.destroy_all
  end

  class DynamicMemberJob < ApplicationJob
    def perform(arg)
      Rails.logger.info "dynamic member #{arg}"
    end
  end

  class CallbackRecordingJob < ApplicationJob
    def perform
      JobBuffer.add "callback:total=#{batch.total_jobs}:completed=#{batch.completed_jobs}:failed=#{batch.failed_jobs}"
    end
  end

  test "enqueue_dynamic creates a dynamic, unsealed batch" do
    batch = SolidQueue::Batch.enqueue_dynamic { DynamicMemberJob.perform_later("a") }

    assert batch.reload.dynamic?
    assert_not batch.sealed?
    assert_equal :enqueued, batch.status
    assert_equal 1, batch.total_jobs
  end

  test "regular batches stay static and never sealed" do
    batch = SolidQueue::Batch.enqueue { DynamicMemberJob.perform_later("a") }

    assert_not batch.reload.dynamic?
    assert_not batch.sealed?
  end

  test "an unsealed dynamic batch does not finish when its jobs complete" do
    batch = SolidQueue::Batch.enqueue_dynamic(on_finish: CallbackRecordingJob) do
      DynamicMemberJob.perform_later("a")
    end

    batch.jobs.sole.finished!

    assert_not batch.reload.finished?
    assert_not batch.sealed?
    assert_empty SolidQueue::Job.where(class_name: CallbackRecordingJob.name)
  end

  test "a dynamic batch finishes once sealed with no outstanding work" do
    batch = SolidQueue::Batch.enqueue_dynamic(on_finish: CallbackRecordingJob) do
      DynamicMemberJob.perform_later("a")
    end

    batch.jobs.sole.finished!
    returned = batch.seal

    assert_same batch, returned
    assert batch.reload.finished?
    assert batch.sealed?
    assert_equal 1, SolidQueue::Job.where(class_name: CallbackRecordingJob.name).count
  end

  test "sealing an empty dynamic batch finishes it and fires callbacks once" do
    batch = SolidQueue::Batch.enqueue_dynamic(on_finish: CallbackRecordingJob) { }

    assert_not batch.reload.finished?

    batch.seal

    assert batch.reload.finished?
    assert_equal 1, SolidQueue::Job.where(class_name: CallbackRecordingJob.name).count

    # Idempotent: re-sealing neither duplicates the callback nor raises
    batch.seal
    assert_equal 1, SolidQueue::Job.where(class_name: CallbackRecordingJob.name).count
  end

  test "appends are accepted while open and rejected after sealing" do
    batch = SolidQueue::Batch.enqueue_dynamic { DynamicMemberJob.perform_later("a") }

    batch.enqueue { DynamicMemberJob.perform_later("b") }
    assert_equal 2, batch.jobs.count

    batch.seal

    assert_raises(SolidQueue::Batch::AlreadyFinished) do
      batch.enqueue { DynamicMemberJob.perform_later("c") }
    end

    assert_equal 2, SolidQueue::Job.where(batch_id: batch.id).count
    assert_equal 2, batch.reload.total_jobs
  end

  test "a rejected append rolls back the job row, leaving no orphan job" do
    batch = SolidQueue::Batch.enqueue_dynamic { DynamicMemberJob.perform_later("a") }
    batch.seal

    jobs_before = SolidQueue::Job.count
    executions_before = SolidQueue::BatchExecution.count

    assert_raises(SolidQueue::Batch::AlreadyFinished) do
      batch.enqueue { DynamicMemberJob.perform_later("orphan") }
    end

    assert_equal jobs_before, SolidQueue::Job.count
    assert_equal executions_before, SolidQueue::BatchExecution.count
    assert_not SolidQueue::Job.where(batch_id: batch.id).where.not(id: batch.jobs.pluck(:id)).exists?
  end

  test "sealing is idempotent once the batch has sealed and finished" do
    batch = SolidQueue::Batch.enqueue_dynamic { DynamicMemberJob.perform_later("a") }
    batch.jobs.sole.finished!
    batch.seal
    assert batch.reload.finished?
    assert batch.sealed?

    assert_nothing_raised { batch.seal }
  end

  test "sealing an unsealed batch that was force-finished raises AlreadyFinished" do
    batch = SolidQueue::Batch.enqueue_dynamic { DynamicMemberJob.perform_later("a") }
    # A state the application can't reach through normal flow, kept as a guard
    # against callers bypassing the completion gate.
    batch.update_columns(finished_at: Time.current)

    assert_raises(SolidQueue::Batch::AlreadyFinished) { batch.seal }
  end

  test "static batches ignore seal: they still finish from the completion query" do
    batch = SolidQueue::Batch.enqueue(on_finish: CallbackRecordingJob) { DynamicMemberJob.perform_later("a") }

    # Sealing a static batch is a no-op for its lifecycle and never flips the
    # dynamic flag; its completion continues to be driven by the tracking rows.
    batch.seal

    assert_not batch.reload.dynamic?
    assert_not batch.sealed?
    assert_not batch.finished?

    batch.jobs.sole.finished!

    assert batch.reload.finished?
    assert_equal 1, SolidQueue::Job.where(class_name: CallbackRecordingJob.name).count
  end

  test "static batches still append to a running batch and finish without sealing" do
    batch = SolidQueue::Batch.enqueue(on_finish: CallbackRecordingJob) do
      DynamicMemberJob.perform_later("first")
    end

    batch.enqueue { DynamicMemberJob.perform_later("second") }
    assert_equal 2, batch.jobs.count

    batch.jobs.each(&:finished!)

    assert batch.reload.finished?
    assert_not batch.sealed?
    assert_equal 1, SolidQueue::Job.where(class_name: CallbackRecordingJob.name).count
  end

  test "the sweeper never finishes an unsealed dynamic batch with no executions" do
    batch = SolidQueue::Batch.enqueue_dynamic { DynamicMemberJob.perform_later("a") }

    # Simulate bulk discard: jobs and tracking rows vanish without callbacks
    SolidQueue::BatchExecution.where(batch_id: batch.id).delete_all
    batch.update_columns(enqueued_at: 5.seconds.ago)

    SolidQueue::Batch.sweep_stalled

    assert_not batch.reload.finished?

    batch.seal
    SolidQueue::Batch.sweep_stalled

    assert batch.reload.finished?
  end

  test "an append from another connection joins the batch and orders after the initial members" do
    batch = SolidQueue::Batch.enqueue_dynamic(on_finish: CallbackRecordingJob) do
      DynamicMemberJob.perform_later("one")
      DynamicMemberJob.perform_later("two")
    end

    initial_ids = batch.jobs.order(:id).pluck(:id)
    assert_equal 2, initial_ids.size

    SolidQueue::Record.connection_pool.with_connection do
      found = SolidQueue::Batch.find(batch.id)
      found.enqueue { DynamicMemberJob.perform_later("three") }
      found.seal
    end

    batch.reload
    assert batch.sealed?
    assert_equal 3, batch.total_jobs

    all_ids = SolidQueue::Job.where(batch_id: batch.id).order(:id).pluck(:id)
    assert_equal initial_ids, all_ids.first(2)
    assert_operator all_ids.last, :>, initial_ids.last
  end

  test "concurrent completion checks on a sealed dynamic batch fire the callback once" do
    batch = SolidQueue::Batch.enqueue_dynamic(on_finish: CallbackRecordingJob) do
      DynamicMemberJob.perform_later("a")
    end

    SolidQueue::BatchExecution.where(batch_id: batch.id).delete_all
    batch.seal

    concurrency = 8
    barrier = Concurrent::CyclicBarrier.new(concurrency)
    threads = concurrency.times.map do
      Thread.new do
        SolidQueue::Record.connection_pool.with_connection do
          barrier.wait
          3.times { SolidQueue::Batch.find(batch.id).finish }
        end
      end
    end
    threads.each(&:join)

    assert batch.reload.finished?
    assert_equal 1, SolidQueue::Job.where(class_name: CallbackRecordingJob.name).count
  end

  test "a completion check racing a seal does not finish the batch before the seal is visible" do
    batch = SolidQueue::Batch.enqueue_dynamic(on_finish: CallbackRecordingJob) do
      DynamicMemberJob.perform_later("a")
    end

    # No executions left, but the batch is still open: completion must be a no-op
    SolidQueue::BatchExecution.where(batch_id: batch.id).delete_all
    SolidQueue::Batch.find(batch.id).finish

    assert_not batch.reload.finished?
    assert_equal 0, SolidQueue::Job.where(class_name: CallbackRecordingJob.name).count
  end

  test "an append racing a seal is atomic: either counted or fully rolled back with no orphan job" do
    # Reproduce the exact guard BatchExecution.create_all_from_jobs uses, while
    # holding the batch row lock so the seal genuinely blocks instead of winning
    # trivially. No job row is inserted here: this pins the counter CAS itself.
    batch = SolidQueue::Batch.enqueue_dynamic(on_finish: CallbackRecordingJob) { DynamicMemberJob.perform_later("a") }
    batch.jobs.sole.finished!

    barrier = Concurrent::CyclicBarrier.new(2)
    outcomes = Queue.new

    adder = Thread.new do
      SolidQueue::Record.connection_pool.with_connection do
        begin
          SolidQueue::Record.transaction do
            updated = SolidQueue::Batch.where(id: batch.id).unfinished.
              where("dynamic IS NOT TRUE OR sealed_at IS NULL").
              update_all("total_jobs = total_jobs + 1")
            raise SolidQueue::Batch::AlreadyFinished if updated == 0
            barrier.wait
            sleep 0.3 # hold the row lock; the seal blocks on it
          end
          outcomes << :committed
        rescue SolidQueue::Batch::AlreadyFinished, ActiveRecord::ActiveRecordError
          outcomes << :rejected
        end
      end
    end

    sealer = Thread.new do
      SolidQueue::Record.connection_pool.with_connection do
        barrier.wait
        SolidQueue::Batch.find(batch.id).seal
      end
    end

    adder.join
    sealer.join
    outcome = outcomes.pop

    batch.reload
    assert batch.finished?
    assert batch.sealed?

    case outcome
    when :committed
      # The append won the race, so its counter bump survives and is sealed in
      assert_equal 2, batch.total_jobs
    when :rejected
      # The seal won before the increment: no partial bump, no orphan rows
      assert_equal 1, batch.total_jobs
      assert_equal 1, SolidQueue::Job.where(batch_id: batch.id).count
      assert_equal 0, SolidQueue::BatchExecution.where(batch_id: batch.id).count
    end

    # Exactly one callback regardless of which side won
    assert_equal 1, SolidQueue::Job.where(class_name: CallbackRecordingJob.name).count
  end

  test "a real enqueue racing a seal never leaves an orphan job" do
    batch = SolidQueue::Batch.enqueue_dynamic(on_finish: CallbackRecordingJob) { DynamicMemberJob.perform_later("a") }

    concurrency = 6
    barrier = Concurrent::CyclicBarrier.new(concurrency)
    errors = Queue.new
    threads = concurrency.times.map do |i|
      Thread.new do
        SolidQueue::Record.connection_pool.with_connection do
          barrier.wait
          found = SolidQueue::Batch.find(batch.id)
          if i.even?
            begin
              found.enqueue { DynamicMemberJob.perform_later("race-#{i}") }
            rescue SolidQueue::Batch::AlreadyFinished => e
              errors << e
            end
          else
            found.seal
          end
        end
      end
    end
    threads.each(&:join)

    batch.reload
    assert batch.sealed?

    # A rejected append runs inside Batch#enqueue's transaction, so its job row
    # must never outlive the failed batch-tracking insert: every persisted member
    # job is reflected in the counter, with no dangling job left behind.
    job_count = SolidQueue::Job.where(batch_id: batch.id).count
    assert_equal job_count, batch.total_jobs

    # Drive any members that won the append race to completion; the batch then
    # finishes because it is sealed, and fires its callback exactly once.
    SolidQueue::Job.where(batch_id: batch.id).where(finished_at: nil).find_each(&:finished!)
    SolidQueue::Batch.sweep_stalled

    assert batch.reload.finished?
    assert_equal 0, SolidQueue::BatchExecution.where(batch_id: batch.id).count
    assert_equal 1, SolidQueue::Job.where(class_name: CallbackRecordingJob.name).count

    # Every surfaced error is the expected rejection, nothing else leaked through
    rejected = []
    rejected << errors.pop until errors.empty?
    assert(rejected.all? { |e| e.is_a?(SolidQueue::Batch::AlreadyFinished) })
  end
end