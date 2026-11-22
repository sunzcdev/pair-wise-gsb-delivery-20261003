# frozen_string_literal: true

require 'thread'

require_relative 'io_buffer'
require_relative 'server_plugin_control'

module Puma

  Thread.attr_accessor(:puma_server)

  # Internal Docs for A simple thread pool management object.
  #
  # Each Puma "worker" has a thread pool to process requests.
  #
  # First a connection to a client is made in `Puma::Server`. It is wrapped in a
  # `Puma::Client` instance and then passed to the `Puma::Reactor` to ensure
  # the whole request is buffered into memory. Once the request is ready, it is passed into
  # a thread pool via the `Puma::ThreadPool#<<` operator where it is stored in a `@todo` array.
  #
  # Each thread in the pool has an internal loop where it pulls a request from the `@todo` array
  # and processes it.
  class ThreadPool
    class ForceShutdown < RuntimeError
    end

    class ProcessorThread
      attr_accessor :thread
      attr_writer :marked_as_io_thread

      def initialize(pool)
        @pool = pool
        @thread = nil
        @marked_as_io_thread = false
      end

      def mark_as_io_thread!
        unless @marked_as_io_thread
          @marked_as_io_thread = true

          # Immediately signal the pool that it can spawn a new thread
          # if there's some work in the queue.
          @pool.spawn_thread_if_needed
        end
      end

      def marked_as_io_thread?
        @marked_as_io_thread
      end

      def alive?
        @thread&.alive?
      end

      def join(...)
        @thread.join(...)
      end

      def kill(...)
        @thread.kill(...)
      end

      def [](key)
        @thread[key]
      end

      def raise(...)
        @thread.raise(...)
      end
    end

    # How long, after raising the ForceShutdown of a thread during
    # forced shutdown mode, to wait for the thread to try and finish
    # up its work before leaving the thread to die on the vine.
    SHUTDOWN_GRACE_TIME = 5 # seconds

    # Sentinel used to tell "the caller did not pass +queue_timeout+" from
    # "the caller passed +queue_timeout: nil+ to disable it".
    NOT_GIVEN = Object.new.freeze
    private_constant :NOT_GIVEN

    # An item of work together with the time at which it became able to run and
    # the time by which a processor thread must have taken it.
    #
    # Both times are `Process::CLOCK_MONOTONIC` readings. The clock therefore
    # measures only the time the work waited for a thread in this pool. For a
    # request it starts when the request is complete and could be given to the
    # Rack app, and it stops when a processor thread takes the request, so the
    # time spent reading the request from the client is not counted.
    #
    # Work that is added before it can run, such as a connection whose request
    # has not been read yet, has no `ready_at` and no deadline: its clock has
    # not started, so it can never expire.
    #
    # Only used when the pool has a queue timeout (`queue_timeout`).
    class QueuedWork # :nodoc:
      attr_reader :work, :ready_at, :deadline

      def initialize(work, ready_at = nil, timeout = nil)
        @work = work
        @ready_at = ready_at
        @deadline = ready_at && timeout ? ready_at + timeout : nil
      end

      # Whether a processor thread may no longer take this work. The processor
      # thread that takes work and the timer that drops it both decide while
      # holding the pool mutex, so at the deadline exactly one of them applies:
      # the work is either run by a processor thread or handed to the queue
      # timeout handler, never both and never neither.
      def expired?(now)
        deadline = @deadline
        !deadline.nil? && now >= deadline
      end

      # Seconds this work waited in the queue, or nil when it could not run
      # when it was queued.
      def waited(now)
        ready_at = @ready_at
        ready_at && (now - ready_at)
      end
    end

    attr_reader :out_of_band_running
    # The number of seconds work may wait for a thread, or nil when there is
    # no limit.
    attr_reader :queue_timeout

    # Maintain a minimum of +min+ and maximum of +max+ threads
    # in the pool.
    #
    # The block passed is the work that will be performed in each
    # thread.
    #
    # When +queue_timeout+ is set, work added with #<< that is still waiting
    # for a thread after that many seconds is removed from the queue and passed
    # to +queue_timeout_handler+ with the time it waited instead of being run.
    # The clock starts when the work is added, which for a request is the
    # moment it is complete and could be handed to the Rack app.
    #
    # +options[:queue_timeout]+ is used when +queue_timeout+ is not given, so
    # the value from the configuration is applied by default. Passing
    # `queue_timeout: nil` explicitly disables it.
    #
    def initialize(name, options = {}, server: nil, queue_timeout_handler: nil,
        queue_timeout: NOT_GIVEN, &block)
      @server = server

      @not_empty = ConditionVariable.new
      @not_full = ConditionVariable.new
      @mutex = Mutex.new

      @queue_timeout = queue_timeout.equal?(NOT_GIVEN) ? options[:queue_timeout] : queue_timeout
      if @queue_timeout
        @queue_timeout = Float(@queue_timeout)
        unless @queue_timeout.finite? && @queue_timeout > 0
          raise ArgumentError, "queue_timeout must be a finite number of seconds greater than 0, got #{@queue_timeout}"
        end
        unless queue_timeout_handler
          raise ArgumentError, "queue_timeout requires a queue_timeout_handler"
        end
      end
      @queue_timeout_handler = queue_timeout_handler
      # Expiring work needs it to be found in the queue by deadline, so the
      # queue is an Array in that case. Without a queue timeout nothing is
      # inspected and the cheaper Queue is kept.
      @todo = @queue_timeout ? [] : Queue.new

      @backlog_max = 0
      @spawned = 0
      @waiting = 0

      @name = name
      @min = Integer(options[:min_threads])
      @max = Integer(options[:max_threads])
      @max_io_threads = Integer(options[:max_io_threads] || 0)

      # Not an 'exposed' option, options[:pool_shutdown_grace_time] is used in CI
      # to shorten @shutdown_grace_time from SHUTDOWN_GRACE_TIME. Parallel CI
      # makes stubbing constants difficult.
      @shutdown_grace_time = Float(options[:pool_shutdown_grace_time] || SHUTDOWN_GRACE_TIME)
      @shutdown_debug = options[:shutdown_debug]
      @block = block
      @out_of_band = options[:out_of_band]
      @out_of_band_running = false
      @out_of_band_condvar = ConditionVariable.new
      @before_thread_start = options[:before_thread_start]
      @before_thread_exit = options[:before_thread_exit]
      @reaping_time = options[:reaping_time]
      @auto_trim_time = options[:auto_trim_time]

      @shutdown = false

      @trim_requested = 0
      @out_of_band_pending = false

      @processors = []

      @auto_trim = nil
      @reaper = nil

      # Wakes the queue timeout timer when the earliest deadline may have
      # moved, and holds the deadline it is currently waiting for so #add_work
      # can skip waking it needlessly.
      @queue_timeout_cond = ConditionVariable.new
      @queue_timeout_next = nil
      @queue_timeout_thread = nil

      @mutex.synchronize do
        @min.times do
          spawn_thread
          @not_full.wait(@mutex)
        end
      end

      start_queue_timeout_timer if @queue_timeout

      @force_shutdown = false
      @shutdown_mutex = Mutex.new
    end

    attr_reader :spawned, :trim_requested, :waiting
    attr_accessor :min, :max

    # generate stats hash so as not to perform multiple locks
    # @return [Hash] hash containing stat info from ThreadPool
    def stats
      with_mutex do
        temp = @backlog_max
        @backlog_max = 0
        { backlog: @todo.size,
          running: @spawned,
          pool_capacity: pool_capacity,
          busy_threads: @spawned - @waiting + @todo.size,
          io_threads: @processors.count(&:marked_as_io_thread?),
          backlog_max: temp
        }
      end
    end

    def reset_max
      with_mutex { @backlog_max = 0 }
    end

    # How many objects have yet to be processed by the pool?
    #
    def backlog
      with_mutex { @todo.size }
    end

    # The maximum size of the backlog
    #
    def backlog_max
      with_mutex { @backlog_max }
    end

    # @!attribute [r] pool_capacity
    def pool_capacity
      (waiting + (@max - spawned)).clamp(0, Float::INFINITY)
    end

    # @!attribute [r] busy_threads
    # @version 5.0.0
    def busy_threads
      with_mutex { @spawned - @waiting + @todo.size }
    end

    # :nodoc:
    #
    # Must be called with @mutex held!
    #
    def spawn_thread
      @spawned += 1

      trigger_before_thread_start_hooks
      processor = ProcessorThread.new(self)
      processor.thread = Thread.new(processor, @spawned) do |processor, spawned|
        Puma.set_thread_name '%s tp %03i' % [@name, spawned]
        # Advertise server into the thread
        Thread.current.puma_server = @server

        todo  = @todo
        block = @block
        mutex = @mutex
        not_empty = @not_empty
        not_full = @not_full

        while true
          work = nil
          expired = nil

          mutex.synchronize do
            if processor.marked_as_io_thread?
              if @processors.count { |t| !t.marked_as_io_thread? } < @max
                # We're not at max processor threads, so the io thread can rejoin the normal population.
                processor.marked_as_io_thread = false
              else
                # We're already at max threads, so we exit the extra io thread.
                @spawned -= 1
                @processors.delete(processor)
                trigger_before_thread_exit_hooks
                Thread.exit
              end
            end

            while todo.empty?
              if @trim_requested > 0
                @trim_requested -= 1
                @spawned -= 1
                @processors.delete(processor)
                not_full.signal
                trigger_before_thread_exit_hooks
                Thread.exit
              end

              @waiting += 1
              if @out_of_band_pending && trigger_out_of_band_hook
                @out_of_band_pending = false
              end
              not_full.signal
              begin
                not_empty.wait mutex
              ensure
                @waiting -= 1
              end
            end

            if @queue_timeout
              # Taking work and expiring work both happen under this mutex, so
              # an item whose deadline has just passed is decided exactly once,
              # here or by the queue timeout timer.
              #
              # Shutdown drains the queue instead of expiring it, matching the
              # pool without a queue timeout: work still waiting is handed to
              # a processor thread.
              expired = @shutdown ? [] : take_expired_locked
              if expired.empty?
                expired = nil
                work = take_work_locked
              else
                not_full.broadcast
              end
            else
              work = todo.shift
            end
          end

          if expired
            # Work that passed its deadline is never handed to the block. The
            # handler runs outside the mutex, then this thread looks for its
            # next item of work.
            notify_expired expired
            next
          end

          begin
            @out_of_band_pending = true if block.call(processor, work)
          rescue Exception => e
            STDERR.puts "Error reached top of thread-pool: #{e.message} (#{e.class})"
          end
        end
      end

      @processors << processor

      processor
    end

    private :spawn_thread

    # :nodoc:
    #
    # Must be called with @mutex held!
    #
    # Removes and returns the next item of work in queue order, unwrapped.
    #
    def take_work_locked
      item = @todo.shift
      item.is_a?(QueuedWork) ? item.work : item
    end

    private :take_work_locked

    # :nodoc:
    #
    # Must be called with @mutex held!
    #
    # Removes every queued item whose deadline has passed and returns them in
    # queue order. Items that were queued before they could run, such as a
    # connection whose request has not been read yet, have no deadline and are
    # never removed.
    #
    # Ready items are enqueued with the same timeout, so their deadlines are in
    # queue order. The scan can therefore stop at the first ready item that has
    # not expired: no later ready item can have expired either.
    #
    # @param now [Float] a `Process::CLOCK_MONOTONIC` reading
    # @return [Array<QueuedWork>]
    #
    def take_expired_locked(now = Process.clock_gettime(Process::CLOCK_MONOTONIC))
      expired = []

      index = 0
      while index < @todo.size
        item = @todo[index]
        unless item.is_a?(QueuedWork) && item.deadline
          # Not ready when it was queued, so it has no deadline.
          index += 1
          next
        end

        break unless item.expired?(now)

        expired << item
        @todo.delete_at index
      end

      expired
    end

    private :take_expired_locked

    # :nodoc:
    #
    # Must be called with @mutex held!
    #
    # The earliest deadline of any queued item, or +nil+ when nothing queued
    # can expire. +nil+ means the queue timeout timer can sleep until new work
    # wakes it.
    #
    def next_deadline_locked
      @todo.each do |item|
        next unless item.is_a?(QueuedWork)
        deadline = item.deadline
        return deadline if deadline
      end

      nil
    end

    private :next_deadline_locked

    # Hands expired work to the queue timeout handler. Called outside the pool
    # mutex so a slow handler cannot block the pool.
    def notify_expired(expired)
      handler = @queue_timeout_handler
      now = Process.clock_gettime(Process::CLOCK_MONOTONIC)

      expired.each do |item|
        begin
          handler.call(item.work, item.waited(now))
        rescue Exception => e
          STDERR.puts "Error in queue timeout handler: #{e.message} (#{e.class})"
        end
      end
    end

    private :notify_expired

    def trigger_before_thread_start_hooks
      return unless @before_thread_start&.any?

      @before_thread_start.each do |b|
        begin
          b[:block].call(ServerPluginControl.new(@server))
        rescue Exception => e
          STDERR.puts "WARNING before_thread_start hook failed with exception (#{e.class}) #{e.message}"
        end
      end
      nil
    end

    private :trigger_before_thread_start_hooks

    def trigger_before_thread_exit_hooks
      return unless @before_thread_exit&.any?

      @before_thread_exit.each do |b|
        begin
          b[:block].call
        rescue Exception => e
          STDERR.puts "WARNING before_thread_exit hook failed with exception (#{e.class}) #{e.message}"
        end
      end
      nil
    end

    private :trigger_before_thread_exit_hooks

    # @version 5.0.0
    def trigger_out_of_band_hook
      return false unless @out_of_band&.any?

      # we execute on idle hook when all threads are free
      return false unless @spawned == @waiting
      @out_of_band_running = true
      @out_of_band.each { |b| b[:block].call }
      true
    rescue Exception => e
      STDERR.puts "Exception calling out_of_band_hook: #{e.message} (#{e.class})"
      true
    ensure
      @out_of_band_running = false
      @out_of_band_condvar.broadcast
    end

    private :trigger_out_of_band_hook

    def wait_while_out_of_band_running
      return unless @out_of_band_running

      with_mutex do
        @out_of_band_condvar.wait(@mutex) while @out_of_band_running
      end
    end

    def wait_until_not_full
      with_mutex do
        while true
          return if @shutdown
          return if (@spawned - @waiting + @todo.size) < @max

          @not_full.wait @mutex
        end
      end
    end

    # @version 5.0.0
    def with_mutex(&block)
      @mutex.owned? ?
        yield :
        @mutex.synchronize(&block)
    end

    # :nodoc:
    #
    # Must be called with @mutex held!
    #
    def can_spawn_processor?
      io_processors_count = @processors.count(&:marked_as_io_thread?)
      extra_io_processors_count = io_processors_count > @max_io_threads ? io_processors_count - @max_io_threads : 0
      (@spawned - io_processors_count) < (@max - extra_io_processors_count)
    end

    # Add +work+ to the todo list for a Thread to pickup and process.
    #
    # When the pool has a queue timeout, the work's clock starts now: this is
    # the point at which it is ready to be run, which for a request is the
    # moment it is complete and can be handed to the Rack app.
    def <<(work)
      add_work work
    end

    # Add +work+ to the todo list for a Thread to pickup and process.
    #
    # +ready+ says whether the work can be run as soon as a thread takes it.
    # It only matters when the pool has a queue timeout: work that is not ready
    # yet, such as a connection whose request has not been read, has no
    # deadline and can never expire in the queue.
    def add_work(work, ready: true)
      with_mutex do
        if @shutdown
          raise "Unable to add work while shutting down"
        end

        if @queue_timeout
          if ready
            item = QueuedWork.new(work, Process.clock_gettime(Process::CLOCK_MONOTONIC), @queue_timeout)
            @todo << item
            # Ready items share one timeout, so a new one can only expire at or
            # after the deadline the timer already tracks. Wake it only when it
            # has no deadline yet, i.e. when the queue was empty of ready work.
            wake_queue_timeout_timer_locked item.deadline
          else
            @todo << QueuedWork.new(work)
          end
        else
          @todo << work
        end
        t = @todo.size
        @backlog_max = t if t > @backlog_max

        if @waiting < @todo.size and can_spawn_processor?
          spawn_thread
        end

        @not_empty.signal
      end
      self
    end

    # :nodoc:
    #
    # Must be called with @mutex held!
    #
    # Wakes the queue timeout timer when the item just enqueued can expire
    # before the deadline the timer is already waiting for. Ready items share
    # one timeout and are enqueued in order, so an item only needs a wakeup when
    # the timer is not tracking a deadline (`@queue_timeout_next` is nil); that
    # is the case when the queue held no ready work before this enqueue.
    #
    def wake_queue_timeout_timer_locked(deadline)
      return unless @queue_timeout_next.nil? || deadline < @queue_timeout_next
      @queue_timeout_next = deadline
      @queue_timeout_cond.signal
    end

    private :wake_queue_timeout_timer_locked

    def spawn_thread_if_needed # :nodoc:
      with_mutex do
        if @waiting < @todo.size and can_spawn_processor?
          spawn_thread
        end
      end
    end

    # If there are any free threads in the pool, tell one to go ahead
    # and exit. If +force+ is true, then a trim request is requested
    # even if all threads are being utilized.
    #
    def trim(force=false)
      with_mutex do
        free = @waiting - @todo.size
        if (force or free > 0) and @spawned - @trim_requested > @min
          @trim_requested += 1
          @not_empty.signal
        end
      end
    end

    # If there are dead threads in the pool make them go away while decreasing
    # spawned counter so that new healthy threads could be created again.
    def reap
      with_mutex do
        @processors, dead_processors = @processors.partition(&:alive?)

        dead_processors.each do |processor|
          processor.kill
          @spawned -= 1
        end
      end
    end

    class Automaton
      def initialize(pool, timeout, thread_name, message)
        @pool = pool
        @timeout = timeout
        @thread_name = thread_name
        @message = message
        @running = false
      end

      def start!
        @running = true

        @thread = Thread.new do
          Puma.set_thread_name @thread_name
          while @running
            @pool.public_send(@message)
            sleep @timeout
          end
        end
      end

      def stop
        @running = false
        @thread.wakeup
      end
    end

    def auto_trim!(timeout=@auto_trim_time)
      @auto_trim = Automaton.new(self, timeout, "#{@name} tp trim", :trim)
      @auto_trim.start!
    end

    def auto_reap!(timeout=@reaping_time)
      @reaper = Automaton.new(self, timeout, "#{@name} tp reap", :reap)
      @reaper.start!
    end

    # Starts the background thread that enforces the queue timeout.
    #
    # Processor threads also drop expired work when they look for their next
    # item of work, but they can only do so once one of them is free. When
    # every processor thread is busy, this thread is what releases a request
    # that has waited too long, whatever the application threads are doing.
    def start_queue_timeout_timer
      @queue_timeout_thread = Thread.new do
        Puma.set_thread_name "#{@name} tp queue timeout"
        begin
          queue_timeout_loop
        rescue Exception => e
          STDERR.puts "Error reached top of queue timeout timer: #{e.message} (#{e.class})"
        end
      end

      @queue_timeout_thread
    end

    private :start_queue_timeout_timer

    def queue_timeout_loop
      loop do
        expired = nil

        @mutex.synchronize do
          loop do
            return if @shutdown

            deadline = next_deadline_locked
            @queue_timeout_next = deadline

            unless deadline
              # Nothing queued can expire until new work arrives.
              @queue_timeout_cond.wait @mutex
              next
            end

            remaining = deadline - Process.clock_gettime(Process::CLOCK_MONOTONIC)
            if remaining > 0
              @queue_timeout_cond.wait @mutex, remaining
              next
            end

            expired = take_expired_locked
            if expired.empty?
              # The deadline was reached but nothing is expired yet. Don't spin.
              @queue_timeout_cond.wait @mutex, 0.001
              next
            end

            # The pool may have looked full only because of the work that has
            # just been dropped, so let the admission gate re-check.
            @not_full.broadcast
            break
          end
        end

        notify_expired expired
      end
    end

    private :queue_timeout_loop

    # Allows ThreadPool::ForceShutdown to be raised within the
    # provided block if the thread is forced to shutdown during execution.
    def with_force_shutdown
      t = Thread.current
      @shutdown_mutex.synchronize do
        raise ForceShutdown if @force_shutdown
        t[:with_force_shutdown] = true
      end
      yield
    ensure
      t[:with_force_shutdown] = false
    end

    # Tell all threads in the pool to exit and wait for them to finish.
    # Wait +timeout+ seconds then raise +ForceShutdown+ in remaining threads.
    # Next, wait an extra +@shutdown_grace_time+ seconds then force-kill remaining
    # threads. Finally, wait 1 second for remaining threads to exit.
    #
    def shutdown(timeout)
      threads = with_mutex do
        @shutdown = true
        @trim_requested = @spawned
        @not_empty.broadcast
        @not_full.broadcast

        # The queue timeout timer must stop expiring work now that the queue is
        # drained instead of expired. Take it out of its wait and out of the
        # pool, and join it with the processor threads below.
        @queue_timeout_cond.broadcast
        queue_timeout_thread = @queue_timeout_thread
        @queue_timeout_thread = nil

        @auto_trim&.stop
        @reaper&.stop
        # dup processors so that we join them all safely
        @processors.dup.push(queue_timeout_thread).compact
      end

      if @shutdown_debug == true
        shutdown_debug("Shutdown initiated")
      end

      if timeout == -1
        # Wait for threads to finish without force shutdown.
        threads.each(&:join)
      else
        join = ->(inner_timeout) do
          start = Process.clock_gettime(Process::CLOCK_MONOTONIC)
          threads.reject! do |t|
            remaining = inner_timeout - (Process.clock_gettime(Process::CLOCK_MONOTONIC) - start)
            remaining > 0 && t.join(remaining)
          end
        end

        # Wait +timeout+ seconds for threads to finish.
        join.call(timeout)
        if @shutdown_debug == :on_force && !threads.empty?
          shutdown_debug("Shutdown timeout exceeded")
        end

        # If threads are still running, raise ForceShutdown and wait to finish.
        @shutdown_mutex.synchronize do
          @force_shutdown = true
          threads.each do |t|
            t.raise ForceShutdown if t[:with_force_shutdown]
          end
        end
        join.call(@shutdown_grace_time)
        if @shutdown_debug == :on_force && !threads.empty?
          shutdown_debug("Shutdown grace timeout exceeded")
        end

        # If threads are _still_ running, forcefully kill them and wait to finish.
        threads.each(&:kill)
        join.call(1)
      end

      @spawned = 0
      @processors = []
    end

    private

    def shutdown_debug(message)
      pid = Process.pid
      threads = Thread.list

      $stdout.syswrite "#{pid}: #{message}\n"
      $stdout.syswrite "#{pid}: === Begin thread backtrace dump ===\n"

      threads.each_with_index do |thread, index|
        $stdout.syswrite "#{pid}: Thread #{index + 1}/#{threads.size}: #{thread.inspect}\n"
        $stdout.syswrite "#{pid}: #{(thread.backtrace || []).join("\n#{pid}: ")}\n\n"
      end

      $stdout.syswrite "#{pid}: === End thread backtrace dump ===\n"
    end
  end
end
