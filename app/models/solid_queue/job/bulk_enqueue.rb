# frozen_string_literal: true

module SolidQueue
  class Job
    # Bulk enqueue logic, including dedup-key based recognition of requests
    # that are already enqueued. Kept separate from SolidQueue::Job so the
    # single-enqueue path stays untouched.
    module BulkEnqueue
      extend ActiveSupport::Concern

      class_methods do
        # Bulk enqueue Active Job instances, recognizing jobs that are already
        # present by their declared dedup key.
        #
        # Jobs whose class calls +dedup_by+ carry a business key. When a job
        # with the same key is already stored, the incoming one is reported as
        # duplicated instead of being inserted again; everything else is
        # enqueued as new. The whole operation runs in one transaction, so a
        # failure writing any execution (for example ReadyExecution) rolls back
        # every row and raises rather than being misreported as success.
        def enqueue_all(active_jobs)
          # Bulk enqueues bypass ActiveJob#enqueue, so batch membership is captured here
          current_batch_id = Batch.current_batch_id

          active_jobs.each do |job|
            job.scheduled_at ||= Time.current
            job.batch_id = current_batch_id || job.batch_id
          end

          transaction do
            deduplicable, plain = active_jobs.partition { |job| dedup_enabled? && job.solid_queue_dedup_key.present? }

            duplicated_active_jobs, new_deduplicable_jobs = insert_deduplicable_jobs(deduplicable)
            new_plain_jobs = insert_plain_jobs(plain)

            new_jobs = new_deduplicable_jobs + new_plain_jobs
            new_job_ids = new_jobs.map(&:id)
            # Only the rows inserted by this request get an execution. Scoping the
            # preparation to their ids keeps an already-existing duplicate from
            # sneaking into the enqueued set through the execution lookups.
            enqueued_jobs = prepare_all_for_execution(new_jobs).select { |job| new_job_ids.include?(job.id) }

            mark_enqueued!(active_jobs.index_by(&:job_id), enqueued_jobs)

            duplicated_jobs = existing_jobs_for(duplicated_active_jobs)
            mark_duplicated!(duplicated_active_jobs, duplicated_jobs)

            EnqueueAllResult.new(enqueued: enqueued_jobs, duplicated: duplicated_jobs)
          end
        end

        private
          # Insert jobs with a dedup key, letting the database unique index skip
          # rows whose key already exists. The rows that made it in are found by
          # their active_job_id, since looking them up by dedup key would also
          # return the pre-existing rows that were skipped.
          def insert_deduplicable_jobs(active_jobs)
            return [ [], [] ] if active_jobs.empty?

            rows = active_jobs.map { |job| attributes_from_active_job(job) }
            insert_all(rows, unique_by: :dedup_key)

            incoming_job_ids = active_jobs.map(&:job_id)
            inserted = where(active_job_id: incoming_job_ids).order(id: :asc).to_a
            inserted_job_ids = inserted.map(&:active_job_id).to_set

            duplicated = active_jobs.reject { |job| inserted_job_ids.include?(job.job_id) }
            [ duplicated, inserted ]
          end

          # Jobs without a dedup key keep the original insert-all behavior.
          def insert_plain_jobs(active_jobs)
            return [] if active_jobs.empty?

            rows = active_jobs.map { |job| attributes_from_active_job(job) }
            insert_all(rows)
            where(active_job_id: active_jobs.map(&:job_id)).order(id: :asc).to_a
          end

          def existing_jobs_for(duplicated_active_jobs)
            return [] if duplicated_active_jobs.empty?

            where(dedup_key: duplicated_active_jobs.map(&:solid_queue_dedup_key)).order(id: :asc).to_a
          end

          # Only jobs that survived preparation (not discarded by a concurrency
          # conflict) count as enqueued, mirroring the single-enqueue behavior.
          def mark_enqueued!(active_jobs_by_job_id, enqueued_jobs)
            enqueued_jobs.each do |enqueued_job|
              active_job = active_jobs_by_job_id.fetch(enqueued_job.active_job_id)
              active_job.provider_job_id = enqueued_job.id
              active_job.successfully_enqueued = true
            end
          end

          # A duplicate request is accepted (the work is already queued) but
          # gets no provider job id of its own: it maps to the existing job.
          def mark_duplicated!(duplicated_active_jobs, existing_jobs)
            existing_by_key = existing_jobs.index_by(&:dedup_key)

            duplicated_active_jobs.each do |active_job|
              existing_job = existing_by_key.fetch(active_job.solid_queue_dedup_key)
              active_job.provider_job_id = existing_job.id
              active_job.successfully_enqueued = true
            end
          end
      end
    end
  end
end
