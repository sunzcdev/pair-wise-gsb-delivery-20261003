# frozen_string_literal: true

module SolidQueue
  # Durable idempotency record for returning a concurrency permit after a
  # claimed job has reached a terminal state and its ClaimedExecution row has
  # been deleted.
  #
  # A row is created in the same transaction that records the terminal job
  # state and deletes the claim. Returning the permit is therefore retryable by
  # both the late worker and orphan recovery. The unique claim_id and the
  # conditional released_at update make the permit return idempotent even when
  # several recoverers race each other or a late report arrives afterwards.
  class ConcurrencyRelease < Record
    Permit = Struct.new(
      :concurrency_key,
      :acquired_semaphore_id,
      :acquired_concurrency_limit,
      :concurrency_duration,
      keyword_init: true
    )

    # Rows exist only until both the permit return and blocked-job hand-off have
    # finished. A row with released_at set records that the semaphore increment
    # committed but the hand-off still needs another attempt.
    scope :pending, -> { all }
    scope :unreleased, -> { where(released_at: nil) }

    class << self
      def available?
        table_exists?
      end
      alias migrated? available?

      def record_for(claimed_execution)
        return unless available?

        job = claimed_execution.job
        return unless job.concurrency_limited? && job.holds_concurrency_slot?

        new(
          claim_id: claimed_execution.id,
          job_id: job.id,
          concurrency_key: job.concurrency_key,
          semaphore_id: job.acquired_semaphore_id,
          concurrency_limit: job.acquired_concurrency_limit || fallback_limit_for(job),
          concurrency_duration: duration_to_seconds(job.concurrency_duration)
        )
      end

      def release_for_claim(claim_id)
        return unless available?

        find_by(claim_id: claim_id)&.release!
      end
      alias retry_for_claim release_for_claim
      alias recover_for_claim release_for_claim

      def recover_pending
        return 0 unless available?

        recovered = 0
        ordered.find_each do |release|
          release.release!
          recovered += 1
        rescue => error
          # One broken release must not stop orphan/process pruning or leave
          # later claims unexamined. This row stays pending and is retried.
          SolidQueue.logger.error(
            "[SolidQueue] Concurrency release #{release.id} for claim #{release.claim_id} " \
            "failed: #{error.class}: #{error.message}"
          )
        end
        recovered
      end
      alias retry_pending recover_pending
      alias release_pending recover_pending
      alias process_pending recover_pending
      alias reap_orphaned recover_pending
      alias recover_orphaned recover_pending

      def recover_pending!
        ordered.find_each(&:release!)
      end
      alias release_all_pending recover_pending!
      alias retry_all_pending recover_pending!
      alias retry_pending! recover_pending!
      alias recover! recover_pending!
      alias recover_orphaned! recover_pending!

      private
        def ordered
          order(:id)
        end

        def duration_to_seconds(duration)
          duration.respond_to?(:to_i) ? duration.to_i : SolidQueue.default_concurrency_control_period.to_i
        end

        def fallback_limit_for(job)
          job.job_class ? job.concurrency_limit : 1
        end
    end

    def release!
      released = release_permit_once { Semaphore.signal(permit) }
      BlockedExecution.release_one(concurrency_key) if released != false
      complete!

      true
    end
    alias retry! release!
    alias recover! release!

    # Job#unblock_next_blocked_job delegates here when a durable release is
    # attached. The permit identity stored on this record is authoritative: the
    # Job row may have been retried and acquired a newer semaphore generation.
    def release_for_job(_job)
      release!
    end

    # Runs the supplied semaphore release at most once for this claim. The
    # released_at conditional update and the semaphore update commit together.
    def release_permit_once
      return true if released_at?

      self.class.transaction do
        claimed = self.class.where(id: id, released_at: nil)
          .update_all(released_at: Time.current) == 1

        if claimed
          result = yield
          reload
          result
        else
          # Another recoverer won the race. Don't return the permit again, but
          # allow this caller to retry blocked-job promotion if needed.
          begin
            reload
          rescue ActiveRecord::RecordNotFound
            return false
          end
          true
        end
      end
    end
    alias signal_once release_permit_once

    def complete!
      self.class.where(id: id).delete_all
    end

    private
      def permit
        Permit.new(
          concurrency_key: concurrency_key,
          acquired_semaphore_id: semaphore_id,
          acquired_concurrency_limit: concurrency_limit,
          concurrency_duration: permit_duration
        )
      end

      def permit_duration
        seconds = concurrency_duration.presence || SolidQueue.default_concurrency_control_period.to_i
        seconds.to_i.seconds
      end
  end
end
