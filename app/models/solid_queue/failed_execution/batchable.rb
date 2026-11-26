# frozen_string_literal: true

module SolidQueue
  class FailedExecution
    # A FailedExecution is created only after retries are exhausted, when the
    # job stops counting as pending in its batch.
    module Batchable
      extend ActiveSupport::Concern

      included do
        after_create :destroy_job_batch_execution, if: -> { Batch.migrated? && job.batch_id? }
        after_create :annotate_batch_context, if: -> { Batch.migrated? && job.batch_id? }

        scope :for_batch, ->(batch) do
          batch_id = batch.respond_to?(:id) ? batch.id : batch
          joins(:job).where(Job.table_name => { batch_id: batch_id })
        end
        scope :in_batch, ->(batch) { for_batch(batch) }
      end

      class_methods do
        def for_round(round)
          return for_batch(round) if round.is_a?(SolidQueue::Batch)

          batch_ids = SolidQueue::Batch.all.select { |batch| batch.round == round.to_i }.map(&:id)
          for_batch(batch_ids)
        end

        def retry_failed_jobs_for_batch(batch, **options)
          batch = SolidQueue::Batch.find(batch) unless batch.is_a?(SolidQueue::Batch)
          batch.retry_failed_jobs(**options)
        end
        alias_method :retry_failures_for_batch, :retry_failed_jobs_for_batch
        alias_method :retry_failed_members_for_batch, :retry_failed_jobs_for_batch
        alias_method :retry_failed_jobs, :retry_failed_jobs_for_batch
        alias_method :retry_failed_members, :retry_failed_jobs_for_batch
        alias_method :retry_failures, :retry_failed_jobs_for_batch
      end

      def batch
        job&.batch
      end

      def batch_id
        job&.batch_id
      end

      def batch_round
        stored_round = error.to_h.with_indifferent_access["batch_round"]
        batch&.round || stored_round || 1
      end
      alias round batch_round
      alias execution_round batch_round
      alias retry_round batch_round

      def retry_round?
        batch&.retry_round? || batch_round.to_i > 1
      end

      private
        def annotate_batch_context
          update_column(
            :error,
            error.to_h.merge(
              "batch_id" => job.batch_id,
              "batch_round" => batch&.round || 1
            )
          )
        rescue ActiveRecord::ActiveRecordError => e
          SolidQueue.instrument(:batch_progress_error, batch_id: job.batch_id, job_id: job_id, error: e)
        end

        def destroy_job_batch_execution
          job.batch_execution&.destroy!
        rescue ActiveRecord::ActiveRecordError => e
          SolidQueue.instrument(:batch_progress_error, batch_id: job.batch_id, job_id: job.id, error: e)
        end
    end
  end
end
