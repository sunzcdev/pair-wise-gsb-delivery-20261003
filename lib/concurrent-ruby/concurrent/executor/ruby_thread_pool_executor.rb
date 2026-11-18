require 'thread'
require 'concurrent/atomic/event'
require 'concurrent/concern/logging'
require 'concurrent/executor/ruby_executor_service'
require 'concurrent/utility/monotonic_time'
require 'concurrent/collection/timeout_queue'

module Concurrent

  # @!macro thread_pool_executor
  # @!macro thread_pool_options
  # @!visibility private
  class RubyThreadPoolExecutor < RubyExecutorService
    include Concern::Deprecation

    # @!macro thread_pool_executor_constant_default_max_pool_size
    DEFAULT_MAX_POOL_SIZE      = 2_147_483_647 # java.lang.Integer::MAX_VALUE

    # @!macro thread_pool_executor_constant_default_min_pool_size
    DEFAULT_MIN_POOL_SIZE      = 0

    # @!macro thread_pool_executor_constant_default_max_queue_size
    DEFAULT_MAX_QUEUE_SIZE     = 0

    # @!macro thread_pool_executor_constant_default_thread_timeout
    DEFAULT_THREAD_IDLETIMEOUT = 60

    # @!macro thread_pool_executor_constant_default_synchronous
    DEFAULT_SYNCHRONOUS = false

    # @!macro thread_pool_executor_attr_reader_max_length
    attr_reader :max_length

    # @!macro thread_pool_executor_attr_reader_min_length
    attr_reader :min_length

    # @!macro thread_pool_executor_attr_reader_idletime
    attr_reader :idletime

    # @!macro thread_pool_executor_attr_reader_max_queue
    attr_reader :max_queue

    # @!macro thread_pool_executor_attr_reader_synchronous
    attr_reader :synchronous

    # @!macro thread_pool_executor_method_initialize
    def initialize(opts = {})
      super(opts)
    end

    # @!macro thread_pool_executor_attr_reader_largest_length
    def largest_length
      synchronize { @largest_length }
    end

    # @!macro thread_pool_executor_attr_reader_scheduled_task_count
    def scheduled_task_count
      synchronize { @scheduled_task_count }
    end

    # @!macro thread_pool_executor_attr_reader_completed_task_count
    def completed_task_count
      synchronize { @completed_task_count }
    end

    # @!macro thread_pool_executor_method_active_count
    #
    # Only live worker threads are counted. A worker whose thread has already
    # terminated is excluded even if its termination has not been processed by
    # the pool yet.
    def active_count
      synchronize do
        idle = @ready.count { |(worker, _last_message)| worker.alive? }
        @pool.count(&:alive?) - idle
      end
    end

    # @!macro executor_service_method_can_overflow_question
    def can_overflow?
      synchronize { ns_limited_queue? }
    end

    # @!macro thread_pool_executor_attr_reader_length
    #
    # Only live worker threads are counted, so a worker which has terminated
    # (for example because its task called `Thread.exit`) is never reported as
    # part of the pool.
    def length
      synchronize { @pool.count(&:alive?) }
    end

    # @!macro thread_pool_executor_attr_reader_queue_length
    def queue_length
      synchronize { @queue.length }
    end

    # @!macro thread_pool_executor_attr_reader_remaining_capacity
    def remaining_capacity
      synchronize do
        if ns_limited_queue?
          @max_queue - @queue.length
        else
          -1
        end
      end
    end

    # Removes the worker if it can be pruned.
    #
    # @return [true, false] true when the worker must stop (it was pruned, or it
    #   had already been terminated by the pool)
    #
    # @!visibility private
    def prune_worker(worker)
      synchronize do
        # Already on its way out: make sure the worker leaves its loop instead
        # of blocking on a queue no one will feed again.
        return true if worker.terminal?

        return false unless ns_prunable_capacity > 0

        remove_worker worker
      end
    end

    # Unregisters a worker which the pool asked to terminate (pruning or
    # shutdown). This is an ordered termination: the worker moves to its
    # terminal state first so that its own cleanup does not mistake it for a
    # crash and does not schedule a replacement.
    #
    # @return [true, false] false when the worker was already terminal
    #
    # @!visibility private
    def remove_worker(worker, terminal_state = :stopped)
      synchronize do
        return false unless worker.terminate!(terminal_state)

        ns_remove_ready_worker worker
        ns_remove_busy_worker worker
        true
      end
    end

    # @!visibility private
    def ready_worker(worker, last_message)
      synchronize { ns_ready_worker worker, last_message }
    end

    # Called from the worker's own thread when its body exits for any reason.
    # A worker which the pool already terminated on purpose needs no
    # replacement; anything else is an unexpected death and is replaced so the
    # queue cannot wait forever for a worker which no longer exists.
    #
    # Logging happens after the lock is released: the logger is
    # user-configurable and must never run while the pool lock is held.
    #
    # @!visibility private
    def worker_terminated(worker)
      synchronize { ns_worker_terminated worker }
      flush_pending_log_messages
      nil
    end

    # @!visibility private
    def worker_task_completed
      synchronize { @completed_task_count += 1 }
    end

    # The number of worker threads which the runtime refused to create since
    # the pool was constructed (normally a `ThreadError`). A non-zero value
    # means the pool may be running with fewer workers than configured; every
    # failure is also logged. Used to keep a failed replacement observable
    # instead of pretending the pool is healthy.
    #
    # @return [Integer] the number of failed worker starts
    def worker_spawn_failures
      synchronize { @worker_spawn_failures }
    end

    # @!macro thread_pool_executor_method_prune_pool
    def prune_pool
      deprecated "#prune_pool has no effect and will be removed in next the release, see https://github.com/ruby-concurrency/concurrent-ruby/pull/1082."
    end

    # Messages about stranded tasks are queued by the lifecycle code under the
    # pool lock and logged here, with the lock released.
    #
    # @!visibility private
    def shutdown
      result = super
      flush_pending_log_messages
      result
    end

    # @!visibility private
    def kill
      result = super
      flush_pending_log_messages
      result
    end

    private

    # @!visibility private
    def ns_initialize(opts)
      @min_length      = opts.fetch(:min_threads, DEFAULT_MIN_POOL_SIZE).to_i
      @max_length      = opts.fetch(:max_threads, DEFAULT_MAX_POOL_SIZE).to_i
      @idletime        = opts.fetch(:idletime, DEFAULT_THREAD_IDLETIMEOUT).to_i
      @max_queue       = opts.fetch(:max_queue, DEFAULT_MAX_QUEUE_SIZE).to_i
      @synchronous     = opts.fetch(:synchronous, DEFAULT_SYNCHRONOUS)
      @fallback_policy = opts.fetch(:fallback_policy, :abort)

      raise ArgumentError.new("`synchronous` cannot be set unless `max_queue` is 0") if @synchronous && @max_queue > 0
      raise ArgumentError.new("#{@fallback_policy} is not a valid fallback policy") unless FALLBACK_POLICIES.include?(@fallback_policy)
      raise ArgumentError.new("`max_threads` cannot be less than #{DEFAULT_MIN_POOL_SIZE}") if @max_length < DEFAULT_MIN_POOL_SIZE
      raise ArgumentError.new("`max_threads` cannot be greater than #{DEFAULT_MAX_POOL_SIZE}") if @max_length > DEFAULT_MAX_POOL_SIZE
      raise ArgumentError.new("`min_threads` cannot be less than #{DEFAULT_MIN_POOL_SIZE}") if @min_length < DEFAULT_MIN_POOL_SIZE
      raise ArgumentError.new("`min_threads` cannot be more than `max_threads`") if min_length > max_length

      @pool                         = [] # all registered workers
      @ready                        = [] # used as a stash (most idle worker is at the start)
      @queue                        = [] # used as queue
      # @ready or @queue is empty at all times
      @scheduled_task_count         = 0
      @completed_task_count         = 0
      @worker_spawn_failures        = 0
      @largest_length               = 0
      @workers_counter              = 0
      @pending_log_messages         = [] # flushed outside the lock
      @ruby_pid                     = $$ # detects if Ruby has forked
    end

    # @!visibility private
    def ns_limited_queue?
      @max_queue != 0
    end

    # @!visibility private
    def ns_execute(*args, &task)
      ns_reset_if_forked

      if ns_assign_worker(*args, &task) || ns_enqueue(*args, &task)
        @scheduled_task_count += 1
        nil
      else
        fallback_action(*args, &task)
      end
    end

    # @!visibility private
    def ns_shutdown_execution
      ns_reset_if_forked

      if @queue.empty?
        # Nothing is pending: every worker stops after its current task (if any).
        @pool.each(&:stop)
        stopped_event.set if ns_terminated?
        return
      end

      # Tasks accepted before the shutdown still have to run. Busy workers keep
      # draining the queue (they are not sent :stop until it is empty), so the
      # queue can only be stranded when no live worker is left.
      if ns_live_worker_count.zero? && !ns_pending_worker_cleanup?
        ns_replace_worker

        if ns_live_worker_count.zero? && !ns_pending_worker_cleanup?
          # Nothing alive and nothing about to be cleaned up: these tasks can
          # never run. Drop them rather than let `#wait_for_termination` wait
          # forever for a worker which no longer exists.
          stranded = @queue.length
          @queue.clear
          stopped_event.set
          ns_enqueue_log "shutdown discarded #{stranded} queued task(s): no worker is alive"
        end
      end

      @pool.each(&:stop) if @queue.empty?
      stopped_event.set if ns_terminated?
    end

    # @!visibility private
    def ns_kill_execution
      discarded = @queue.length
      @queue.clear

      @pool.each do |worker|
        # Mark first so the worker's own cleanup does not treat the kill as an
        # unexpected death and try to spawn a replacement.
        worker.terminate! :killed
        worker.kill
      end
      @pool.clear
      @ready.clear

      if discarded > 0
        ns_enqueue_log "killed with #{discarded} queued task(s) which were discarded"
      end
    end

    # tries to assign task to a worker, tries to get one from @ready or to create new one
    # @return [true, false] if task is assigned to a worker
    #
    # @!visibility private
    def ns_assign_worker(*args, &task)
      # keep growing if the pool is not at the minimum yet
      worker = ns_take_idle_worker || ns_spawn_worker
      if worker
        worker.busy!
        worker << [task, args]
        true
      else
        false
      end
    end

    # tries to enqueue task
    # @return [true, false] if enqueued
    #
    # @!visibility private
    def ns_enqueue(*args, &task)
      return false if @synchronous

      if !ns_limited_queue? || @queue.size < @max_queue
        @queue << [task, args]
        true
      else
        false
      end
    end

    # Handles the exit of a worker's thread.
    #
    # A worker which the pool already moved to a terminal state was stopped on
    # purpose (pruning, orderly shutdown, kill) and needs no replacement.
    # Anything else is an unexpected death: the worker is dropped, its
    # in-progress task is abandoned (never re-run) and, while running, the pool
    # is replenished so pending tasks are not stranded.
    #
    # Called with the pool lock held. Returns a description of the event so the
    # caller can log it outside the lock, or nil when nothing has to be logged.
    #
    # @!visibility private
    def ns_worker_terminated(worker)
      if worker.terminal?
        ns_remove_ready_worker worker
        ns_remove_busy_worker worker
        nil
      else
        ns_worker_died worker
      end
    end

    # @!visibility private
    def ns_worker_died(worker)
      in_progress = worker.busy?
      return nil unless worker.die!

      ns_remove_ready_worker worker
      ns_remove_busy_worker worker

      # A dead worker always gets a replacement while the pool is running, so
      # pending tasks are never left waiting for a worker which no longer
      # exists. During an orderly shutdown a replacement is only needed when no
      # live worker is left to drain the queue.
      spawn_error = nil
      if running? || (!@queue.empty? && ns_live_worker_count.zero?)
        spawn_error = ns_replace_worker
      end

      # Once the pool is shutting down and nothing alive is left, queued tasks
      # can never run. Report and drop them instead of letting
      # `#wait_for_termination` wait forever. While the pool is still running
      # the queue is kept: posting more work can bring workers back, and
      # dropping accepted tasks silently would be worse than a reported stall.
      stranded = 0
      if !running? && !@queue.empty? && ns_live_worker_count.zero?
        stranded = @queue.length
        @queue.clear
        stopped_event.set
      end

      message = "worker #{worker.id} died unexpectedly"
      if (cause = worker.death_cause)
        # A fatal condition is reported as such, never turned into a business
        # success. The task is not replayed because it already started.
        message << " (#{cause.class}: #{cause.message})"
      end
      if in_progress
        message << '; its in-progress task was abandoned and will not be re-run'
      end
      message << "; replacement worker could not be started (#{spawn_error.class}: #{spawn_error.message})" if spawn_error
      message << "; #{stranded} queued task(s) can no longer run" if stranded > 0
      ns_enqueue_log message

      nil
    end

    # Creates a new worker in the `:starting` state.
    # @return [Worker, nil] nil when max capacity is reached or the runtime
    #   refuses to start another thread
    #
    # @!visibility private
    def ns_spawn_worker
      # Workers whose thread already ended but whose cleanup has not run yet are
      # not alive and must not occupy a slot; otherwise a replacement could be
      # refused for a pool which in fact has capacity.
      return if ns_live_worker_count >= @max_length

      @workers_counter += 1
      worker = Worker.new(self, @workers_counter)
      @pool << worker
      @largest_length = @pool.length if @pool.length > @largest_length
      worker
    rescue ThreadError => ex
      # The runtime refused to create a thread. Remember it so that a pool
      # silently running with fewer workers than configured is observable.
      @worker_spawn_failures += 1
      @last_worker_spawn_error = ex
      nil
    end

    # Starts one worker to replace a lost one.
    #
    # Idle workers do not poll the pool queue: they only receive work when the
    # pool hands it to them (on `#post` or when they report ready). A
    # replacement therefore takes a pending task directly, otherwise tasks
    # already accepted would wait for a worker which no longer exists. With
    # nothing pending it either joins the idle list (while running, restoring
    # `min_length`) or is stopped (during shutdown).
    #
    # A failure to start the thread is reported and returned, never raised into
    # the caller (which normally is a dying worker's `ensure` block). The
    # message is queued for logging outside the pool lock, since the logger is
    # user-configurable and must not run while the lock is held.
    #
    # @return [Exception, nil] the thread creation error, if any
    #
    # @!visibility private
    def ns_replace_worker
      @last_worker_spawn_error = nil

      worker = ns_spawn_worker
      if worker.nil?
        spawn_error = @last_worker_spawn_error
        @last_worker_spawn_error = nil
        # A replacement which cannot be started must be visible, not silently
        # absorbed: without it the pool is short of a worker.
        if spawn_error
          ns_enqueue_log "could not start a replacement worker " \
                         "(#{spawn_error.class}: #{spawn_error.message})"
        end
        return spawn_error
      end

      if ns_hand_queued_task(worker)
        # The replacement picked up pending work.
      elsif running?
        worker.idle!
        @ready.push([worker, Concurrent.monotonic_time])
      else
        # Shutting down with nothing left to run: let the new worker exit.
        worker.stop
      end

      @last_worker_spawn_error = nil
      nil
    end

    # Hands the oldest queued task to the given worker.
    #
    # @return [true, false] whether a task was handed over
    #
    # @!visibility private
    def ns_hand_queued_task(worker)
      task_and_args = @queue.shift
      return false unless task_and_args

      worker.busy!
      worker << task_and_args
      true
    end

    # Queues an error message for logging outside the pool lock.
    #
    # @!visibility private
    def ns_enqueue_log(message)
      @pending_log_messages << message
    end

    # Flushes queued log messages. Must be called without the pool lock held.
    #
    # @!visibility private
    def flush_pending_log_messages
      messages = synchronize do
        pending = @pending_log_messages
        @pending_log_messages = []
        pending
      end
      messages.each { |message| log ERROR, 'RubyThreadPoolExecutor', message }
      nil
    end

    # @!visibility private
    def ns_live_worker_count
      @pool.count(&:alive?)
    end

    # A registered worker whose thread already ended but whose cleanup has not
    # run yet. It is not alive, so it must not count as capacity, yet it also
    # must not make the pool conclude that its queue is permanently stranded.
    #
    # @!visibility private
    def ns_pending_worker_cleanup?
      @pool.any? { |worker| !worker.alive? }
    end

    # @!visibility private
    def ns_idle_worker_available?
      @ready.any? { |worker, _| worker.alive? }
    end

    # Pops the most idle live worker, growing the pool instead while it is still
    # below `min_length`. Entries for workers which are already gone are
    # discarded; their own termination handler unregisters them.
    #
    # @!visibility private
    def ns_take_idle_worker
      # Keep growing until the pool holds `min_length` live workers, even if an
      # idle one is available.
      return nil if ns_live_worker_count < @min_length

      while (entry = @ready.pop)
        worker = entry.first
        return worker if worker.alive?
      end
      nil
    end

    # handle ready worker, giving it new job or assigning back to @ready
    #
    # @!visibility private
    def ns_ready_worker(worker, last_message)
      # The worker may already be on its way out (e.g. the pool was killed
      # while the task was finishing), in which case it must not re-enter the pool.
      return if worker.terminal?

      worker.idle!
      task_and_args = @queue.shift
      if task_and_args
        worker.busy!
        worker << task_and_args
      else
        # stop workers when !running?, do not return them to @ready
        if running?
          @ready.push([worker, last_message])
        else
          worker.stop
        end
      end
    end

    # removes a worker which is not tracked in @ready
    #
    # @!visibility private
    def ns_remove_busy_worker(worker)
      @pool.delete(worker)
      stopped_event.set if ns_terminated?
      true
    end

    # @!visibility private
    def ns_remove_ready_worker(worker)
      if index = @ready.index { |rw, _| rw == worker }
        @ready.delete_at(index)
      end
      true
    end

    # The pool has terminated once it is not running, no worker is registered
    # any more and nothing is left in the queue. Registered workers are removed
    # by their own thread on exit, so a worker whose thread died unexpectedly
    # (e.g. after `Thread.exit`) cannot keep `#wait_for_termination` waiting.
    #
    # @!visibility private
    def ns_terminated?
      !running? && @pool.empty? && @queue.empty?
    end

    # @return [Integer] number of excess idle workers which can be removed without
    #                   going below min_length, or all workers if not running
    #
    # @!visibility private
    def ns_prunable_capacity
      if running?
        [ns_live_worker_count - @min_length, @ready.count { |worker, _| worker.alive? }].min
      else
        ns_live_worker_count
      end
    end

    # @!visibility private
    def ns_reset_if_forked
      if $$ != @ruby_pid
        @queue.clear
        @ready.clear
        @pool.clear
        @scheduled_task_count        = 0
        @completed_task_count        = 0
        @worker_spawn_failures       = 0
        @last_worker_spawn_error     = nil
        @pending_log_messages        = []
        @largest_length              = 0
        @workers_counter             = 0
        @ruby_pid                    = $$
      end
    end

    # @!visibility private
    class Worker
      include Concern::Logging

      # A worker is `:starting` until the pool hands it a task or parks it as
      # idle. After that it alternates between `:idle` and `:busy`. The terminal
      # states are `:stopped` (pruned or orderly shutdown), `:killed` (the pool
      # was killed) and `:died` (the thread ended on its own). Terminal workers
      # are never returned to the pool and never receive more work.
      STATES = [:starting, :idle, :busy, :stopped, :killed, :died].freeze
      TERMINAL_STATES = [:stopped, :killed, :died].freeze

      attr_reader :id

      def initialize(pool, id)
        # instance variables accessed only under pool's lock so no need to sync here again
        @id          = id
        @state       = :starting
        @death_cause = nil
        @queue       = Collection::TimeoutQueue.new
        @pool        = pool
        @thread      = create_worker @queue, pool, pool.idletime

        if @thread.respond_to?(:name=)
          @thread.name = [pool.name, 'worker', id].compact.join('-')
        end
      end

      # @return [Symbol] one of {STATES}
      def state
        @state
      end

      # The fatal exception which ended this worker, if any.
      #
      # @return [Exception, nil]
      attr_reader :death_cause

      def terminal?
        TERMINAL_STATES.include?(@state)
      end

      def busy?
        @state == :busy
      end

      def alive?
        @thread.alive?
      end

      def idle!
        @state = :idle
      end

      def busy!
        @state = :busy
      end

      # Moves the worker to a terminal state. Returns false when it already was
      # terminal, which makes the transition idempotent: only the first caller
      # owns the cleanup and the replacement decision.
      def die!
        transition_to :died
      end

      def terminate!(state = :stopped)
        raise ArgumentError, "not a terminal state: #{state}" unless TERMINAL_STATES.include?(state)
        transition_to state
      end

      def <<(message)
        @queue << message
      end

      def stop
        @queue << :stop
      end

      def kill
        @thread.kill
      end

      private

      def transition_to(state)
        return false if terminal?
        @state = state
        true
      end

      def create_worker(queue, pool, idletime)
        Thread.new(queue, pool, idletime) do |my_queue, my_pool, my_idletime|
          # A worker's death is reported by the pool itself (with context about
          # the abandoned task and the replacement), so Ruby's default
          # per-thread dump of the raw exception would only duplicate it.
          # `Thread#report_on_exception=` exists since Ruby 2.5.
          if Thread.current.respond_to?(:report_on_exception=)
            Thread.current.report_on_exception = false
          end

          begin
            catch(:stop) do
              prunable = true

              loop do
                timeout = prunable && my_pool.running? ? my_idletime : nil
                case message = my_queue.pop(timeout: timeout)
                when nil
                  throw :stop if my_pool.prune_worker(self)
                  prunable = false
                when :stop
                  my_pool.remove_worker(self)
                  throw :stop
                else
                  task, args = message
                  run_task my_pool, task, args
                  my_pool.ready_worker(self, Concurrent.monotonic_time)
                  prunable = true
                end
              end
            end
          ensure
            # Whatever ended this thread (orderly stop, pruning, kill,
            # Thread.exit, a fatal exception) the pool has to learn about it so
            # it can drop the worker and, when needed, start a replacement.
            # This runs last, so the pool sees the state the worker reached
            # above. It is a no-op for workers the pool already unregistered.
            my_pool.worker_terminated(self)
          end
        end
      end

      # Runs one task.
      #
      # A `StandardError` fails the task but keeps the worker alive: that is an
      # ordinary business failure, logged at DEBUG.
      #
      # Anything else is fatal for the worker: the exception is kept as the
      # worker's cause of death and the thread leaves the pool loop. The task is
      # never re-run (it already started), the worker loss is reported at ERROR
      # together with the cause and, while the pool is running, a replacement is
      # started. Nothing is silently turned into a success: both the abandoned
      # task and the lost worker are visible.
      def run_task(pool, task, args)
        task.call(*args)
        pool.worker_task_completed
      rescue StandardError => ex
        # let it fail
        log DEBUG, ex
      rescue Exception => ex
        # Recorded so the pool can report *why* the worker died, then leave the
        # pool loop. `throw` is used instead of re-raising so that this is not
        # mistaken for the task still being in progress.
        @death_cause = ex
        throw :stop
      end
    end

    private_constant :Worker
  end
end
