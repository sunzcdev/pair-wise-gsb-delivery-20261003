# frozen_string_literal: true

module SolidQueue
  class ScheduledExecution < Execution
    include Dispatching

    scope :due, -> { where(scheduled_at: ..Time.current) }
    scope :ordered, -> { order(scheduled_at: :asc, priority: :asc, job_id: :asc) }
    scope :next_batch, ->(batch_size) { due.ordered.limit(batch_size) }

    # When no explicit scan limit is provided, the bounded candidate window is
    # this multiple of the ready batch size (at least +MIN_SCAN_LIMIT+). It only
    # extends past candidates that couldn't enter ready (concurrency-blocked or
    # discarded); a fully dispatchable batch still touches exactly +batch_size+
    # rows.
    DEFAULT_SCAN_LIMIT_MULTIPLIER = 10
    MIN_SCAN_LIMIT = 100

    assumes_attributes_from_job :scheduled_at

    class << self
      # Dispatches due jobs until +batch_size+ of them have actually entered the
      # ready queue, scanning at most +scan_limit+ due scheduled executions.
      #
      # Candidates at the head that hit a concurrency limit aren't skipped or
      # lost: they're moved to blocked executions following the usual ordering
      # and get their next chance through the blocked-execution release path.
      # The scan is always bounded by +scan_limit+, so filling the ready batch
      # never requires an unbounded query.
      #
      # Returns the number of scheduled executions removed for jobs dispatched
      # to ready/blocked, and emits a +dispatch_scheduled+ report with the
      # scanned/ready/blocked breakdown.
      def dispatch_next_batch(batch_size, scan_limit: nil)
        batch_size = [ batch_size.to_i, 0 ].max
        scan_limit = effective_scan_limit(batch_size, scan_limit)

        transaction do
          chunk_limit = [ batch_size, scan_limit ].min
          job_ids = next_batch(chunk_limit).non_blocking_lock.pluck(:job_id)

          next 0 if job_ids.empty?

          SolidQueue.instrument(:dispatch_scheduled,
              batch_size: batch_size, scan_limit: scan_limit) do |payload|
            scanned = 0
            dispatched = 0
            ready = 0
            blocked = 0
            ready_remaining = batch_size
            scan_remaining = scan_limit

            loop do
              scanned += job_ids.size
              report = dispatch_jobs_report(job_ids)

              dispatched += report[:size]
              ready += report[:ready]
              blocked += report[:blocked]
              ready_remaining -= report[:ready]
              scan_remaining -= job_ids.size

              # Ready batch filled, bounded scan exhausted, or last due page.
              break if ready_remaining <= 0
              break if job_ids.size < chunk_limit

              chunk_limit = [ ready_remaining, scan_remaining ].min
              break if chunk_limit <= 0

              job_ids = next_batch(chunk_limit).non_blocking_lock.pluck(:job_id)
              break if job_ids.empty?
            end

            payload[:size] = dispatched
            payload[:scanned] = scanned
            payload[:ready] = ready
            payload[:blocked] = blocked

            dispatched
          end
        end
      end

      private
        def effective_scan_limit(batch_size, scan_limit)
          limit = scan_limit ? scan_limit.to_i : default_scan_limit(batch_size)
          [ limit, batch_size, 1 ].max
        end

        def default_scan_limit(batch_size)
          [ batch_size * DEFAULT_SCAN_LIMIT_MULTIPLIER, MIN_SCAN_LIMIT ].max
        end
    end
  end
end
