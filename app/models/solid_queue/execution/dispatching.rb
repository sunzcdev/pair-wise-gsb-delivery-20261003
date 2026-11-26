# frozen_string_literal: true

module SolidQueue
  class Execution
    module Dispatching
      extend ActiveSupport::Concern

      class_methods do
        # Returns the number of scheduled/failed executions removed when their
        # jobs become ready or blocked. Kept as an integer for the retry flow.
        def dispatch_jobs(job_ids)
          dispatch_jobs_report(job_ids)[:size]
        end

        # Same dispatch as +dispatch_jobs+, but reports what happened so a
        # bounded scan can keep filling its ready batch:
        #   size:    executions deleted for dispatched (ready/blocked) jobs
        #   ready:   dispatched jobs that entered solid_queue_ready_executions
        #   blocked: dispatched jobs that entered solid_queue_blocked_executions
        # Jobs discarded by concurrency policies are neither ready nor blocked
        # and aren't part of the dispatched set.
        def dispatch_jobs_report(job_ids)
          jobs = Job.where(id: job_ids)

          Job.dispatch_all(jobs).map(&:id).then do |dispatched_job_ids|
            deleted = where(id: where(job_id: dispatched_job_ids).pluck(:id)).delete_all
            ready = ReadyExecution.where(job_id: dispatched_job_ids).count
            blocked = BlockedExecution.where(job_id: dispatched_job_ids).count

            { size: deleted, ready: ready, blocked: blocked }
          end
        end
      end
    end
  end
end
