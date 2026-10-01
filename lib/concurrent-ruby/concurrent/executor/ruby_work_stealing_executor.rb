require 'thread'
require 'concurrent/atomic/atomic_fixnum'
require 'concurrent/concern/logging'
require 'concurrent/executor/ruby_executor_service'

module Concurrent

  # @!visibility private
  #
  # A double-ended queue owned by exactly one worker thread.
  #
  # Two ends, two strict access rules:
  #
  # * Local end (owner only): {#push} (LIFO push) and {#pop_local} (LIFO pop).
  #   A worker always resumes the *most recently* forked child first, which
  #   keeps the working set small for recursive, fork/join style workloads.
  # * Steal end (any other worker): {#pop_steal} removes the *oldest* item
  #   (FIFO), i.e. the largest available unit of work.
  #
  # Every removal holds the deque's own mutex, so the same item can be
  # observed by the owner and a thief simultaneously but is returned to at
  # most one of them: the last contended item executes exactly once.
  class WorkStealingDeque
    # @!visibility private
    Job = Struct.new(:task, :args, :id)

    def initialize
      @mutex = Mutex.new
      @items = []
    end

    # Called by the owning worker when it forks a child task (LIFO tail push).
    def push(job)
      @mutex.synchronize { @items.push(job) }
    end

    # Called by the owning worker (LIFO tail pop). Returns nil when empty.
    def pop_local
      @mutex.synchronize { @items.pop }
    end

    # Called by any *other* idle worker (FIFO head pop). Returns nil when empty.
    def pop_steal
      @mutex.synchronize { @items.shift }
    end

    def empty?
      @mutex.synchronize { @items.empty? }
    end

    def size
      @mutex.synchronize { @items.size }
    end

    def clear
      @mutex.synchronize { @items.clear }
    end
  end

  # @!macro abstract_executor_service_public_api
  # @!visibility private
  #
  # A fixed-size thread pool implementing the classic work-stealing algorithm.
  #
  # == Submission protocol
  #
  # The executor speaks the same protocol as every other executor service in
  # the library: {#post}/{#<<} submit a task and return according to the
  # configured +fallback_policy+ once the pool stops accepting work. Where a
  # submission goes depends solely on the submitting thread:
  #
  # * Tasks +post+ed from *inside* a running worker (forked children) are
  #   pushed to that worker's own deque under a lightweight park lock. No
  #   shared work queue is touched, so deeply recursive task graphs scale
  #   without a single contention point.
  # * Tasks +post+ed from *outside* any worker use the independent external
  #   submission entrance: the producer deposits the task (round-robin) onto a
  #   worker deque while holding only the park lock, never a deque lock. The
  #   entrance therefore never serialises against owners or thieves.
  #
  # == Claim rules
  #
  # A worker claims work in a strict order:
  #
  # 1. its own deque, LIFO (youngest forked child first - small working set);
  # 2. another worker's deque, FIFO, scanning from a randomised starting
  #    position (oldest/largest available unit first). Externally submitted
  #    work on a foreign deque is reported with +:external+ provenance; forked
  #    work with +[:stolen, victim_id]+.
  #
  # Each claim removes an item under the target deque's lock, therefore a
  # task is handed to exactly one worker even when several thieves and the
  # owner race for the last item.
  #
  # == Shutdown
  #
  # +shutdown+ stops accepting new work and lets workers drain every accepted
  # task (including tasks forked during the drain); +kill+ dismisses queued
  # work. Idle workers never busy-wait: they park on a condition variable and
  # are woken per accepted task (all at once on shutdown), with the emptiness
  # check and the park performed atomically so no wake-up is lost.
  class RubyWorkStealingExecutor < RubyExecutorService
    include Concern::Logging

    DEFAULT_MAX_THREADS = 2

    # Bounded park interval while shutting down with a busy peer. Short
    # enough to drain promptly once peers finish, long enough to release the
    # GVL to executing workers (a pure Thread.pass spin starves them under a
    # large backlog).
    SHUTDOWN_RESCAN_TIMEOUT = 0.01

    # Thread-local slot recording which worker (of which executor) the current
    # thread is running as. Used to route a task forked from inside a worker to
    # the worker's own deque instead of the external entrance.
    THREAD_LOCAL_WORKER = :concurrent_work_stealing_worker

    attr_reader :max_length

    def largest_length
      synchronize { @max_length }
    end

    def scheduled_task_count
      @scheduled_task_count.value
    end

    def completed_task_count
      @completed_task_count.value
    end

    def length
      synchronize { @workers.size }
    end

    # Number of tasks not yet started anywhere: the sum of the per-worker
    # deques (externally submitted work is stored there too).
    def queue_length
      synchronize { @workers.sum { |w| w.deque.size } }
    end

    # @!visibility private
    # Snapshot of worker-local counters, used by the correctness suite to
    # verify claim provenance. Key is the worker id; value is
    # +{local:, external:, stolen:}+ claim counts.
    def worker_claims
      synchronize do
        @workers.each_with_object({}) do |worker, memo|
          memo[worker.id] = {
            local: worker.local_claims,
            external: worker.external_claims,
            stolen: worker.stolen_claims
          }
        end
      end
    end

    # @!visibility private
    def worker_for(thread)
      synchronize { @workers.find { |w| w.thread.equal?(thread) } }
    end

    private

    def ns_initialize(opts = {})
      @max_length = opts.fetch(:max_threads, DEFAULT_MAX_THREADS).to_i
      raise ArgumentError.new('number of threads must be greater than zero') if @max_length < 1

      @fallback_policy = opts.fetch(:fallback_policy, :abort)
      raise ArgumentError.new("#{@fallback_policy} is not a valid fallback policy") unless FALLBACK_POLICIES.include?(@fallback_policy)

      # Independent external submission entrance is conceptual: accepted
      # external items are distributed round-robin onto the worker deques by
      # #ns_execute.
      @park_mutex = Mutex.new
      @park_condition = ConditionVariable.new

      @scheduled_task_count = AtomicFixnum.new(0)
      @completed_task_count = AtomicFixnum.new(0)
      @task_sequence = AtomicFixnum.new(0)
      # Round-robin cursor for external submission placement, and the set of
      # externally accepted task ids (used only to label claim provenance;
      # reclaimed when the task finishes).
      @external_rr_index = 0
      @external_job_ids = {}
      # Monotonic generation counter bumped once per producer signal (task
      # acceptance, shutdown) and once per finishing-task relay. A parked
      # worker compares the generation it last observed and re-scans before
      # sleeping again, so a signal landing in the window after a wake-up but
      # before the next wait cannot be swallowed.
      @wake_generation = 0
      # Number of workers currently blocked on @park_condition. All signalling
      # decisions (producer acceptance, task completion relay, shutdown) use
      # this to wake exactly the parked workers that have pending work, never
      # to spin.
      @parked_count = 0
      # Number of workers currently executing a task. Used during shutdown:
      # a worker may not terminate while a peer is still running, because that
      # peer is allowed to fork children after the queue probe.
      @busy_count = 0
      @active_workers = 0
      @killed = false
      @workers = []
      @ruby_pid = $$

      ns_spawn_workers
    end

    def ns_spawn_workers
      @max_length.times do |index|
        worker = Worker.new(index, WorkStealingDeque.new, self)
        @workers << worker
        @active_workers += 1
        ns_start_worker_thread(worker)
      end
    end

    def ns_start_worker_thread(worker)
      worker.thread = Thread.new(worker) do |w|
        Thread.current.abort_on_exception = false
        Thread.current.report_on_exception = false if Thread.current.respond_to?(:report_on_exception=)
        Thread.current.name = [name, 'worker', w.id].compact.join('-') if Thread.current.respond_to?(:name=)
        # Identity established for the thread's whole lifetime: every #post
        # performed by a running task is routed to this worker's own deque.
        Thread.current.thread_variable_set(THREAD_LOCAL_WORKER, w) if Thread.current.respond_to?(:thread_variable_set)
        begin
          worker_loop(w)
          worker_finished
        rescue Exception => error
          # Only reached for Exception-level failures raised while a task runs
          # (StandardError is contained inside #run_job). Replace the worker so
          # the fixed pool size and the shutdown-draining guarantees are
          # preserved; the failing task itself is gone for the same reason an
          # Exception kills a thread in RubyThreadPoolExecutor.
          log(ERROR, error)
          worker_failed(w, error)
        end
      end
    end

    # ---- submission -------------------------------------------------------

    public

    # Submission protocol. Identical contract to RubyExecutorService#post
    # (argument validation, deferred fallback action), with one shutdown
    # refinement: a task +post+ed from *inside a still-running worker* after
    # orderly shutdown began is a forked child of accepted, draining work and
    # must therefore also be accepted and drained. External submissions after
    # shutdown get the configured fallback policy as usual.
    def post(*args, &task)
      raise ArgumentError.new('no block given') unless block_given?
      deferred_action = synchronize do
        if running? || (shuttingdown? && worker_thread_still_busy?)
          ns_execute(*args, &task)
        else
          fallback_action(*args, &task)
        end
      end
      if deferred_action
        deferred_action.call
      else
        true
      end
    end

    private

    # Must be called under the object monitor.
    def worker_thread_still_busy?
      worker = Thread.current.thread_variable_get(THREAD_LOCAL_WORKER) if Thread.current.respond_to?(:thread_variable_get)
      worker && worker.executor.equal?(self)
    end

    # Called by #post while new work is admitted.
    def ns_execute(*args, &task)
      ns_reset_if_forked

      job = WorkStealingDeque::Job.new(task, args, @task_sequence.increment)
      worker = Thread.current.thread_variable_get(THREAD_LOCAL_WORKER) if Thread.current.respond_to?(:thread_variable_get)

      @park_mutex.synchronize do
        if worker && worker.executor.equal?(self)
          # Forked child: own deque, local (LIFO) end. This path never touches
          # the external entrance, so nested fan-out does not contend on it.
          worker.deque.push(job)
        else
          # Independent external submission entrance. Accepted items are
          # deposited round-robin onto worker deques; external producers only
          # ever take @park_mutex here and never a deque lock, so they do not
          # serialise against owners or thieves. Once placed, external work is
          # stealable by any idle worker exactly like forked work - the last
          # item contended by several workers is still removed atomically by
          # its deque.
          target = @workers[@external_rr_index % @workers.size]
          @external_rr_index += 1
          target.deque.push(job)
          @external_job_ids[job.id] = true
        end
        @scheduled_task_count.increment
        @wake_generation += 1
        # Wake a parked worker if one exists. When all workers are busy the
        # signal is unused; the task-completion relay wakes idle workers once
        # a worker returns from its task.
        @park_condition.signal if @parked_count > 0
      end
      nil
    end

    # ---- worker main loop -------------------------------------------------

    def worker_loop(worker)
      # Generation of the last producer signal this worker has already
      # observed. Carried across iterations so a signal that wakes us is
      # never swallowed by re-entering the park with a fresh snapshot.
      generation = @wake_generation
      loop do
        # The full claim scan runs OUTSIDE the park lock: this is exactly the
        # point a worker reaches when it returns from a (possibly long) task,
        # and the point at which it must steal work deposited on blocked
        # peers' deques.
        if (job = worker.deque.pop_local)
          run_job(worker, job, local_origin(job))
          next
        end

        if (stolen = steal_from_others(worker))
          job, victim = stolen
          run_job(worker, job, stolen_origin(job, victim))
          next
        end

        keep_going, generation = wait_for_work(worker, generation)
        return unless keep_going
      end
    end

    def steal_from_others(worker)
      others = workers_other_than(worker)
      offset = others.empty? ? 0 : rand(others.size)
      others.size.times do |i|
        victim = others[(offset + i) % others.size]
        if (job = victim.deque.pop_steal)
          return [job, victim]
        end
      end
      nil
    end

    def workers_other_than(worker)
      @workers.reject { |w| w.equal?(worker) }
    end

    # Emptiness probe using only deque-internal locks; safe from both the park
    # path and the task-completion relay.
    def queues_empty?
      @workers.all? { |w| w.deque.empty? }
    end

    # The canonical condition-variable park, with the emptiness probe and the
    # sleep as one atomic step under @park_mutex - the same monitor producers
    # hold while accepting tasks - so a task cannot be accepted in between and
    # missed. Every return from +wait+ hands control back to the caller for a
    # full claim scan, so a wake-up can never be swallowed.
    #
    # @return [Array(Boolean, Integer)] +[keep_running, generation]+ where
    #   +keep_running+ is +false+ only when shutdown completed with an empty
    #   pool and no executing peer.
    def wait_for_work(worker, generation)
      @park_mutex.synchronize do
        @parked_count += 1
        begin
          loop do
            if stop_event.set?
              # The pool is draining. Termination requires a quiescent
              # snapshot: no queued work anywhere AND no peer still executing
              # (a running peer is allowed to fork children that an exiting
              # worker could otherwise strand). A bounded parked wait, not a
              # spin, releases execution resources to busy peers; the
              # completing peer's broadcast ends the wait immediately.
              if queues_empty? && @busy_count.zero?
                return [false, generation]
              end
              @park_condition.wait(@park_mutex, SHUTDOWN_RESCAN_TIMEOUT)
              generation = @wake_generation
              next
            end

            return [true, generation] if generation != @wake_generation

            @park_condition.wait(@park_mutex)
            # Woken (or spurious): leave immediately for a full claim scan.
            return [true, @wake_generation]
          end
        ensure
          @parked_count -= 1
        end
      end
    end

    def local_origin(job)
      @external_job_ids.key?(job.id) ? [:external, nil] : [:local, nil]
    end

    def stolen_origin(job, victim)
      kind = @external_job_ids.key?(job.id) ? :external : :stolen
      [kind, victim.id]
    end

    def run_job(worker, job, origin)
      kind, _victim = origin
      case kind
      when :local    then worker.local_claims += 1
      when :external then worker.external_claims += 1
      when :stolen   then worker.stolen_claims += 1
      end
      @park_mutex.synchronize { @busy_count += 1 }
      begin
        job.task.call(*job.args)
      rescue => error
        # Task errors are contained exactly like the other Ruby executors:
        # they must never kill the worker or lose subsequent accepted work.
        log(DEBUG, error)
      ensure
        @completed_task_count.increment
        # Reclaim the provenance marker so long-running pools do not retain
        # one hash entry per ever-executed external task.
        @external_job_ids.delete(job.id)
        # Become non-busy under the park lock and wake waiting peers: this is
        # the "quiesced" notification shutdown drainage waits for, as well as
        # the scheduling point for forked/submitted work announced while we
        # were executing.
        @park_mutex.synchronize do
          @busy_count -= 1
          if stop_event.set?
            @wake_generation += 1
            @park_condition.broadcast
          else
            wake_parked_if_pending_locked
          end
        end
      end
    end

    # Must be called holding @park_mutex.
    def wake_parked_if_pending_locked
      if @parked_count > 0 && !queues_empty?
        @wake_generation += 1
        @park_condition.signal
      end
    end

    # ---- lifecycle --------------------------------------------------------

    def ns_shutdown_execution
      ns_reset_if_forked
      # Wake parked workers OUTSIDE the object monitor: a worker forking a
      # child holds the park lock and takes the object monitor (via #post), so
      # nesting object -> park here would deadlock against such a worker.
      @park_mutex.synchronize do
        @wake_generation += 1
        @park_condition.broadcast
      end
      ns_finish_if_no_workers
    end

    def ns_kill_execution
      ns_reset_if_forked
      @killed = true
      synchronize { @workers.each { |w| w.deque.clear } }
      @park_mutex.synchronize do
        @external_job_ids.clear
        @wake_generation += 1
        @park_condition.broadcast
      end
    end

    # Called when a worker's loop returns normally.
    def worker_finished
      synchronize do
        @active_workers -= 1
        ns_finish_if_no_workers
      end
    end

    # Called when a worker died from an Exception raised by a task. The worker
    # is replaced with a fresh thread reusing the same id/deque, unless the
    # pool was killed or terminated in the meantime.
    def worker_failed(worker, _error)
      synchronize do
        if @killed || stopped_event.set?
          @active_workers -= 1
          ns_finish_if_no_workers
        else
          # @active_workers is unchanged: the dead worker is replaced 1:1.
          ns_start_worker_thread(worker)
        end
      end
    end

    # Must be called holding the object monitor.
    def ns_finish_if_no_workers
      stopped_event.set if @active_workers.zero?
    end

    def ns_reset_if_forked
      return if $$ == @ruby_pid
      synchronize do
        return if $$ == @ruby_pid
        @workers.each(&:deque).each(&:clear)
        @workers.clear
        @active_workers = 0
        @busy_count = 0
        @parked_count = 0
        @wake_generation = 0
        @external_rr_index = 0
        @external_job_ids = {}
        @scheduled_task_count = AtomicFixnum.new(0)
        @completed_task_count = AtomicFixnum.new(0)
        @task_sequence = AtomicFixnum.new(0)
        @killed = false
        @ruby_pid = $$
        ns_spawn_workers
      end
    end

    # @!visibility private
    class Worker
      attr_reader :id, :deque
      attr_accessor :thread, :local_claims, :external_claims, :stolen_claims

      def initialize(id, deque, executor)
        @id = id
        @deque = deque
        @executor = executor
        @thread = nil
        @local_claims = 0
        @external_claims = 0
        @stolen_claims = 0
      end

      def executor
        @executor
      end
    end

    private_constant :Worker
  end
end
