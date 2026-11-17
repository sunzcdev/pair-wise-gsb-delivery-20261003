require 'concurrent/collection/copy_on_notify_observer_set'
require 'concurrent/concern/dereferenceable'
require 'concurrent/concern/observable'
require 'concurrent/concern/logging'
require 'concurrent/atomic/atomic_boolean'
require 'concurrent/atomic/atomic_fixnum'
require 'concurrent/executor/executor_service'
require 'concurrent/executor/ruby_executor_service'
require 'concurrent/executor/safe_task_executor'
require 'concurrent/scheduled_task'

module Concurrent

  # A very common concurrency pattern is to run a thread that performs a task at
  # regular intervals. The thread that performs the task sleeps for the given
  # interval then wakes up and performs the task. Lather, rinse, repeat... This
  # pattern causes two problems. First, it is difficult to test the business
  # logic of the task because the task itself is tightly coupled with the
  # concurrency logic. Second, an exception raised while performing the task can
  # cause the entire thread to abend. In a long-running application where the
  # task thread is intended to run for days/weeks/years a crashed task thread
  # can pose a significant problem. `TimerTask` alleviates both problems.
  #
  # When a `TimerTask` is launched it starts a thread for monitoring the
  # execution interval. The `TimerTask` thread does not perform the task,
  # however. Instead, the TimerTask launches the task on a separate thread.
  # Should the task experience an unrecoverable crash only the task thread will
  # crash. This makes the `TimerTask` very fault tolerant. Additionally, the
  # `TimerTask` thread can respond to the success or failure of the task,
  # performing logging or ancillary operations.
  #
  # One other advantage of `TimerTask` is that it forces the business logic to
  # be completely decoupled from the concurrency logic. The business logic can
  # be tested separately then passed to the `TimerTask` for scheduling and
  # running.
  #
  # A `TimerTask` supports two different types of interval calculations.
  # A fixed delay will always wait the same amount of time between the
  # completion of one task and the start of the next. A fixed rate will
  # attempt to maintain a constant rate of execution regardless of the
  # duration of the task. For example, if a fixed rate task is scheduled
  # to run every 60 seconds but the task itself takes 10 seconds to
  # complete, the next tick is still due 60 seconds after the previous
  # tick's phase. If the task takes 70 seconds to complete, the ticks that
  # became due while it was running are handled by the configured overlap
  # policy rather than being executed concurrently. Tasks are never
  # executed concurrently.
  #
  # == Overlapping executions
  #
  # When a tick becomes due while the previous execution is still running the
  # `TimerTask` needs an explicit policy for what to do. The policy never runs
  # two executions concurrently and it never queues an unbounded number of
  # ticks. The {#overlap_policy} option selects between:
  #
  # * {SKIP_OVERLAP} - ticks that fire while an execution is in progress are
  #   dropped. Each skipped tick is still recorded with its original identity,
  #   it simply never runs.
  # * {COALESCE_OVERLAP} (default) - all ticks that fire while an execution is
  #   in progress are merged into a single catch-up execution which runs as soon
  #   as the running execution finishes. The catch-up execution keeps the
  #   identity of the earliest coalesced tick; the other identities are recorded
  #   as coalesced.
  # * {QUEUE_OVERLAP} - ticks that fire while an execution is in progress are
  #   queued and run, one at a time, in their original order. The queue is
  #   bounded by {#max_queue}: ticks fired once the queue is full are skipped
  #   (and recorded) instead of accumulating without limit.
  #
  # Every scheduled tick has a stable, monotonically increasing identity which
  # is assigned when the tick is scheduled. Changing {#execution_interval} only
  # affects ticks scheduled afterwards, it never changes the identity of an
  # already scheduled tick. The mapping between scheduled ticks and the actual
  # executions (including skipped and coalesced ticks) is available through
  # {#tick_history} and {#statistics}.
  #
  # In some cases it may be necessary for a `TimerTask` to affect its own
  # execution cycle. To facilitate this, a reference to the TimerTask instance
  # is passed as an argument to the provided block every time the task is
  # executed.
  #
  # The `TimerTask` class includes the `Dereferenceable` mixin module so the
  # result of the last execution is always available via the `#value` method.
  # Dereferencing options can be passed to the `TimerTask` during construction or
  # at any later time using the `#set_deref_options` method.
  #
  # `TimerTask` supports notification through the Ruby standard library
  # {http://ruby-doc.org/stdlib-2.0/libdoc/observer/rdoc/Observable.html
  # Observable} module. On execution the `TimerTask` will notify the observers
  # with three arguments: time of execution, the result of the block (or nil on
  # failure), and any raised exceptions (or nil on success).
  #
  # @!macro copy_options
  #
  # @example Basic usage
  #   task = Concurrent::TimerTask.new{ puts 'Boom!' }
  #   task.execute
  #
  #   task.execution_interval #=> 60 (default)
  #
  #   # wait 60 seconds...
  #   #=> 'Boom!'
  #
  #   task.shutdown #=> true
  #
  # @example Configuring `:execution_interval`
  #   task = Concurrent::TimerTask.new(execution_interval: 5) do
  #          puts 'Boom!'
  #        end
  #
  #   task.execution_interval #=> 5
  #
  # @example Immediate execution with `:run_now`
  #   task = Concurrent::TimerTask.new(run_now: true){ puts 'Boom!' }
  #   task.execute
  #
  #   #=> 'Boom!'
  #
  # @example Configuring `:interval_type` with either :fixed_delay or :fixed_rate, default is :fixed_delay
  #   task = Concurrent::TimerTask.new(execution_interval: 5, interval_type: :fixed_rate) do
  #          puts 'Boom!'
  #        end
  #   task.interval_type #=> :fixed_rate
  #
  # @example Configuring the overlap policy
  #   task = Concurrent::TimerTask.new(execution_interval: 1,
  #                                   overlap_policy: :queue,
  #                                   max_queue: 4) do
  #          puts 'Boom!'
  #        end
  #
  # @example Last `#value` and `Dereferenceable` mixin
  #   task = Concurrent::TimerTask.new(
  #     dup_on_deref: true,
  #     execution_interval: 5
  #   ){ Time.now }
  #
  #   task.execute
  #   Time.now   #=> 2013-11-07 18:06:50 -0500
  #   sleep(10)
  #   task.value #=> 2013-11-07 18:06:55 -0500
  #
  # @example Controlling execution from within the block
  #   timer_task = Concurrent::TimerTask.new(execution_interval: 1) do |task|
  #     task.execution_interval.to_i.times{ print 'Boom! ' }
  #     print "\n"
  #     task.execution_interval += 1
  #     if task.execution_interval > 5
  #       puts 'Stopping...'
  #       task.shutdown
  #     end
  #   end
  #
  #   timer_task.execute
  #   #=> Boom!
  #   #=> Boom! Boom!
  #   #=> Boom! Boom! Boom!
  #   #=> Boom! Boom! Boom! Boom!
  #   #=> Boom! Boom! Boom! Boom! Boom!
  #   #=> Stopping...
  #
  # @example Observation
  #   class TaskObserver
  #     def update(time, result, ex)
  #       if result
  #         print "(#{time}) Execution successfully returned #{result}\n"
  #       else
  #         print "(#{time}) Execution failed with error #{ex}\n"
  #       end
  #     end
  #   end
  #
  #   task = Concurrent::TimerTask.new(execution_interval: 1){ 42 }
  #   task.add_observer(TaskObserver.new)
  #   task.execute
  #   sleep 4
  #
  #   #=> (2013-10-13 19:08:58 -0400) Execution successfully returned 42
  #   #=> (2013-10-13 19:09:00 -0400) Execution successfully returned 42
  #   #=> (2013-10-13 19:09:00 -0400) Execution successfully returned 42
  #   task.shutdown
  #
  # @see http://ruby-doc.org/stdlib-2.0/libdoc/observer/rdoc/Observable.html
  # @see http://docs.oracle.com/javase/7/docs/api/java/util/TimerTask.html
  class TimerTask < RubyExecutorService
    include Concern::Dereferenceable
    include Concern::Observable
    include Concern::Logging

    # Default `:execution_interval` in seconds.
    EXECUTION_INTERVAL = 60

    # Maintain the interval between the end of one execution and the start of the next execution.
    FIXED_DELAY = :fixed_delay

    # Maintain the interval between the start of one execution and the start of the next.
    # Ticks keep a constant phase regardless of how long an execution takes; ticks due
    # while an execution is running are handled by the configured overlap policy.
    # Executions will not run concurrently.
    FIXED_RATE = :fixed_rate

    # Default `:interval_type`
    DEFAULT_INTERVAL_TYPE = FIXED_DELAY

    # Overlap policy: a tick which fires while an execution is still running is
    # dropped. The tick keeps its identity and is recorded as skipped.
    SKIP_OVERLAP = :skip

    # Overlap policy: ticks which fire while an execution is still running are
    # merged into a single catch-up execution carrying the earliest tick
    # identity.
    COALESCE_OVERLAP = :coalesce

    # Overlap policy: ticks which fire while an execution is still running are
    # queued (up to {#max_queue}) and executed one at a time in order.
    QUEUE_OVERLAP = :queue

    # All supported overlap policies.
    OVERLAP_POLICIES = [SKIP_OVERLAP, COALESCE_OVERLAP, QUEUE_OVERLAP].freeze

    # Default `:overlap_policy`
    DEFAULT_OVERLAP_POLICY = COALESCE_OVERLAP

    # Default bound on the number of ticks queued by the {QUEUE_OVERLAP} policy.
    DEFAULT_MAX_QUEUE = 10

    # Create a new TimerTask with the given task and configuration.
    #
    # @!macro timer_task_initialize
    #   @param [Hash] opts the options defining task execution.
    #   @option opts [Float] :execution_interval number of seconds between
    #     task executions (default: EXECUTION_INTERVAL)
    #   @option opts [Boolean] :run_now Whether to run the task immediately
    #     upon instantiation or to wait until the first #  execution_interval
    #     has passed (default: false)
    #   @options opts [Symbol] :interval_type method to calculate the interval
    #     between executions, can be either :fixed_rate or :fixed_delay.
    #     (default: :fixed_delay)
    #   @option opts [Symbol] :overlap_policy what to do when a tick fires while
    #     the previous execution is still running. One of :skip, :coalesce or
    #     :queue. Executions never overlap and the queue never grows without
    #     bound. (default: :coalesce)
    #   @option opts [Integer] :max_queue maximum number of due ticks kept
    #     waiting by the :queue overlap policy. Ticks fired once the queue is
    #     full are skipped. Must be greater than zero. (default: 10)
    #   @option opts [Executor] executor, default is `global_io_executor`
    #   @option opts [Object] :scheduler injection point for the scheduling
    #     clock. The object must respond to `schedule(delay, &block)` and return
    #     a handle responding to `#cancel`. Intended for deterministic testing,
    #     production code should leave this unset.
    #
    #   @!macro deref_options
    #
    #   @raise ArgumentError when no block is given.
    #
    #   @yield to the block after :execution_interval seconds have passed since
    #     the last yield
    #   @yieldparam task a reference to the `TimerTask` instance so that the
    #     block can control its own lifecycle. Necessary since `self` will
    #     refer to the execution context of the block rather than the running
    #     `TimerTask`.
    #
    #   @return [TimerTask] the new `TimerTask`
    def initialize(opts = {}, &task)
      raise ArgumentError.new('no block given') unless block_given?
      super
      set_deref_options opts
    end

    # Is the executor running?
    #
    # @return [Boolean] `true` when running, `false` when shutting down or shutdown
    def running?
      @running.true?
    end

    # Execute a previously created `TimerTask`.
    #
    # @return [TimerTask] a reference to `self`
    #
    # @example Instance and execute in separate steps
    #   task = Concurrent::TimerTask.new(execution_interval: 10){ print "Hello World\n" }
    #   task.running? #=> false
    #   task.execute
    #   task.running? #=> true
    #
    # @example Instance and execute in one line
    #   task = Concurrent::TimerTask.new(execution_interval: 10){ print "Hello World\n" }.execute
    #   task.running? #=> true
    def execute
      synchronize do
        if @running.false?
          @running.make_true
          @generation.increment
          ns_schedule_first_tick
        end
      end
      self
    end

    # Create and execute a new `TimerTask`.
    #
    # @!macro timer_task_initialize
    #
    # @example
    #   task = Concurrent::TimerTask.execute(execution_interval: 10){ print "Hello World\n" }
    #   task.running? #=> true
    def self.execute(opts = {}, &task)
      TimerTask.new(opts, &task).execute
    end

    # @!attribute [rw] execution_interval
    # @return [Float] Number of seconds after the task completes before the
    #   task is performed again. Changing the interval only affects ticks that
    #   are scheduled afterwards; identities of already scheduled ticks are
    #   unaffected.
    def execution_interval
      synchronize { @execution_interval }
    end

    # @!attribute [rw] execution_interval
    # @return [Float] Number of seconds after the task completes before the
    #   task is performed again.
    def execution_interval=(value)
      if (value = value.to_f) <= 0.0
        raise ArgumentError.new('must be greater than zero')
      else
        synchronize { @execution_interval = value }
      end
    end

    # @!attribute [r] interval_type
    # @return [Symbol] method to calculate the interval between executions
    attr_reader :interval_type

    # @!attribute [r] overlap_policy
    # @return [Symbol] policy applied when a tick fires while an execution is
    #   still running, one of :skip, :coalesce or :queue.
    attr_reader :overlap_policy

    # @!attribute [r] max_queue
    # @return [Integer] bound on the number of due ticks kept waiting by the
    #   :queue overlap policy.
    attr_reader :max_queue

    # @!attribute [rw] timeout_interval
    # @return [Fixnum] Number of seconds the task can run before it is
    #   considered to have failed.
    def timeout_interval
      warn 'TimerTask timeouts are now ignored as these were not able to be implemented correctly'
    end

    # @!attribute [rw] timeout_interval
    # @return [Fixnum] Number of seconds the task can run before it is
    #   considered to have failed.
    def timeout_interval=(value)
      warn 'TimerTask timeouts are now ignored as these were not able to be implemented correctly'
    end

    # The identity of the most recently scheduled tick. Tick identities start
    # at zero and increase by one for every scheduled tick.
    #
    # @return [Integer, nil] the latest tick identity or nil before the first
    #   tick is scheduled
    def last_tick
      synchronize { @last_tick }
    end

    # The identity of the tick currently being executed, if any.
    #
    # @return [Integer, nil] the running tick identity or nil when idle
    def running_tick
      synchronize { @running_tick }
    end

    # Ticks which have fired while an execution is in progress and are waiting
    # to run. Under the :coalesce policy the array contains at most one element
    # (the earliest coalesced tick), under :queue it is bounded by
    # {#max_queue}, under :skip it is always empty.
    #
    # @return [Array<Integer>] the waiting tick identities, oldest first
    def pending_ticks
      synchronize { @pending_ticks.dup }
    end

    # The recorded mapping between every scheduled tick and what happened to
    # it. Each entry is a hash with:
    #
    # * `:tick` (Integer) - the stable tick identity,
    # * `:scheduled_at` (Float) - monotonic time when the tick was scheduled,
    # * `:due_at` (Float) - monotonic time the tick was due,
    # * `:status` (Symbol) - one of `:ran`, `:skipped`, `:coalesced`,
    #   `:cancelled`,
    # * `:ran_as` (Integer, nil) - for a coalesced tick, the identity of the
    #   execution which absorbed it,
    # * `:started_at` (Float, nil) - monotonic time the execution started,
    # * `:finished_at` (Float, nil) - monotonic time the execution finished.
    #
    # The history is capped so that a long running task does not accumulate
    # records without limit.
    #
    # @return [Array<Hash>] a copy of the tick history, oldest first
    def tick_history
      synchronize { @tick_history.dup }
    end

    # Aggregated counters describing the mapping between scheduled ticks and
    # actual executions.
    #
    # @return [Hash{Symbol => Integer}] counts of ticks `:scheduled`, `:ran`,
    #   `:skipped`, `:coalesced` and `:cancelled`
    def statistics
      synchronize { @statistics.dup }
    end

    private :post, :<<

    private

    # Maximum number of tick history entries retained by a running task.
    HISTORY_LIMIT = 1000

    def ns_initialize(opts, &task)
      set_deref_options(opts)

      self.execution_interval = opts[:execution] || opts[:execution_interval] || EXECUTION_INTERVAL
      if opts[:interval_type] && ![FIXED_DELAY, FIXED_RATE].include?(opts[:interval_type])
        raise ArgumentError.new('interval_type must be either :fixed_delay or :fixed_rate')
      end
      @overlap_policy = opts[:overlap_policy] || DEFAULT_OVERLAP_POLICY
      unless OVERLAP_POLICIES.include?(@overlap_policy)
        raise ArgumentError.new("overlap_policy must be one of #{OVERLAP_POLICIES.inspect}")
      end
      @max_queue = (opts[:max_queue] || DEFAULT_MAX_QUEUE).to_i
      raise ArgumentError.new('max_queue must be greater than zero') if @max_queue <= 0
      if opts[:timeout] || opts[:timeout_interval]
        warn 'TimeTask timeouts are now ignored as these were not able to be implemented correctly'
      end

      @run_now = opts[:now] || opts[:run_now]
      @interval_type = opts[:interval_type] || DEFAULT_INTERVAL_TYPE
      @task = Concurrent::SafeTaskExecutor.new(task)
      @executor = opts[:executor] || Concurrent.global_io_executor
      @scheduler = opts[:scheduler] || TimerTask::SystemScheduler.new(@executor)
      @running = Concurrent::AtomicBoolean.new(false)
      # Bumped on every (re)start; handles scheduled from a previous life are
      # rejected so a restarted task can never run a stale tick twice.
      @generation = Concurrent::AtomicFixnum.new(0)

      @scheduled_handle = nil
      @armed_tick = nil
      @last_tick = nil
      @running_tick = nil
      @pending_ticks = []
      @tick_history = []
      @statistics = { scheduled: 0, ran: 0, skipped: 0, coalesced: 0, cancelled: 0 }
      @value = nil

      self.observers = Collection::CopyOnNotifyObserverSet.new
    end

    # @!visibility private
    def ns_shutdown_execution
      ns_stop_accepting_ticks
      super
    end

    # @!visibility private
    def ns_kill_execution
      ns_stop_accepting_ticks
      super
    end

    # Stop accepting new ticks and cancel the recurring schedule.
    # Executions already running are left to finish on their own; when they
    # eventually complete they are still recorded and observed as having run,
    # they simply never start any further work.
    #
    # @!visibility private
    def ns_stop_accepting_ticks
      @running.make_false
      handle = @scheduled_handle
      @scheduled_handle = nil
      @armed_tick = nil
      handle.cancel if handle
      # Ticks which never reached an executor are cancelled. The tick that is
      # currently running keeps its pending status and is finalized by its
      # own completion callback; queued catch-up ticks never start, so they
      # are cancelled here.
      @pending_ticks.each do |tick|
        entry = ns_history_entry(tick)
        ns_mark_final(entry, :cancelled)
      end
      @pending_ticks.clear
      @tick_history.each do |record|
        ns_mark_final(record, :cancelled) if record[:status] == :scheduled && record[:tick] != @running_tick
      end
    end

    # Start the tick stream for a newly (re)started task. The first tick fires
    # immediately when `:run_now` was requested, otherwise after one interval.
    #
    # A fixed-rate task arms a periodic stream so ticks keep a constant phase
    # and a long execution simply produces overlapping (policy-managed) ticks.
    # A fixed-delay task arms a single shot; the following tick is not armed
    # until the current execution has finished, so the gap between completion
    # and the next start is always exactly one interval.
    #
    # The first tick is allocated an identity and a history record here, before
    # it becomes due, so a task shut down ahead of its first firing still has a
    # complete, attributable record rather than an untracked cancellation.
    #
    # @!visibility private
    def ns_schedule_first_tick
      initial_delay = @run_now ? 0 : @execution_interval
      generation = @generation.value
      first_tick, = ns_allocate_tick(initial_delay)
      @armed_tick = first_tick
      if @interval_type == FIXED_RATE
        @scheduled_handle = @scheduler.schedule(initial_delay, period: @execution_interval) do
          handle_tick_fired(generation)
        end
      else
        @scheduled_handle = @scheduler.schedule(initial_delay) do
          handle_tick_fired(generation)
        end
      end
    end

    # Called by the scheduler every time a tick stream reaches a due instant.
    # Runs on the scheduler's own thread, so all bookkeeping happens under the
    # task lock and the (possibly slow) user task is dispatched to the
    # configured executor.
    #
    # The first firing of a lifecycle consumes the tick pre-allocated while
    # arming. Later firings of a periodic fixed-rate stream allocate their tick
    # on the spot; a fixed-delay stream always pre-arms its single next tick in
    # {#ns_arm_fixed_delay_tick}. All allocation happens under this lock, so
    # identities stay strictly monotonic even when several periods elapse within
    # one scheduler wake-up.
    #
    # @!visibility private
    def handle_tick_fired(generation_when_scheduled)
      synchronize do
        # Reject ticks from a previous lifecycle (the task was shut down and
        # restarted) and ticks arriving after shutdown.
        return unless @running.true? && generation_when_scheduled == @generation.value

        if @armed_tick
          tick = @armed_tick
          @armed_tick = nil
          entry = ns_history_entry(tick)
        else
          tick, entry = ns_allocate_tick(0)
        end

        if @running_tick.nil? && @pending_ticks.empty?
          ns_dispatch(tick, entry)
        else
          ns_defer_due_tick(tick, entry)
        end
      end
      nil
    rescue => ex
      # A failure in the scheduling machinery must never silently kill the
      # scheduler thread and thereby freeze the task.
      log(ERROR, self.class.name) { "scheduling a tick failed: #{ex.class}: #{ex.message}" }
      nil
    end

    # Allocate a tick identity together with its history record. `delay` is the
    # time from now until the tick is due. Identities are assigned once, when a
    # tick is scheduled, so changing {#execution_interval} never rewrites an
    # already assigned identity.
    #
    # @!visibility private
    def ns_allocate_tick(delay)
      tick = @last_tick.nil? ? 0 : @last_tick + 1
      now = Concurrent.monotonic_time
      @last_tick = tick
      @tick_history << {
        tick: tick,
        scheduled_at: now,
        due_at: now + delay,
        status: :scheduled,
        ran_as: nil,
        started_at: nil,
        finished_at: nil
      }
      ns_trim_history
      @statistics[:scheduled] += 1
      [tick, @tick_history.last]
    end

    # @!visibility private
    def ns_trim_history
      excess = @tick_history.size - HISTORY_LIMIT
      @tick_history.shift(excess) if excess > 0
    end

    # @!visibility private
    def ns_history_entry(tick)
      @tick_history.find { |record| record[:tick] == tick }
    end

    # Transition a still-pending history entry to a terminal status exactly
    # once, keeping the aggregated counters consistent.
    #
    # @!visibility private
    def ns_mark_final(entry, status)
      return unless entry && entry[:status] == :scheduled
      entry[:status] = status
      @statistics[status] += 1 if @statistics.key?(status)
    end

    # Apply the configured overlap policy to a tick that fired while another
    # execution (or queued catch-up work) is pending.
    #
    # @!visibility private
    def ns_defer_due_tick(tick, entry)
      case @overlap_policy
      when SKIP_OVERLAP
        ns_mark_final(entry, :skipped)
      when COALESCE_OVERLAP
        if @pending_ticks.empty?
          # First overlapping tick becomes the single catch-up execution.
          @pending_ticks << tick
        else
          # Further overlapping ticks are absorbed by the pending catch-up;
          # they keep their own identity and point at the execution that
          # actually runs on their behalf.
          entry[:ran_as] = @pending_ticks.first
          ns_mark_final(entry, :coalesced)
        end
      when QUEUE_OVERLAP
        if @pending_ticks.size < @max_queue
          @pending_ticks << tick
        else
          # The bounded queue is full: drop the tick rather than accumulate
          # work without limit.
          ns_mark_final(entry, :skipped)
        end
      end
    end

    # Dispatch tick `tick` to the executor and mark it as the running tick.
    #
    # @!visibility private
    def ns_dispatch(tick, entry)
      @running_tick = tick
      generation = @generation.value
      begin
        @executor.post { run_execution(tick, generation) }
      rescue RejectedExecutionError => ex
        # The executor rejected the work: record it, release the running slot
        # and keep the recurring stream alive instead of freezing forever.
        @running_tick = nil
        ns_mark_final(entry, :skipped)
        log(WARN, self.class.name) { "tick #{tick.inspect} rejected by executor: #{ex.class}: #{ex.message}" }
      end
    end

    # Executes the user block for tick `tick` on the configured executor.
    #
    # @!visibility private
    def run_execution(tick, generation_when_dispatched)
      start_time = Concurrent.monotonic_time
      _success, value, reason = @task.execute(self)
      finish_time = Concurrent.monotonic_time

      synchronize do
        # The task may have been shut down (or restarted) while the block was
        # running. Its work is already done, so it is still recorded and
        # observed, but it starts no further executions.
        lifecycle_current = @running.true? && generation_when_dispatched == @generation.value

        entry = ns_history_entry(tick)
        if entry
          entry[:started_at] = start_time
          entry[:finished_at] = finish_time
          ns_mark_final(entry, :ran)
        end

        if lifecycle_current
          self.value = value
          observers.notify_observers do
            [Time.now, self.value, reason]
          end
          ns_after_execution
        end
      end
      nil
    rescue => ex
      # Safety net: SafeTaskExecutor already captures block failures, so an
      # exception here means the surrounding machinery failed. Swallowing it
      # here prevents the executor thread from dying without a trace.
      log(ERROR, self.class.name) { "tick #{tick.inspect} execution failed: #{ex.class}: #{ex.message}" }
      nil
    end

    # Bookkeeping once an execution has finished while the task is still alive.
    #
    # For a fixed-rate task the periodic tick stream keeps producing ticks on
    # its own; overlapping ticks deferred by the overlap policy are started
    # here. For a fixed-delay task there is no periodic stream, so any queued
    # catch-up work is started first and, once the queue is empty, the next
    # single-shot tick is armed measured from completion, which guarantees the
    # fixed gap between an execution end and the next start.
    #
    # @!visibility private
    def ns_after_execution
      @running_tick = nil

      if @pending_ticks.any?
        catch_up_tick = @pending_ticks.shift
        catch_up_entry = ns_history_entry(catch_up_tick)
        # The catch-up execution routes back through this method when it
        # finishes, eventually draining the whole queue before re-arming.
        ns_dispatch(catch_up_tick, catch_up_entry)
      elsif @interval_type == FIXED_DELAY
        ns_arm_fixed_delay_tick
      end
    end

    # Arm the next single-shot tick for a fixed-delay task. Called once the
    # previously dispatched work (including any queued catch-up) has fully
    # drained. The tick is allocated its identity up front so that a shutdown
    # before its due time still leaves an attributable, cancelled record.
    #
    # @!visibility private
    def ns_arm_fixed_delay_tick
      generation = @generation.value
      tick, = ns_allocate_tick(@execution_interval)
      @armed_tick = tick
      @scheduled_handle = @scheduler.schedule(@execution_interval) do
        handle_tick_fired(generation)
      end
    end

    # Default scheduling clock, backed by {ScheduledTask}. Indirection through
    # this tiny object lets tests inject a deterministic virtual clock instead
    # of sleeping through real intervals.
    #
    # A periodic stream is a self-perpetuating chain of scheduled tasks: the
    # next link is created from inside the firing task, so a slow firing can
    # never produce overlapping timer threads. All links run on the task's
    # executor, preserving the historical behaviour of keeping every
    # task-related posting on one executor.
    #
    # @!visibility private
    class SystemScheduler
      def initialize(executor)
        @executor = executor
      end

      # Schedule `block` to run after `delay` seconds and, when `period` is
      # given, repeatedly every `period` seconds afterwards. The returned
      # handle cancels the one-shot or the whole periodic stream.
      #
      # @return [Handle]
      def schedule(delay, period: nil, &block)
        handle = Handle.new
        arm(handle, delay, period, block)
        handle
      end

      private

      def arm(handle, delay, period, block)
        scheduled = ScheduledTask.execute(delay, executor: @executor) do
          block.call
          # Re-arm only after the firing has fully returned. A cancelled
          # handle stops the chain.
          arm(handle, period, period, block) if period && handle.alive?
        end
        handle.bind(scheduled)
      end

      # Cancellation handle for a (possibly recurring) scheduled stream.
      #
      # @!visibility private
      class Handle
        def initialize
          @mutex = Mutex.new
          @scheduled_task = nil
          @cancelled = false
        end

        # Associate the currently armed scheduled task. If the handle was
        # cancelled before binding (a shutdown racing the very first arm), the
        # freshly created task is cancelled immediately.
        def bind(scheduled_task)
          @mutex.synchronize do
            @scheduled_task = scheduled_task
            scheduled_task.cancel if @cancelled
          end
        end

        def cancel
          @mutex.synchronize do
            @cancelled = true
            @scheduled_task&.cancel
          end
        end

        def alive?
          @mutex.synchronize { !@cancelled }
        end
      end
    end
  end
end
