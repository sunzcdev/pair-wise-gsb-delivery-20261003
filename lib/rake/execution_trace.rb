# frozen_string_literal: true
module Rake

  ##
  # Records structured events describing what a Rake run actually did.
  #
  # The regular <tt>--trace</tt> output is human readable text.  An
  # ExecutionTrace is the machine readable counterpart used by build
  # tooling: it distinguishes waiting for prerequisites, skipping a task
  # that was already invoked, skipping a task whose artifact is up to
  # date, and starting, succeeding or failing an action.
  #
  # Events are represented as frozen Hash-like objects with these keys:
  #
  # [:event]        One of +:dependency_wait+, +:skipped_already_invoked+,
  #                 +:skipped_not_needed+, +:action_started+,
  #                 +:action_succeeded+ or +:action_failed+.
  # [:task]         The task name.
  # [:invocation]   Stable identity of this invocation.  All events raised
  #                 while one task invocation is active share the same id.
  # [:parent]       Invocation id of the invoking task, or nil at top level.
  # [:time]         Monotonic clock reading when the event was recorded.
  # [:duration]     Monotonic elapsed seconds (completed events only).
  # [:action_index] Zero based index of the action (action events only).
  # [:prerequisites] Prerequisite names of the task (dependency wait only).
  # [:exception]    Class name of the failure (action failures only).
  # [:causes]       Class names of the exception cause chain, when present.
  #
  # Action events are recorded once per action execution.  A shared
  # prerequisite reached through several dependency edges is executed
  # once, so its action is counted once; the other edges only produce
  # +:skipped_already_invoked+ events.  When prerequisites fail, only the
  # task whose action actually raised gets an +:action_failed+ event, so
  # nested exceptions keep the real failure node.
  #
  # Observers are called synchronously, but an observer that raises is
  # removed and its failure is recorded in #observer_failures.  Observer
  # errors never abort, retry or re-execute a task.  Exception messages,
  # backtraces, the process environment and command lines are deliberately
  # not part of an event.
  #
  # Recording is lazy: with no observer, no output stream and no
  # <tt>trace_events</tt> option nothing is allocated, so leaving the trace
  # in place costs nothing when it is unused.  A trace with an output stream
  # streams events instead of accumulating them in memory; use #events when
  # you want to inspect them in process.
  #
  # == Example
  #
  #   trace = Rake.application.execution_trace
  #   trace.add_observer do |event|
  #     Metrics.increment(event[:event], task: event[:task])
  #   end
  #
  # Structured output is enabled with <tt>--trace-events</tt>, which writes
  # one JSON document per event to the selected stream.  Observers and the
  # in-memory event list do not replace the regular text trace.
  #
  class ExecutionTrace

    # A structured trace event.  It behaves like a Hash but also exposes
    # convenience readers for the common fields.
    class Event < Hash
      def event
        self[:event]
      end

      def task
        self[:task]
      end

      def invocation
        self[:invocation]
      end

      def parent
        self[:parent]
      end

      def time
        self[:time]
      end

      def duration
        self[:duration]
      end

      def action_index
        self[:action_index]
      end

      def exception
        self[:exception]
      end

      # Return a plain Hash copy suitable for serialization.
      def to_h
        Hash[self]
      end

      def inspect
        "#<#{self.class} #{super}>"
      end
    end

    # Thread local stack of active invocation ids.
    THREAD_KEY = :rake_execution_trace_stack

    # Inert trace used when a task is attached to an object that does not
    # expose an execution trace (for example a bare TaskManager).
    class NullTrace # :nodoc:
      def now
        ExecutionTrace.now
      end

      def enabled?
        false
      end

      def current_invocation
        nil
      end

      def begin_invocation(*)
        nil
      end

      def finish_invocation(*)
        nil
      end

      def with_current_invocation(*)
        yield
      end

      def dependency_wait(*)
        nil
      end

      def skipped_already_invoked(*)
        nil
      end

      def skipped_not_needed(*)
        nil
      end

      def action_started(*)
        nil
      end

      def action_succeeded(*)
        nil
      end

      def action_failed(*)
        nil
      end
    end

    NULL = NullTrace.new.freeze

    # The application (or task manager) owning this trace.
    attr_reader :application

    # Force structured recording even when no observer or output is set.
    attr_accessor :enabled

    # Return the monotonic clock reading in seconds.
    def self.now
      Process.clock_gettime(Process::CLOCK_MONOTONIC)
    rescue StandardError
      Time.now.to_f
    end

    def initialize(application = nil)
      @application = application
      @mutex = Mutex.new
      @output_mutex = Mutex.new
      @sequence = 0
      @parents = {}
      @events = []
      @observers = []
      @has_observers = false
      @observer_failures = []
      @enabled = false
      @output = nil
      @output_failed = false
    end

    # Is structured recording active?
    def enabled?
      return true if @enabled
      return true if @has_observers
      return true if @output && !@output_failed
      option = application_option(:trace_events)
      !option.nil? && option != false
    end

    # The stream receiving JSON lines, or nil when no stream is selected.
    def output
      return nil if @output_failed
      return @output if @output
      return nil unless application_option(:trace_events)
      application_option(:trace_events_output)
    end

    # Select an explicit output stream (an IO like StringIO or a File).
    def output=(io)
      @output = io
      @output_failed = false
    end

    # Register an observer.  It is called with each Event.
    def add_observer(observer = nil, &block)
      observer ||= block
      raise ArgumentError, "observer required" unless observer
      @mutex.synchronize do
        @observers << observer unless @observers.include?(observer)
        @has_observers = true
      end
      observer
    end
    alias observe add_observer

    # Remove a previously registered observer.
    def remove_observer(observer)
      @mutex.synchronize do
        @observers.delete(observer)
        @has_observers = !@observers.empty?
      end
      observer
    end

    # A snapshot of the registered observers.
    def observers
      @mutex.synchronize { @observers.dup }
    end

    # Class names of observers that were disabled because they raised.
    def observer_failures
      @mutex.synchronize { @observer_failures.dup }
    end

    # A snapshot of the recorded events.
    def events
      @mutex.synchronize { @events.dup }
    end

    # Forget recorded events.  Active invocations keep their identity.
    def clear
      @mutex.synchronize { @events.clear }
      self
    end

    def now
      self.class.now
    end

    # Invocation id currently active on this thread.
    def current_invocation
      stack = Thread.current[THREAD_KEY]
      stack && stack.last
    end

    # Begin a tracked task invocation and return its stable id.
    def begin_invocation(task, parent = current_invocation)
      return nil unless enabled?
      id = @mutex.synchronize do
        @sequence += 1
        @parents[@sequence] = parent
        @sequence
      end
      push_invocation(id)
      id
    end

    # Finish a tracked invocation.  Safe to call with nil.
    def finish_invocation(id)
      return unless id
      pop_invocation(id)
      @mutex.synchronize { @parents.delete(id) }
      nil
    end

    # Make +id+ the current invocation for the duration of the block.
    # Used to carry the parent identity into thread pool workers.
    def with_current_invocation(id)
      return yield unless id
      push_invocation(id)
      begin
        yield
      ensure
        pop_invocation(id)
      end
    end

    # Record a dependency wait.  Returns the event or nil.
    def dependency_wait(task, invocation, started_at, prerequisites)
      return nil unless invocation && started_at && enabled?
      finished = now
      emit(task, invocation, :dependency_wait,
           time: finished,
           duration: non_negative(finished - started_at),
           prerequisites: prerequisites.map(&:to_s))
    end

    # Record that an invocation was skipped because the task had already
    # been invoked.
    def skipped_already_invoked(task, invocation)
      emit(task, invocation, :skipped_already_invoked)
    end

    # Record that an invocation was skipped because the task's artifact
    # was up to date (Task#needed? returned false).
    def skipped_not_needed(task, invocation)
      emit(task, invocation, :skipped_not_needed)
    end

    # Record the start of one action.  Returns the start time, which the
    # caller passes back to #action_succeeded or #action_failed, or nil when
    # the action must not be counted (trace disabled).
    def action_started(task, invocation, index)
      return nil unless invocation && enabled?
      started_at = now
      emit(task, invocation, :action_started,
           time: started_at, action_index: index.to_i)
      started_at
    end

    # Record a successfully completed action.
    def action_succeeded(task, invocation, index, started_at)
      return nil unless invocation && started_at && enabled?
      finished = now
      emit(task, invocation, :action_succeeded,
           time: finished,
           duration: non_negative(finished - started_at),
           action_index: index.to_i)
    end

    # Record a failed action.  Only the action that actually raised is
    # recorded; callers re-raise the original exception untouched.
    def action_failed(task, invocation, index, started_at, exception)
      return nil unless invocation && started_at && enabled?
      finished = now
      attributes = {
        time: finished,
        duration: non_negative(finished - started_at),
        action_index: index.to_i,
        exception: exception.class.to_s,
      }
      causes = cause_classes(exception)
      attributes[:causes] = causes unless causes.empty?
      emit(task, invocation, :action_failed, attributes)
    end

    private

    def emit(task, invocation, event_name, attributes = {})
      return nil unless invocation && enabled?
      parent = @mutex.synchronize { @parents[invocation] }
      event = Event.new
      event[:event] = event_name
      event[:task] = task_name(task)
      event[:invocation] = invocation
      event[:parent] = parent
      attrs = attributes.dup
      event[:time] = attrs.key?(:time) ? attrs.delete(:time) : now
      attrs.each { |key, value| event[key] = value unless value.nil? }
      event.freeze
      publish(event)
    rescue StandardError
      nil
    end

    def publish(event)
      observers = @mutex.synchronize do
        @events << event if @enabled || !@observers.empty?
        @observers.dup
      end
      observers.each { |observer| call_observer(observer, event) }
      write_event(event)
      event
    end

    def call_observer(observer, event)
      observer.call(event)
    rescue Exception => ex
      raise if fatal_exception?(ex)
      disable_observer(observer, ex)
    end

    def disable_observer(observer, exception)
      @mutex.synchronize do
        @observers.delete(observer)
        @has_observers = !@observers.empty?
        @observer_failures << exception.class.to_s
      end
      nil
    end

    # Do not swallow exceptions that are not failures of the observer body.
    def fatal_exception?(exception)
      exception.is_a?(SystemExit) ||
        exception.is_a?(SignalException) ||
        exception.is_a?(NoMemoryError)
    end

    def write_event(event)
      stream = output
      return unless stream
      line = json_line(event)
      @output_mutex.synchronize { stream.write(line + "\n") }
    rescue StandardError
      @output_failed = true
      nil
    end

    def json_line(event)
      if !defined?(@json_available)
        begin
          require "json"
          @json_available = true
        rescue LoadError
          @json_available = false
        end
      end
      @json_available ? JSON.generate(event.to_h) : event.to_h.inspect
    end

    def task_name(task)
      if task.respond_to?(:name)
        task.name.to_s
      else
        task.to_s
      end
    end

    def cause_classes(exception)
      classes = []
      seen = {}
      current = exception.respond_to?(:cause) ? exception.cause : nil
      while current && !seen[current.object_id] && classes.size < 5
        seen[current.object_id] = true
        classes << current.class.to_s
        current = current.respond_to?(:cause) ? current.cause : nil
      end
      classes
    end

    def non_negative(value)
      value < 0 ? 0.0 : value
    end

    def application_option(name)
      app = @application
      return nil unless app && app.respond_to?(:options)
      opts = app.options
      return nil unless opts && opts.respond_to?(name)
      opts.public_send(name)
    rescue StandardError
      nil
    end

    def push_invocation(id)
      (Thread.current[THREAD_KEY] ||= []) << id
    end

    def pop_invocation(id)
      stack = Thread.current[THREAD_KEY]
      return unless stack
      if stack.last == id
        stack.pop
      else
        stack.delete(id)
      end
      Thread.current[THREAD_KEY] = nil if stack.empty?
    end
  end
end
