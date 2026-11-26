# frozen_string_literal: true

module Bunny
  # Thread pool that dispatches consumer deliveries. Not supposed to be shared between channels
  # or threads.
  #
  # Every channel has its own consumer pool.
  #
  # Deliveries (and cancellations) carrying a consumer tag are dispatched with
  # *consumer affinity*: all jobs of the same consumer tag are bound to a single
  # worker at a time, and at most one of them runs at any given moment. This
  # guarantees that callbacks of one consumer start strictly in submission order
  # even when the pool runs more than one worker, while different consumers keep
  # running concurrently. Jobs without a tag are treated as unordered and any
  # free worker may pick them up.
  #
  # The pool can be resized while it is running (#resize). Growing the pool adds
  # workers that start taking work immediately. Shrinking marks surplus workers
  # as draining: they finish the block they are currently executing and may keep
  # draining queues they already own, but no new consumer is ever bound to them.
  # Their consumer affinities are reassigned to remaining workers up front, so
  # queued blocks migrate without breaking per-consumer ordering. The block
  # already running on a draining worker always finishes on that worker; the
  # consumer's next block is not started anywhere until that block completes.
  #
  # @private
  class ConsumerWorkPool

    # A unit of work. `tag` is the consumer tag providing affinity/ordering
    # guarantees, or nil for unordered work.
    Job = Struct.new(:tag, :callable) do
      def call
        callable.call
      end
    end

    #
    # API
    #

    attr_reader :size
    attr_reader :abort_on_exception
    attr_reader :shutdown_timeout

    def initialize(size = 1, abort_on_exception = false, shutdown_timeout = 60)
      raise ArgumentError, "pool size must be positive" if size.nil? || size < 1

      @mutex                = ::Mutex.new
      @condition            = ::ConditionVariable.new
      @shutdown_conditional = ::ConditionVariable.new

      @size               = size
      @abort_on_exception = abort_on_exception
      @shutdown_timeout   = shutdown_timeout

      # FIFO of jobs not yet claimed by a worker.
      @jobs     = []
      # Consumer tag => worker currently owning that consumer's queue.
      @affinity = {}
      # Consumer tag => worker executing that consumer's current block. A tag
      # present here is "busy": no other block of that tag may start anywhere.
      @busy     = {}
      # All live worker threads, including draining ones.
      @threads  = []
      # Number of live worker threads. Shutdown waiting uses this so it only
      # completes once every worker has actually exited its loop, not merely
      # when the last queued block finished.
      @active_worker_count = 0
      # Worker thread => true while it must not accept new long-term ownership.
      @draining = {}
      # Worker thread => job it is currently executing (nil when idle).
      @in_flight = {}

      @running  = false
      @paused   = false
      @shutdown = false
    end

    # Submits a block for execution.
    #
    # @param [String] consumer_tag optional consumer tag. Jobs sharing a tag are
    #   guaranteed to start in submission order and never run concurrently with
    #   each other.
    def submit(callable = nil, consumer_tag: nil, &block)
      job = Job.new(consumer_tag, callable || block)

      @mutex.synchronize do
        return if @shutdown

        @jobs << job
        @condition.broadcast if @running
      end
    end

    def start
      @mutex.synchronize do
        return if @running

        @running  = true
        @paused   = false
        @shutdown = false

        @size.times { spawn_worker }
        @condition.broadcast
      end
    end

    def running?
      @mutex.synchronize { @running && !@shutdown }
    end

    # Number of jobs waiting to be started plus blocks currently being executed.
    def backlog
      @mutex.synchronize { @jobs.length + @in_flight.values.compact.length }
    end

    def busy?
      backlog > 0
    end

    # Changes the number of workers while the pool keeps running.
    #
    # When growing, new workers are spawned immediately and begin sharing work.
    # When shrinking, surplus workers enter draining mode: consumer affinities
    # they own are reassigned to remaining workers up front, so ordering and
    # block ownership described in the class docs are preserved. In-flight
    # blocks always run to completion on their original worker.
    #
    # @return [Integer] the new pool size
    def resize(new_size)
      raise ArgumentError, "pool size must be positive" if new_size.nil? || new_size < 1

      @mutex.synchronize do
        @size = new_size

        if @running && !@shutdown
          active = active_workers
          if active.length < new_size
            (new_size - active.length).times { spawn_worker }
          elsif active.length > new_size
            active.drop(new_size).each { |worker| mark_draining(worker) }
          end
          @condition.broadcast
        end
      end

      new_size
    end

    # Gracefully stops the pool.
    #
    # With wait_for_workers=true the call blocks (up to shutdown_timeout) until
    # every already submitted block has finished, letting in-flight work wrap up.
    # Otherwise it only signals the workers, which still finish their current
    # block and drain the remaining queue before exiting.
    def shutdown(wait_for_workers = false)
      was_running = false
      @mutex.synchronize do
        return self if @shutdown

        was_running = @running
        @shutdown   = true
        @running    = false
        @paused     = false
        @condition.broadcast
      end

      if wait_for_workers && @shutdown_timeout && was_running
        @mutex.synchronize do
          deadline = monotonic_time + @shutdown_timeout
          while (busy_locked? || @active_worker_count > 0) && monotonic_time < deadline
            remaining = deadline - monotonic_time
            break if remaining <= 0

            @shutdown_conditional.wait(@mutex, remaining)
          end
        end
      end

      self
    end

    def join(timeout = nil)
      threads = @mutex.synchronize { @threads.dup }
      threads.each { |t| t.join(timeout) }
    end

    def pause
      @mutex.synchronize do
        @running = false
        @paused  = true
      end
    end

    def resume
      @mutex.synchronize do
        @running = true
        @paused  = false
        @condition.broadcast
      end
    end

    def kill
      threads = @mutex.synchronize do
        @running  = false
        @paused   = false
        @shutdown = true
        @jobs.clear
        @condition.broadcast
        @threads.dup
      end
      threads.each { |t| t.kill }
    end

    # Worker threads, including workers that are draining but have not exited.
    def threads
      @mutex.synchronize { @threads.dup }
    end

    protected

    #
    # Worker lifecycle
    #

    def spawn_worker
      worker = Thread.new(&method(:run_loop))
      worker.abort_on_exception = true if @abort_on_exception
      @threads    << worker
      @draining[worker]  = false
      @in_flight[worker] = nil
      @active_worker_count += 1
      worker
    end

    def run_loop
      worker = Thread.current

      loop do
        job = wait_for_job(worker)
        break if job.nil?

        begin
          job.call
        rescue ::StandardError => e
          # TODO: use connection logger
          $stderr.puts e.class.name
          $stderr.puts e.message
        ensure
          @mutex.synchronize do
            @in_flight[worker] = nil
            @busy.delete(job.tag) if job.tag
            # The freed consumer may have queued blocks another worker now owns.
            @condition.broadcast
            @shutdown_conditional.broadcast if @shutdown
          end
        end
      end

      on_worker_exit(worker)
    end

    # Blocks until a job this worker is allowed to run is available, or until
    # the worker must exit. Returns nil when the worker should stop.
    def wait_for_job(worker)
      @mutex.synchronize do
        loop do
          return nil if must_exit?(worker)

          # A paused worker parks without claiming anything; #resume broadcasts
          # and the flag flips back, so the loop re-enters scheduling.
          if @paused
            @condition.wait(@mutex)
            next
          end

          job = claimable_job_for(worker)
          return job if job

          # Park until submit/resize/resume/completion signals progress.
          @condition.wait(@mutex)
        end
      end
    end

    def on_worker_exit(worker)
      @mutex.synchronize do
        # Re-home every consumer still bound to this worker before it disappears,
        # so queued blocks keep their ordering on the remaining workers.
        reassign_affinity_from(worker)
        prune_affinity_for(worker)

        @threads.delete(worker)
        @draining.delete(worker)
        @in_flight.delete(worker)
        @active_worker_count -= 1

        if @shutdown && !busy_locked? && @active_worker_count.zero?
          @shutdown_conditional.broadcast
        elsif @shutdown
          # Other workers still have to notice the shutdown and exit; wake them.
          @condition.broadcast
          @shutdown_conditional.broadcast
        else
          @condition.broadcast
        end
      end
    end

    #
    # Scheduling
    #

    # Finds and removes the next job the given worker is allowed to execute.
    #
    # Ordering rule: a tagged job may only start when (a) it is the earliest
    # queued job of its consumer still waiting and (b) that consumer has no
    # block in flight anywhere. Combined with single-owner affinity this makes
    # each consumer's callbacks start strictly in submission order, including
    # across worker migration.
    def claimable_job_for(worker)
      draining = @draining[worker]
      index    = nil

      @jobs.each_with_index do |job, i|
        if job.tag.nil?
          # Unordered work: draining workers only take it during full shutdown.
          if !draining || @shutdown
            index = i
            break
          end
          next
        end

        # A block of this consumer is still running: no later block may start,
        # on this worker or any other (this is what keeps migration ordered).
        next if @busy.key?(job.tag)

        owner = @affinity[job.tag]
        if owner.nil?
          # Unowned consumer: never bind new ownership to a draining worker,
          # unless the whole pool is shutting down and nobody else is left.
          next if draining && !@shutdown

          index = i
          @affinity[job.tag] = worker
          break
        elsif owner.equal?(worker)
          index = i
          break
        end
        # Owned by another worker: keep scanning for another claimable job.
      end

      return nil unless index

      job = @jobs.delete_at(index)
      if job.tag
        @affinity[job.tag] = worker unless @affinity.key?(job.tag)
        @busy[job.tag] = worker
      end
      @in_flight[worker] = job
      job
    end

    def mark_draining(worker)
      @draining[worker] = true
      reassign_affinity_from(worker)
    end

    # Moves every consumer affinity currently held by `worker` onto a remaining
    # active worker. The reassignment happens while the old worker is still
    # alive: its current block keeps running on it (tracked in @busy), but queued
    # blocks of the moved consumers are claimed by the new owner in their
    # original order once the running block completes.
    def reassign_affinity_from(worker)
      @affinity.each_key do |tag|
        next unless @affinity[tag].equal?(worker)

        target = pick_affinity_target(worker)
        @affinity[tag] = target if target
        # With no target (pool fully shutting down) the binding stays; any
        # still-alive worker may claim the jobs during shutdown draining.
      end
    end

    # Drops affinity entries for consumers that have no queued work and no block
    # in flight, so the map does not grow with consumer churn.
    def prune_affinity_for(worker)
      @affinity.each_key do |tag|
        next unless @affinity[tag].equal?(worker)
        next if @busy.key?(tag)
        next if @jobs.any? { |job| job.tag == tag }

        @affinity.delete(tag)
      end
    end

    def pick_affinity_target(excluding)
      candidates = active_workers.reject { |w| w.equal?(excluding) }
      return nil if candidates.empty?

      loads = candidates.each_with_object({}) { |w, h| h[w] = 0 }
      @affinity.each_value do |owner|
        loads[owner] += 1 if loads.key?(owner)
      end
      candidates.min_by { |w| loads[w] }
    end

    def active_workers
      @threads.reject { |w| @draining[w] }
    end

    def must_exit?(worker)
      # Full shutdown: every worker leaves once no work remains anywhere.
      return true if @shutdown && !busy_locked? && @jobs.empty?

      if @draining[worker] && !@shutdown
        # A resized-away worker exits once its current block is done and no
        # consumer remains bound to it.
        return true if @in_flight[worker].nil? && owns_nothing?(worker)
      end

      false
    end

    def owns_nothing?(worker)
      @affinity.each_value { |owner| return false if owner.equal?(worker) }
      true
    end

    def busy_locked?
      !@jobs.empty? || !@in_flight.values.compact.empty?
    end

    def monotonic_time
      Process.clock_gettime(Process::CLOCK_MONOTONIC)
    end
  end
end
