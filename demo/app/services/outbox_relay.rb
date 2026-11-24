# frozen_string_literal: true

# Recoverable hand-off between the business database and the Solid Queue
# database.
#
# The two SQLite files cannot share a transaction, so enqueuing can never be
# atomic with the business commit. Instead the business transaction only leaves
# a durable *intent* (an OutboxMessage row). This relay turns that intent into
# a real Solid Queue job, and it is safe to run any number of times:
#
#   * every attempt reuses the outbox row's stable +solid_queue_job_id+ as the
#     Active Job id; Solid Queue carries a unique index on active_job_id, so a
#     job that was already created is rejected as a duplicate instead of being
#     created twice;
#   * the outbox row is only flipped to +dispatched+ AFTER the queue-database
#     transaction that creates the job has committed;
#   * a crash between those two steps leaves the row +pending+ while the job
#     already exists; the next run hits the unique index, treats the duplicate
#     as "already handed off", and marks the row dispatched.
#
# Invariant: one outbox row corresponds to at most one Solid Queue job.
class OutboxRelay
  class HandOffError < StandardError; end

  CRASH_AFTER_QUEUE_COMMIT = "CRASH_AFTER_QUEUE_COMMIT"

  def self.run_once(**options)
    new(**options).run_once
  end

  def initialize(crash_point: nil, limit: 100)
    @crash_point = crash_point
    @limit = limit
  end

  # Returns the number of pending rows processed (already-existing duplicates
  # included, since a duplicate still means "hand-off completed").
  def run_once
    processed = 0
    OutboxMessage.pending.limit(@limit).each do |message|
      dispatch(message)
      processed += 1
    end
    processed
  end

  private

  def dispatch(message)
    attempts = message.attempts + 1
    active_job = build_active_job(message)

    # This transaction is opened on the QUEUE connection only. It commits the
    # solid_queue_jobs row and its ready/scheduled execution together.
    queue_job = enqueue_in_queue_db!(active_job)

    crash_if_requested!(message)

    mark_dispatched!(message, queue_job, attempts)
    OutboxDemo.audit(
      "relay dispatch: outbox=#{message.id} job_id=#{active_job.job_id} " \
      "solid_queue_jobs.id=#{queue_job.id}"
    )
  rescue SolidQueue::Job::EnqueueError, ActiveRecord::RecordNotUnique => e
    # Solid Queue wraps adapter errors (including the SQLite unique-constraint
    # violation) in SolidQueue::Job::EnqueueError, so both shapes are caught.
    raise unless unique_violation?(e)

    handle_duplicate(message, e, attempts)
  end

  def unique_violation?(error)
    error.is_a?(ActiveRecord::RecordNotUnique) ||
      error.cause.is_a?(ActiveRecord::RecordNotUnique) ||
      error.message.match?(/unique|duplicate|UNIQUE constraint/i)
  end

  # Rebuild the Active Job object from the durable outbox payload. The payload
  # is the full Active Job serialization envelope produced at business-commit
  # time, so deserialization restores positional/keyword arguments, queue name
  # and -- crucially -- the stable job id Solid Queue will use as
  # active_job_id.
  def build_active_job(message)
    payload = message.arguments
    unless payload.is_a?(Hash) && payload["job_class"] && payload["arguments"]
      raise HandOffError, "outbox ##{message.id} has no valid Active Job envelope"
    end

    # Defensive: the envelope's job_id must always match the outbox column,
    # since that column is what the unique index and recovery path key on.
    payload["job_id"] = message.solid_queue_job_id
    payload["queue_name"] = message.queue_name

    ActiveJob::Base.deserialize(payload)
  end

  def enqueue_in_queue_db!(active_job)
    SolidQueue::Record.transaction do
      SolidQueue::Job.enqueue(active_job)
    end
    # SolidQueue::Job.enqueue returns the persisted SolidQueue::Job on success.
    SolidQueue::Job.find_by!(active_job_id: active_job.job_id)
  end

  # The queue transaction committed but the process died before the outbox row
  # was updated. On the next run the unique index rejects the re-insert: that is
  # the signal that the hand-off actually succeeded previously, so finish the
  # bookkeeping instead of failing or creating another job.
  def handle_duplicate(message, error, attempts)
    existing = SolidQueue::Job.find_by(active_job_id: message.solid_queue_job_id)
    unless existing
      message.update_columns(
        attempts: attempts,
        last_error: "#{error.class}: #{error.message}",
        updated_at: Time.current
      )
      raise HandOffError, "unique violation without a matching Solid Queue job: #{error.message}"
    end

    mark_dispatched!(message, existing, attempts, recovered: true)
    OutboxDemo.audit(
      "relay recovery: outbox=#{message.id} job_id=#{message.solid_queue_job_id} " \
      "already existed (solid_queue_jobs.id=#{existing.id}); marking dispatched, no duplicate job"
    )
  end

  def mark_dispatched!(message, queue_job, attempts, recovered: false)
    # Business-connection write; separate transaction from the queue one.
    message.with_lock do
      next if message.dispatched? # another relay beat us to it

      message.update!(
        status: OutboxMessage::DISPATCHED,
        solid_queue_provider_id: queue_job.id,
        attempts: attempts,
        last_error: nil,
        dispatched_at: Time.current
      )
    end
    OutboxDemo.audit("outbox=#{message.id} marked dispatched#{" (recovered)" if recovered}")
  end

  def crash_if_requested!(message)
    return unless @crash_point == CRASH_AFTER_QUEUE_COMMIT

    OutboxDemo.audit(
      "SIMULATED CRASH: outbox=#{message.id} queue job committed, " \
      "outbox still pending; exiting process"
    )
    exit!(1)
  end
end
