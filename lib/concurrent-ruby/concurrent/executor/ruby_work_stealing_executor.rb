require 'thread'
require 'concurrent/atomic/event'
require 'concurrent/concern/logging'
require 'concurrent/executor/ruby_executor_service'

module Concurrent

  # A fixed-size thread pool in which every worker owns a private
  # double-ended queue (deque).  Tasks submitted from inside a running task
  # (work which recursively splits into smaller tasks) through the ordinary
  # {#post} protocol enter the *local* deque of the worker executing that task;
  # tasks submitted from outside enter a separate shared inbox.  This removes
  # the single shared work queue as a contention point.
  #
  # The submission protocol is exactly the one of the other executor services:
  # {#post} returns +true+ for accepted work, raises +ArgumentError+ without a
  # block, and applies the configured +fallback_policy+ once shutting down.
  #
  # Taking rules (both ends of the deques are covered):
  #
  # * Local take (the owner): pops the *front* (most recently pushed end) of
  #   its own deque, i.e. LIFO over the work it forked.
  # * Steal (any other worker): pops the *back* (oldest end) of a victim
  #   deque, the end the owner never pops from.  Every removal is performed
  #   atomically under the victim deque's own lock, so when the last task of a
  #   deque is raced for by its owner and one or more thieves the task is
  #   removed first and is executed by exactly one of them, exactly once.
  # * The external inbox is drained only after every deque has been found
  #   empty, so locally forked work is always taken before global work.
  #
  # Idle workers block on a condition variable (never spin) and are woken when
  # work is enqueued or when the pool shuts down.  Orderly shutdown runs every
  # accepted task to completion; +kill+ discards queued tasks like the other
  # pools.
  #
  # @!visibility private
  class RubyWorkStealingExecutor < RubyExecutorService
    include Concern::Logging

    # @param [Integer] num_threads the fixed number of worker threads
    # @param [Hash] opts the options
    # @option opts [Symbol] :fallback_policy (:abort) one of
    #   +:abort+, +:discard+, +:caller_runs+
    # @option opts [String] :name optional name used for thread naming
    # @raise [ArgumentError] if +num_threads+ is less than one or the fallback
    #   policy is unknown
    def initialize(num_threads, opts = {})
      super(opts.merge(min_threads: num_threads, max_threads: num_threads))
    end

    # Submit a task.
    #
    # When called from a thread currently executing a task *on this pool* the
    # task is accepted onto that worker's own deque; otherwise it enters the
    # shared external inbox.  Acceptance semantics are identical to the other
    # executor services.
    #
    # @!macro executor_service_method_post
    def post(*args, &task)
      raise ArgumentError.new('no block given') unless block_given?

      carrier   = Thread.current[:concurrent_ws_pool]
      on_this   = carrier && carrier.first.equal?(self)
      worker_id = on_this ? carrier.last : nil

      deferred_action = synchronize do
        ns_reset_if_forked
        if ns_running?
          if on_this
            ns_execute_local(worker_id, *args, &task)
          else
            ns_execute(*args, &task)
          end
        else
          fallback_action(*args, &task)
        end
      end

      deferred_action ? deferred_action.call : true
    end

    # @return [Integer] the fixed number of worker threads
    def length
      synchronize { @workers.length }
    end

    # @return [Integer] accepted but not yet started tasks (inbox plus all
    #   per-worker deques)
    def queue_length
      synchronize { @inbox.length + @deques.map(&:length).inject(0, :+) }
    end

    # @return [Integer] number of tasks accepted since construction
    def scheduled_task_count
      synchronize { @scheduled_task_count }
    end

    # @return [Integer] number of tasks finished since construction, whether
    #   they raised or not
    def completed_task_count
      synchronize { @completed_task_count }
    end

    # @return [Array<Integer>] number of not-yet-started tasks in each worker
    #   deque, indexed by worker id (the inbox is not included)
    # @!visibility private
    def local_queue_lengths
      synchronize { @deques.map(&:length) }
    end

    # @return [Array<Array<Symbol>>] acquisition source (+:local+, +:steal+ or
    #   +:external+) of every finished task, indexed by worker id
    # @!visibility private
    def acquisition_sources
      synchronize { @stats.map(&:dup) }
    end

    # @return [Array<Integer>] how many times each worker blocked for lack of
    #   work, indexed by worker id
    # @!visibility private
    def idle_wait_counts
      synchronize { @idle_waits.dup }
    end

    # @return [Array<String, nil>] status of each worker thread, indexed by
    #   worker id
    # @!visibility private
    def worker_statuses
      synchronize { @workers.map { |w| w.thread&.status } }
    end

    # Called by workers. Acquire the next job for worker +worker_id+, blocking
    # (without spinning) until work appears or the pool stops. On shutdown the
    # remaining accepted work is drained; the worker terminates only once every
    # deque and the inbox are empty.
    #
    # @return [Array(Symbol, Job), nil] +nil+ when the worker must terminate
    #
    # @!visibility private
    def acquire_or_wait(worker_id)
      loop do
        job, source = take_any(worker_id)
        return [source, job] if job

        result = synchronize do
          # Re-check under the lock so an enqueue can never race a wait:
          # every enqueue commits under this same lock and signals afterwards.
          job, source = take_any(worker_id)
          if job
            [:got, job, source]
          elsif ns_running?
            @idle_waits[worker_id] += 1
            ns_wait
            [:woken]
          else
            [:stop]
          end
        end

        case result.first
        when :got then return [result[2], result[1]]
        when :stop then return nil
        # :woken -> loop and retry
        end
      end
    end

    # Called by workers after every acquired job has run.
    #
    # @!visibility private
    def job_completed(source, worker_id)
      synchronize do
        @completed_task_count += 1
        @stats[worker_id] << source
        # During an orderly shutdown the pool is finished exactly when every
        # accepted task has completed and no queued task remains. Comparing the
        # counters (instead of queue emptiness alone) also covers tasks still
        # in flight on other workers, so waiters are never released early.
        # The broadcast wakes workers already blocked waiting for work.
        if !ns_running? && !ns_shutdown? &&
           @completed_task_count == @scheduled_task_count &&
           @inbox.empty? && @deques.all?(&:empty?)
          stopped_event.set
          ns_broadcast
        end
      end
    end

    private

    # @!visibility private
    def ns_initialize(opts)
      num_threads = opts.fetch(:max_threads).to_i
      raise ArgumentError.new('number of threads must be greater than zero') if num_threads < 1

      @fallback_policy = opts.fetch(:fallback_policy, :abort)
      unless FALLBACK_POLICIES.include?(@fallback_policy)
        raise ArgumentError.new("#{@fallback_policy} is not a valid fallback policy")
      end

      @num_threads          = num_threads
      @inbox                = [] # externally submitted, not yet started tasks
      @deques               = Array.new(num_threads) { Deque.new }
      @workers              = []
      @scheduled_task_count = 0
      @completed_task_count = 0
      @stats                = Array.new(num_threads) { [] }
      @idle_waits           = Array.new(num_threads, 0)
      @ruby_pid             = $$

      @num_threads.times { |i| @workers << Worker.new(self, i) }
    end

    # External submission under the pool lock. Fork reset was already done by
    # {RubyExecutorService#post} before this method is reached.
    #
    # @!visibility private
    def ns_execute(*args, &task)
      @inbox << Job.new(task, args)
      @scheduled_task_count += 1
      ns_signal # wake one idle worker
      nil
    end

    # Submission from one of this pool's own worker threads, under the pool
    # lock: the job goes straight onto that worker's private deque, bypassing
    # the shared inbox; one idle thief is woken so it may help with the backlog.
    # After a fork the inherited thread-local carrier is stale (its worker no
    # longer exists); such a submission is routed through the inbox instead.
    #
    # @!visibility private
    def ns_execute_local(worker_id, *args, &task)
      if $$ == @ruby_pid && worker_id < @deques.length
        @deques[worker_id] << Job.new(task, args)
      else
        @inbox << Job.new(task, args)
      end
      @scheduled_task_count += 1
      ns_signal
      nil
    end

    # @!visibility private
    def ns_shutdown_execution
      ns_reset_if_forked
      # If no accepted task is pending or in flight the completion callback
      # (job_completed) will never fire again: finish here. Otherwise the last
      # finishing task sets the stopped event, which guarantees waiters are
      # released only after every accepted task has run.
      stopped_event.set if @completed_task_count == @scheduled_task_count
      ns_broadcast # wake every idle worker so it observes the shutdown state
    end

    # @!visibility private
    def ns_kill_execution
      ns_reset_if_forked
      @inbox.clear
      @deques.each(&:clear)
      @workers.each(&:kill)
    end

    # Search order for a worker, without holding the pool lock:
    # own deque front -> other deques' backs (steal) -> external inbox.
    #
    # @!visibility private
    def take_any(worker_id)
      if (job = @deques[worker_id].pop_front)
        return [job, :local]
      end

      # Pseudo-random starting victim so idle workers do not all contend the
      # same deque's back.
      offset = rand(@num_threads)
      @num_threads.times do |i|
        victim_id = (offset + i) % @num_threads
        next if victim_id == worker_id
        if (job = @deques[victim_id].pop_back)
          return [job, :steal]
        end
      end

      if (job = drain_inbox_to_own(worker_id))
        return [job, :external]
      end

      [nil, nil]
    end

    # Atomically (under the pool lock) take the whole external inbox, move it
    # onto the worker's own deque and hand back the oldest job for immediate
    # execution. Doing the move under the pool lock guarantees the jobs are
    # visible in a deque in the same critical section in which the inbox
    # becomes empty: no shutdown-completion check can ever observe an empty
    # pool while an accepted inbox batch is in flight.
    #
    # @!visibility private
    def drain_inbox_to_own(worker_id)
      synchronize do
        return nil if @inbox.empty?
        batch  = @inbox
        @inbox = []
        @deques[worker_id].accept_inbox_batch(batch)
      end
    end

    # @!visibility private
    def ns_reset_if_forked
      return unless $$ != @ruby_pid
      @inbox.clear
      @deques.each(&:clear)
      @workers.clear
      @scheduled_task_count = 0
      @completed_task_count = 0
      @ruby_pid             = $$
    end

    # Per-worker double ended queue. The single owner pops the *front*
    # (LIFO over its own pushes); thieves pop the *back* (the oldest item).
    # Both ends share one mutex, so when owner and thief(ves) race for the
    # final remaining item exactly one +pop+ succeeds: the item leaves the
    # deque before anybody executes it and therefore runs exactly once.
    #
    # @!visibility private
    class Deque
      def initialize
        @lock  = Mutex.new
        @items = []
      end

      # Owner push: newest forked work lands at the front.
      def <<(job)
        @lock.synchronize { @items.unshift(job) }
      end

      # Owner take: most recently pushed end.
      def pop_front
        @lock.synchronize { @items.shift }
      end

      # Thief take: oldest end.
      def pop_back
        @lock.synchronize { @items.pop }
      end

      # Move an inbox batch (oldest first) onto the front of this deque and
      # remove/return its oldest job so the draining worker executes it
      # immediately. External submissions keep their FIFO order at the front.
      def accept_inbox_batch(batch)
        @lock.synchronize do
          @items.unshift(*batch)
          @items.shift # the oldest external job, detached => runs once
        end
      end

      def length
        @lock.synchronize { @items.length }
      end

      def empty?
        @lock.synchronize { @items.empty? }
      end

      def clear
        @lock.synchronize { @items.clear }
      end
    end

    private_constant :Deque

    # @!visibility private
    class Worker
      include Concern::Logging

      attr_reader :thread

      def initialize(pool, id)
        @pool   = pool
        @id     = id
        @thread = Thread.new { run(pool, id) }
        if @thread.respond_to?(:name=)
          @thread.name = [pool.name, 'work-stealing-worker', id].compact.join('-')
        end
      end

      def kill
        @thread.kill
      end

      private

      def run(pool, id)
        loop do
          acquired = pool.acquire_or_wait(id)
          break unless acquired

          source, job = acquired
          execute(pool, id, job, source)
        end
      rescue Exception => ex
        # Ordinary task errors are contained by #execute; only a fatal error
        # raised outside task code kills the worker.
        log ERROR, ex
      end

      def execute(pool, id, job, source)
        Thread.current[:concurrent_ws_pool] = [pool, id]
        begin
          job.task.call(*job.args)
        rescue => ex
          # A failing task must not kill the worker or lose the rest of the
          # accepted work; behave like the other pools and log it.
          log DEBUG, ex
        ensure
          Thread.current[:concurrent_ws_pool] = nil
          pool.job_completed(source, id)
        end
      end
    end

    private_constant :Worker

    # @!visibility private
    Job = Struct.new(:task, :args)
  end
end
