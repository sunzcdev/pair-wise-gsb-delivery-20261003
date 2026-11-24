# frozen_string_literal: true

module SolidQueue
  class Batch
    module Status
      extend ActiveSupport::Concern

      included do
        scope :finished, -> { where.not(finished_at: nil) }
        scope :succeeded, -> { finished.where(failed_at: nil) }
        scope :unfinished, -> { where(finished_at: nil) }
        scope :failed, -> { where.not(failed_at: nil) }
        scope :enqueued, -> { where.not(enqueued_at: nil) }
        scope :sealed, -> { where.not(sealed_at: nil) }
        scope :unsealed, -> { where(sealed_at: nil) }
        # Only dynamic batches can be sealed. A static batch never carries a
        # sealed_at, so this keeps it eligible for completion exactly as before.
        # IS NOT TRUE is NULL-safe: legacy rows have a NULL dynamic flag.
        scope :completion_sealed, -> { where("dynamic IS NOT TRUE OR sealed_at IS NOT NULL") }
      end

      def status
        if finished?
          failed? ? :failed : :completed
        elsif enqueued?
          :enqueued
        else
          :pending
        end
      end

      def failed?
        failed_at.present?
      end

      def succeeded?
        finished? && !failed?
      end

      def finished?
        finished_at.present?
      end

      def enqueued?
        enqueued_at.present?
      end

      def dynamic?
        # NULL and false both mean a legacy/static batch
        self[:dynamic] == true
      end

      def sealed?
        self[:sealed_at].present?
      end

      # Failed jobs no longer have tracking rows, so exclude them from the completed count.
      def completed_jobs
        finished? ? self[:completed_jobs] : [ total_jobs - pending_jobs - failed_jobs, 0 ].max
      end

      def failed_jobs
        finished? ? self[:failed_jobs] : jobs.failed.count
      end

      # Pending counts attempts, not logical jobs: while a retry is enqueued
      # and its previous attempt hasn't finished yet, both have tracking rows,
      # so the counts derived from it clamp at the logical totals.
      def pending_jobs
        finished? ? 0 : batch_executions.count
      end

      def progress_percentage
        return 0 if total_jobs == 0
        ([ total_jobs - pending_jobs, 0 ].max * 100.0 / total_jobs).round(2)
      end
    end
  end
end
