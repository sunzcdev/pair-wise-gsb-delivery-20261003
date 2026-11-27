# frozen_string_literal: true

module SolidQueue
  class Batch
    module Clearable
      extend ActiveSupport::Concern

      # Finished batches are cleared once they're older than finished_before,
      # except those a job still waiting to run may need. That covers both an
      # outstanding member, which keeps the batch's tracking rows, and a
      # callback job that's enqueued but hasn't been read by a worker yet:
      # it loads the batch when it runs to report its final state.
      #
      # A reference is live only while it can still be consumed. Jobs that
      # already ran, or that have been waiting for longer than
      # pending_callbacks_for, no longer hold their batch: that's how a
      # callback that never runs, or one that was never enqueued, doesn't
      # keep finished batches around forever, and lets the rest be collected.
      included do
        scope :clearable, ->(finished_before: SolidQueue.clear_finished_jobs_after.ago,
                            pending_callbacks_for: SolidQueue.clear_finished_batches_with_pending_callbacks_after) {
          succeeded.where(finished_at: ...finished_before)
            .without_executions
            .where.not(id: live_callback_batch_ids(pending_callbacks_for: pending_callbacks_for))
        }
      end

      class_methods do
        # Batches that a callback job still awaiting execution references, and
        # that such a job can still consume: one that's ready, scheduled or
        # blocked waiting on concurrency, enqueued within the retention period.
        # Failed callback jobs already ran; finished ones are gone from the
        # execution tables. Either way they have no use for the batch anymore.
        def live_callback_batch_ids(pending_callbacks_for: SolidQueue.clear_finished_batches_with_pending_callbacks_after)
          Job.callback_batch_ids_referenced_by(
            Job.awaiting_execution.where(created_at: pending_callbacks_for.ago..)
          )
        end

        def clear_finished_in_batches(batch_size: 500, finished_before: SolidQueue.clear_finished_jobs_after.ago, pending_callbacks_for: SolidQueue.clear_finished_batches_with_pending_callbacks_after, sleep_between_batches: 0)
          loop do
            records_deleted = clearable(finished_before: finished_before, pending_callbacks_for: pending_callbacks_for).limit(batch_size).delete_all
            sleep(sleep_between_batches) if sleep_between_batches > 0
            break if records_deleted == 0
          end
        end
      end
    end
  end
end
