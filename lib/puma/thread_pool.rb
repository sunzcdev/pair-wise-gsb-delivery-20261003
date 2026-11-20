# frozen_string_literal: true

require 'thread'

require_relative 'io_buffer'
require_relative 'server_plugin_control'

module Puma

  Thread.attr_accessor(:puma_server)

  # Encapsulates cleaning the per-request values out of a processor thread's
  # thread local and fiber local storage.
  #
  # Puma and its plugins may register values on a processor thread for its
  # whole lifetime (e.g. `Thread.current.puma_server`, set when the thread is
  # spawned). Those must survive request after request. A Rack application, on
  # the other hand, may store request-scoped state on `Thread.current` while it
  # handles a request. When `clean_thread_locals` is enabled, the storage is
  # snapshotted before the request and restored afterwards, so everything the
  # application wrote is handed back while the values the thread was born with
  # remain in place.
  #
  # The snapshot/restore runs in an `ensure`, so request fields are also
  # returned when the application raises or the client goes away.
  module ThreadLocalCleaner
    module_function

    # Snapshot the thread's current storage, run the block, then restore the
    # snapshot. Keys the application created are deleted and keys it changed
    # are reset to their original values, on every code path.
    def wrap(thread)
      fiber_locals   = snapshot_fiber_locals(thread)
      thread_locals  = snapshot_thread_locals(thread)

      begin
        yield
      ensure
        restore_fiber_locals(thread, fiber_locals)
        restore_thread_locals(thread, thread_locals)
      end
    end

    # `Thread#keys` only exposes the fiber local storage, which is what
    # `Thread#[]` / `Thread#[]=` operate on. Assigning `nil` removes a key on
    # supported Ruby versions, so the snapshot can be restored exactly.
    def snapshot_fiber_locals(thread)
      thread.keys.each_with_object({}) do |key, snapshot|
        snapshot[key] = thread[key]
      end
    end

    def restore_fiber_locals(thread, snapshot)
      thread.keys.each do |key|
        thread[key] = snapshot.fetch(key) { nil }
      end
      snapshot.each do |key, value|
        thread[key] = value unless thread.key?(key)
      end
    end

    def snapshot_thread_locals(thread)
      thread.thread_variables.each_with_object({}) do |key, snapshot|
        snapshot[key] = thread.thread_variable_get(key)
      end
    end

    def restore_thread_locals(thread, snapshot)
      # Ruby exposes no API to delete a thread variable, but a variable set
      # to nil reports `thread_variable?(key) == false`, which is the same
      # observable state as an unset variable.
      thread.thread_variables.each do |key|
        thread.thread_variable_set(key, nil) unless snapshot.key?(key)
      end
      snapshot.each do |key, value|
        thread.thread_variable_set(key, value)
      end
    end
  end

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

    attr_reader :out_of_band_running

    # Maintain a minimum of +min+ and maximum of +max+ threads
    # in the pool.
    #
    # The block passed is the work that will be performed in each
    # thread.
    #
    def initialize(name, options = {}, server: nil, &block)
      @server = server

      @not_empty = ConditionVariable.new
      @not_full = ConditionVariable.new
      @mutex = Mutex.new
      @todo = Queue.new

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

      # When true, thread locals and fiber locals written while a request is
      # being handled are removed before the processor thread picks up its
      # next piece of work. Values registered on the thread outside of a
      # request (e.g. `puma_server`, or anything a plugin stores on the
      # processor thread for its whole lifetime) are preserved.
      @clean_thread_locals = options[:clean_thread_locals]

      @shutdown = false

      @trim_requested = 0
      @out_of_band_pending = false

      @processors = []

      @auto_trim = nil
      @reaper = nil

      @mutex.synchronize do
        @min.times do
          spawn_thread
          @not_full.wait(@mutex)
        end
      end

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

            work = todo.shift
          end

          begin
            result = if @clean_thread_locals
              ThreadLocalCleaner.wrap(Thread.current) do
                block.call(processor, work)
              end
            else
              block.call(processor, work)
            end
            @out_of_band_pending = true if result
          rescue Exception => e
            STDERR.puts "Error reached top of thread-pool: #{e.message} (#{e.class})"
          end
        end
      end

      @processors << processor

      processor
    end

    private :spawn_thread

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
    def <<(work)
      with_mutex do
        if @shutdown
          raise "Unable to add work while shutting down"
        end

        @todo << work
        t = @todo.size
        @backlog_max = t if t > @backlog_max

        if @waiting < @todo.size and can_spawn_processor?
          spawn_thread
        end

        @not_empty.signal
      end
      self
    end

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

        @auto_trim&.stop
        @reaper&.stop
        # dup processors so that we join them all safely
        @processors.dup
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
