# frozen_string_literal: true

# Minimal Solid Queue consumer for the demo. It uses only Solid Queue's public
# data model (ready executions -> claimed executions -> job execution) so the
# queue file is consumed exactly like a real Solid Queue worker would, without
# booting the full supervisor/heartbeat machinery.
class DemoWorker
  def self.run(queues: %w[orders default], max_jobs: nil)
    new(queues: queues, max_jobs: max_jobs).run
  end

  def initialize(queues:, max_jobs:)
    @queues = queues
    @max_jobs = max_jobs
    @processed = 0
  end

  def run
    loop do
      break if @max_jobs && @processed >= @max_jobs

      execution = claim_next_ready_execution
      break unless execution

      perform(execution)
      @processed += 1
    end
    @processed
  end

  private

  # Single-writer demo worker: take the oldest ready row in one queue-DB
  # transaction and move it to claimed executions. The claim row is what makes
  # a crash mid-job observable (orphaned claim) rather than losing the job.
  def claim_next_ready_execution
    SolidQueue::Record.transaction do
      ready = SolidQueue::ReadyExecution
              .where(queue_name: @queues)
              .order(priority: :asc, job_id: :asc)
              .lock
              .first
      return nil unless ready

      SolidQueue::ClaimedExecution.create!(job_id: ready.job_id, process_id: nil)
      ready.destroy!
      SolidQueue::ClaimedExecution.find_by!(job_id: ready.job_id)
    end
  end

  def perform(claimed)
    job = claimed.job
    OutboxDemo.audit("worker picked up solid_queue_jobs.id=#{job.id} active_job_id=#{job.active_job_id}")

    # Rehydrate through Active Job exactly as Solid Queue's own ClaimedExecution
    # does, then mark the job finished and release the claim.
    ActiveJob::Base.execute(job.arguments.merge("provider_job_id" => job.id))

    SolidQueue::Record.transaction do
      job.finished!
      claimed.destroy!
    end
    OutboxDemo.audit("worker finished solid_queue_jobs.id=#{job.id}")
  rescue => e
    OutboxDemo.audit("worker ERROR #{job&.id}: #{e.class}: #{e.message}")
    raise
  end
end
