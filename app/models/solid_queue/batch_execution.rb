# frozen_string_literal: true

module SolidQueue
  class BatchExecution < Execution
    self.assumable_attributes_from_job = [ :batch_id ]

    belongs_to :batch

    scope :with_finished_jobs, -> { joins(:job).merge(SolidQueue::Job.finished) }
    scope :with_failed_jobs, -> { joins(job: :failed_execution) }

    after_commit :finish_batch, on: :destroy

    class << self
      def create_all_from_jobs(jobs)
        jobs.select(&:batched?).group_by(&:batch_id).each do |batch_id, jobs_in_batch|
          # Update the counter first: inserting tracking rows takes a shared FK lock on
          # the batch row, then incrementing can deadlock concurrent MySQL adders.
          if attempt_to_update_total_jobs(batch_id, jobs_in_batch)
            super jobs_in_batch
          else
            raise Batch::AlreadyFinished, "Can't add jobs into a finished or sealed batch"
          end
        end
      end

      private
        def attempt_to_update_total_jobs(batch_id, jobs)
          new_jobs_count = count_new_jobs_among(jobs)
          # Static batches only reject appends once finished. Dynamic batches also
          # reject them once sealed: the sealed_at guard makes the increment CAS lose
          # after a concurrent seal commits, and that failure rolls the enclosing
          # enqueue transaction back, so the job row can never be left orphaned.
          # IS NOT TRUE is NULL-safe: legacy rows have a NULL dynamic flag.
          appendable = SolidQueue::Batch.where(id: batch_id).unfinished.
            where("dynamic IS NOT TRUE OR sealed_at IS NULL")
          updated = appendable.update_all([ "total_jobs = total_jobs + ?", new_jobs_count ])
          updated > 0
        end

        # A job that has executed before was already counted when it first joined
        # the batch: retries keep their active_job_id and batch across re-enqueues.
        # This might undercount jobs whose retries switch to another batch, but that
        # should be a rare enough case. The counter is used only for report/info, so
        # we favour simplicity here
        def count_new_jobs_among(jobs)
          jobs.reject { |job| job.arguments["executions"].to_i > 0 }.map(&:active_job_id).uniq.size
        end
    end

    private
      def finish_batch
        # Skip the serialized callback and metadata columns on this hot path,
        # but keep every column the completion check reads, including the
        # dynamic/seal flags that gate dynamic batch completion.
        if batch = Batch.select(:id, :finished_at, :enqueued_at, :dynamic, :sealed_at).find_by(id: batch_id)
          batch.finish
        end
      rescue => error
        # The job is already committed; let the sweeper retry batch completion.
        SolidQueue.instrument(:batch_progress_error, batch_id: batch_id, job_id: job_id, error: error)
      end
  end
end
