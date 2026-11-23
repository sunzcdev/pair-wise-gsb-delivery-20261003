# frozen_string_literal: true

# A minimal, fully controlled executor for cancellation tests: jobs are queued
# and processed one at a time by a dedicated worker thread, with no caller_runs
# fallback. This makes "blocked first, queued second" deterministic and keeps
# the test independent of the shared global async thread pool.
class ManualSerialExecutor
  attr_reader :scheduled_task_count, :completed_task_count

  def initialize
    @jobs = Queue.new
    @scheduled_task_count = 0
    @completed_task_count = 0
    @mutex = Mutex.new
    @worker = Thread.new do
      loop do
        job = @jobs.pop
        break if job.nil?
        begin
          job.call
        rescue Exception
          # mirror thread-pool behavior: worker survives a failed job
        ensure
          @mutex.synchronize { @completed_task_count += 1 }
        end
      end
    end
  end

  def post(&job)
    @mutex.synchronize { @scheduled_task_count += 1 }
    @jobs << job
    nil
  end

  def queue_length
    @jobs.size
  end

  def drain(timeout: 10)
    deadline = Time.now + timeout
    loop do
      remaining = @mutex.synchronize { @scheduled_task_count - @completed_task_count }
      break if remaining <= 0
      raise Timeout::Error, "executor did not drain" if Time.now > deadline
      sleep 0.02
    end
  end

  def shutdown
    @jobs << nil
    unless @worker.join(2)
      @worker.kill
      @worker.join(1)
    end
  end
end
