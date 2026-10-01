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

    # @!macro thread_pool_executor_constant_default_max_blocking_compensation
    DEFAULT_MAX_BLOCKING_COMPENSATION = 0

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

    # @!macro thread_pool_executor_attr_reader_max_blocking_compensation
    attr_reader :max_blocking_compensation

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
    def active_count
      synchronize do
        @pool.length - @ready.length
      end
    end

    # @!macro thread_pool_executor_attr_reader_blocking_count
    def blocking_count
      synchronize { @blocking_count }
    end

    # @!macro thread_pool_executor_method_managed_block
    def managed_block
      raise ArgumentError.new('no block given') unless block_given?

      # Only a thread actually owned by this pool may claim a compensation
      # slot; a regular external (caller) thread can never forge one.
      worker, nested = synchronize do
        w = @worker_threads[Thread.current]
        w ? [w, ns_enter_managed_block(w)] : [nil, nil]
      end
      begin
        yield
      ensure
        # nested declarations count once, and the accounting is always
        # restored, even when the block raises.
        synchronize { ns_exit_managed_block(worker) } if worker && !nested
      end
    end

    # @!macro executor_service_method_can_overflow_question
    def can_overflow?
      synchronize { ns_limited_queue? }
    end

    # @!macro thread_pool_executor_attr_reader_length
    def length
      synchronize { @pool.length }
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

    # removes the worker if it can be pruned
    #
    # @return [true, false] if the worker was pruned
    #
    # @!visibility private
    def prune_worker(worker)
      synchronize do
        if ns_prunable_capacity > 0
          remove_worker worker
          true
        else
          false
        end
      end
    end

    # @!visibility private
    def remove_worker(worker)
      synchronize do
        ns_remove_ready_worker worker
        ns_remove_busy_worker worker
      end
    end

    # @!visibility private
    def ready_worker(worker, last_message, success = true, work_signaled = false)
      synchronize { ns_ready_worker worker, last_message, success, work_signaled }
    end

    # @!visibility private
    def worker_died(worker)
      synchronize { ns_worker_died worker }
    end

    # @!visibility private
    def worker_task_completed
      synchronize { @completed_task_count += 1 }
    end

    # @!macro thread_pool_executor_method_prune_pool
    def prune_pool
      deprecated "#prune_pool has no effect and will be removed in next the release, see https://github.com/ruby-concurrency/concurrent-ruby/pull/1082."
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
      @max_blocking_compensation =
        opts.fetch(:max_blocking_compensation, DEFAULT_MAX_BLOCKING_COMPENSATION).to_i

      raise ArgumentError.new("`synchronous` cannot be set unless `max_queue` is 0") if @synchronous && @max_queue > 0
      raise ArgumentError.new("#{@fallback_policy} is not a valid fallback policy") unless FALLBACK_POLICIES.include?(@fallback_policy)
      raise ArgumentError.new("`max_threads` cannot be less than #{DEFAULT_MIN_POOL_SIZE}") if @max_length < DEFAULT_MIN_POOL_SIZE
      raise ArgumentError.new("`max_threads` cannot be greater than #{DEFAULT_MAX_POOL_SIZE}") if @max_length > DEFAULT_MAX_POOL_SIZE
      raise ArgumentError.new("`min_threads` cannot be less than #{DEFAULT_MIN_POOL_SIZE}") if @min_length < DEFAULT_MIN_POOL_SIZE
      raise ArgumentError.new("`min_threads` cannot be more than `max_threads`") if min_length > max_length
      raise ArgumentError.new("`max_blocking_compensation` cannot be less than zero") if @max_blocking_compensation < DEFAULT_MAX_BLOCKING_COMPENSATION
      raise ArgumentError.new("`max_blocking_compensation` cannot be greater than #{DEFAULT_MAX_POOL_SIZE}") if @max_blocking_compensation > DEFAULT_MAX_POOL_SIZE

      @pool                 = [] # all workers
      @ready                = [] # idle workers: [worker, last_message]
      @queue                = [] # used as queue
      # @ready or @queue is empty at all times
      @scheduled_task_count = 0
      @completed_task_count = 0
      @largest_length       = 0
      @workers_counter      = 0
      @blocking_count       = 0 # workers currently inside a managed block
      @worker_threads       = {} # worker thread => Worker, for managed block checks
      @ruby_pid             = $$ # detects if Ruby has forked
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

      if @pool.empty?
        # nothing to do
        stopped_event.set
      end

      if @queue.empty?
        # no more tasks will be accepted, just stop all workers
        @pool.each(&:stop)
      end
    end

    # @!visibility private
    def ns_kill_execution
      # TODO log out unprocessed tasks in queue
      # TODO try to shutdown first?
      @pool.each(&:kill)
      @pool.clear
      @ready.clear
      @worker_threads.clear
      @blocking_count = 0
    end

    # tries to assign task to a worker, tries to get one from @ready or to create new one
    # @return [true, false] if task is assigned to a worker
    #
    # @!visibility private
    def ns_assign_worker(*args, &task)
      # take an idle worker if one exists, preferring ordinary workers over
      # idle compensation workers, otherwise grow the pool
      worker, = ns_pop_ready_worker if @pool.size >= @min_length
      worker ||= ns_add_busy_worker
      if worker
        worker << [task, args]
        true
      else
        false
      end
    rescue ThreadError
      # Raised when the operating system refuses to create the new thread
      return false
    end

    # Pops an idle worker from @ready, preferring an ordinary worker over an
    # idle compensation worker.
    #
    # @return [Array, nil] the popped `[worker, last_message]` entry
    #
    # @!visibility private
    def ns_pop_ready_worker
      index = @ready.index { |w, _| !w.compensating? } || @ready.index { |w, _| w.compensating? }
      index ? @ready.delete_at(index) : nil
    end

    # tries to enqueue task
    # @return [true, false] if enqueued
    #
    # @!visibility private
    def ns_enqueue(*args, &task)
      return false if @synchronous

      if !ns_limited_queue? || @queue.size < @max_queue
        @queue << [task, args]
        # An idle compensation worker cannot take already queued work on its
        # own; signal it for this newly submitted task.
        if entry = @ready.find { |w, _| w.compensating? }
          @ready.delete(entry)
          entry[0].signal_work
        end
        true
      else
        false
      end
    end

    # @!visibility private
    def ns_worker_died(worker)
      ns_remove_busy_worker worker
      replacement_worker = ns_add_busy_worker
      ns_ready_worker replacement_worker, Concurrent.monotonic_time, false if replacement_worker
    end

    # creates new worker which has to receive work to do after it's added
    # @return [nil, Worker] nil of max capacity is reached
    #
    # @!visibility private
    def ns_add_busy_worker
      return if @pool.size >= ns_effective_max_length

      @workers_counter += 1
      @pool << (worker = Worker.new(self, @workers_counter))
      @worker_threads[worker.thread] = worker
      @largest_length = @pool.length if @pool.length > @largest_length
      worker
    end

    # maximum number of workers which may exist right now. Workers parked in a
    # managed block temporarily surrender their execution slot, so the pool may
    # compensate with an additional worker, up to `max_blocking_compensation`.
    #
    # @!visibility private
    def ns_effective_max_length
      @max_length + [@blocking_count, @max_blocking_compensation].min
    end

    # Marks a worker as blocked inside a managed block and spawns a single
    # compensation worker (within the configured bounds) to preserve execution
    # capacity while the parked worker waits.
    #
    # @return [true, false] true if this worker is already blocking, i.e. the
    #   declaration was nested and must not change the accounting
    #
    # @!visibility private
    def ns_enter_managed_block(worker)
      return true if worker.blocking?
      worker.blocking = true
      @blocking_count += 1
      # Only compensate when there is no idle worker which could pick up the
      # work the blocked worker cannot do. The compensation worker starts
      # idle and only drains queued tasks once explicitly signaled for newly
      # submitted work, so it never steals tasks already waiting for an
      # ordinary worker.
      if @ready.empty?
        compensation = ns_add_busy_worker
        if compensation
          compensation.compensating = true
          if running?
            @ready.push([compensation, Concurrent.monotonic_time])
          else
            compensation.stop
          end
        end
      end
      false
    end

    # @!visibility private
    def ns_exit_managed_block(worker)
      return unless worker.blocking?
      worker.blocking = false
      @blocking_count -= 1
    end

    # handle ready worker, giving it new job or assigning back to @ready
    #
    # @!visibility private
    def ns_ready_worker(worker, last_message, success = true, work_signaled = false)
      # a compensation worker only drains the queue once it has been signaled
      # for newly submitted work, so it never steals tasks already pending for
      # an ordinary worker
      task_and_args = @queue.shift if !worker.compensating? || work_signaled
      if task_and_args
        worker << task_and_args
      else
        # stop workers when !running?, do not return them to @ready
        if running?
          raise unless last_message
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
      @worker_threads.delete(worker.thread)
      if worker.blocking?
        worker.blocking = false
        @blocking_count -= 1
      end
      stopped_event.set if @pool.empty? && !running?
      true
    end

    # @!visibility private
    def ns_remove_ready_worker(worker)
      if index = @ready.index { |rw, _| rw == worker }
        @ready.delete_at(index)
      end
      true
    end

    # @return [Integer] number of excess idle workers which can be removed without
    #                   going below min_length, or all workers if not running
    #
    # @!visibility private
    def ns_prunable_capacity
      if running?
        # while workers are parked in managed blocks their compensation
        # workers must be retained so the pool does not lose the capacity it
        # promised while the blocking tasks wait
        floor = [@min_length + @blocking_count, @pool.length].min
        [@pool.length - floor, @ready.size].min
      else
        @pool.size
      end
    end

    # @!visibility private
    def ns_reset_if_forked
      if $$ != @ruby_pid
        @queue.clear
        @ready.clear
        @pool.clear
        @scheduled_task_count = 0
        @completed_task_count = 0
        @largest_length       = 0
        @workers_counter      = 0
        @blocking_count       = 0
        @worker_threads.clear
        @ruby_pid             = $$
      end
    end

    # @!visibility private
    class Worker
      include Concern::Logging

      # the thread running this worker, accessed only under the pool's lock
      attr_reader :thread

      # whether the worker is currently parked inside a managed block,
      # accessed only under the pool's lock
      attr_accessor :blocking

      # whether this worker exists only to compensate for a blocked worker,
      # accessed only under the pool's lock
      attr_accessor :compensating

      def blocking?
        @blocking
      end

      def compensating?
        @compensating
      end

      def initialize(pool, id)
        # instance variables accessed only under pool's lock so no need to sync here again
        @queue        = Collection::TimeoutQueue.new
        @pool         = pool
        @blocking     = false
        @compensating = false
        @thread       = create_worker @queue, pool, pool.idletime

        if @thread.respond_to?(:name=)
          @thread.name = [pool.name, 'worker', id].compact.join('-')
        end
      end

      def <<(message)
        @queue << message
      end

      def stop
        @queue << :stop
      end

      # Wakes an idle compensation worker so it may take newly submitted work.
      def signal_work
        @queue << :work
      end

      def kill
        @thread.kill
      end

      private

      def create_worker(queue, pool, idletime)
        Thread.new(queue, pool, idletime) do |my_queue, my_pool, my_idletime|
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
              when :work
                # only an idle compensation worker receives this; it is now
                # allowed to take a queued task. If there is none (the task
                # was handled meanwhile) it simply stays parked.
                my_pool.ready_worker(self, Concurrent.monotonic_time, true, true)
              else
                task, args = message
                run_task my_pool, task, args
                my_pool.ready_worker(self, Concurrent.monotonic_time)
                prunable = true
              end
            end
          end
        end
      end

      def run_task(pool, task, args)
        task.call(*args)
        pool.worker_task_completed
      rescue => ex
        # let it fail
        log DEBUG, ex
      rescue Exception => ex
        log ERROR, ex
        pool.worker_died(self)
        throw :stop
      end
    end

    private_constant :Worker
  end
end
