# frozen_string_literal: true

module SolidQueue
  class Job
    module Batchable
      extend ActiveSupport::Concern

      included do
        belongs_to :batch, optional: true
        has_one :batch_execution

        after_create :create_batch_execution, if: :batched?
        after_update :update_batch_progress, if: :batched?
        before_destroy :destroy_batch_execution, if: :batched?

        # A job is enqueued but not yet done with while it sits in any of the
        # execution tables: ready, scheduled, blocked waiting on concurrency or
        # claimed by a worker. Once it finishes or fails it's no longer coming
        # back to its batch, so it stops counting as a reference.
        scope :awaiting_execution, -> {
          where(id: ReadyExecution.select(:job_id))
            .or(where(id: ScheduledExecution.select(:job_id)))
            .or(where(id: BlockedExecution.select(:job_id)))
            .or(where(id: ClaimedExecution.select(:job_id)))
        }
      end

      class_methods do
        def batch_all(jobs)
          BatchExecution.create_all_from_jobs(jobs) if Batch.migrated?
        end

        # Callback jobs don't belong to the batch they report on, so they carry
        # its id in their serialized arguments under
        # ActiveJob::BatchId::CALLBACK_BATCH_ID_KEY instead of in their batch_id.
        # Prefilter in SQL and then parse the arguments, so a key that only
        # shows up nested in a job's own payload is never read as a reference.
        def callback_batch_ids_referenced_by(jobs)
          reference = "%\"#{ActiveJob::BatchId::CALLBACK_BATCH_ID_KEY}\"%"

          jobs.where("arguments LIKE ?", reference).pluck(:arguments)
            .filter_map { |arguments| arguments[ActiveJob::BatchId::CALLBACK_BATCH_ID_KEY] }
            .compact.uniq
        end
      end

      # Also guards against the batches schema not being installed: without
      # its migration, jobs don't even have a batch_id.
      def batched?
        Batch.migrated? && batch_id?
      end

      private
        def create_batch_execution
          BatchExecution.create_all_from_jobs([ self ])
        end

        def update_batch_progress
          return unless saved_change_to_finished_at? && finished_at.present?

          batch_execution&.destroy!
        rescue ActiveRecord::ActiveRecordError => e
          SolidQueue.instrument(:batch_progress_error, batch_id: batch_id, job_id: id, error: e)
        end

        # Destroy through Active Record instead of relying on the foreign
        # key's cascade, so destroying the tracking row retries the batch
        # completion check.
        def destroy_batch_execution
          batch_execution&.destroy!
        end
    end
  end
end
