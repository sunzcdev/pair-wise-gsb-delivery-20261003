# frozen_string_literal: true
require_relative "invocation_exception_mixin"
require_relative "execution_trace"

module Rake

  ##
  # A Task is the basic unit of work in a Rakefile.  Tasks have associated
  # actions (possibly more than one) and a list of prerequisites.  When
  # invoked, a task will first ensure that all of its prerequisites have an
  # opportunity to run and then it will execute its own actions.
  #
  # Tasks are not usually created directly using the new method, but rather
  # use the +file+ and +task+ convenience methods.
  #
  class Task
    # List of prerequisites for a task.
    attr_reader :prerequisites
    alias prereqs prerequisites

    # List of order only prerequisites for a task.
    attr_reader :order_only_prerequisites

    # List of actions attached to a task.
    attr_reader :actions

    # Application owning this task.
    attr_accessor :application

    # Array of nested namespaces names used for task lookup by this task.
    attr_reader :scope

    # File/Line locations of each of the task definitions for this
    # task (only valid if the task was defined with the detect
    # location option set).
    attr_reader :locations

    # Has this task already been invoked?  Already invoked tasks
    # will be skipped unless you reenable them.
    attr_reader :already_invoked

    # Return task name
    def to_s
      name
    end

    def inspect # :nodoc:
      "<#{self.class} #{name} => [#{prerequisites.join(', ')}]>"
    end

    # List of sources for task.
    attr_writer :sources
    def sources
      if defined?(@sources)
        @sources
      else
        prerequisites
      end
    end

    # List of prerequisite tasks
    def prerequisite_tasks
      (prerequisites + order_only_prerequisites).map { |pre| lookup_prerequisite(pre) }
    end

    def lookup_prerequisite(prerequisite_name) # :nodoc:
      scoped_prerequisite_task = application[prerequisite_name, @scope]
      if scoped_prerequisite_task == self
        unscoped_prerequisite_task = application[prerequisite_name]
      end
      unscoped_prerequisite_task || scoped_prerequisite_task
    end
    private :lookup_prerequisite

    # List of all unique prerequisite tasks including prerequisite tasks'
    # prerequisites.
    # Includes self when cyclic dependencies are found.
    def all_prerequisite_tasks
      seen = {}
      collect_prerequisites(seen)
      seen.values
    end

    def collect_prerequisites(seen) # :nodoc:
      prerequisite_tasks.each do |pre|
        next if seen[pre.name]
        seen[pre.name] = pre
        pre.collect_prerequisites(seen)
      end
    end
    protected :collect_prerequisites

    # First source from a rule (nil if no sources)
    def source
      sources.first
    end

    # Create a task named +task_name+ with no actions or prerequisites. Use
    # +enhance+ to add actions and prerequisites.
    def initialize(task_name, app)
      @name            = task_name.to_s
      @prerequisites   = []
      @actions         = []
      @already_invoked = false
      @comments        = []
      @lock            = Monitor.new
      @application     = app
      @scope           = app.current_scope
      @arg_names       = nil
      @locations       = []
      @invocation_exception = nil
      @order_only_prerequisites = []
    end

    # Enhance a task with prerequisites or actions.  Returns self.
    def enhance(deps=nil, &block)
      @prerequisites |= deps if deps
      @actions << block if block_given?
      self
    end

    # Name of the task, including any namespace qualifiers.
    def name
      @name.to_s
    end

    # Name of task with argument list description.
    def name_with_args # :nodoc:
      if arg_description
        "#{name}#{arg_description}"
      else
        name
      end
    end

    # Argument description (nil if none).
    def arg_description # :nodoc:
      @arg_names ? "[#{arg_names.join(',')}]" : nil
    end

    # Name of arguments for this task.
    def arg_names
      @arg_names || []
    end

    # Reenable the task, allowing its tasks to be executed if the task
    # is invoked again.
    def reenable
      @already_invoked = false
      @invocation_exception = nil
    end

    # Clear the existing prerequisites, actions, comments, and arguments of a rake task.
    def clear
      clear_prerequisites
      clear_actions
      clear_comments
      clear_args
      self
    end

    # Clear the existing prerequisites of a rake task.
    def clear_prerequisites
      prerequisites.clear
      self
    end

    # Clear the existing actions on a rake task.
    def clear_actions
      actions.clear
      self
    end

    # Clear the existing comments on a rake task.
    def clear_comments
      @comments = []
      self
    end

    # Clear the existing arguments on a rake task.
    def clear_args
      @arg_names = nil
      self
    end

    # Invoke the task if it is needed.  Prerequisites are invoked first.
    def invoke(*args)
      task_args = TaskArguments.new(arg_names, args)
      invoke_with_call_chain(task_args, InvocationChain::EMPTY)
    end

    # Same as invoke, but explicitly pass a call chain to detect
    # circular dependencies.
    #
    # If multiple tasks depend on this
    # one in parallel, they will all fail if the first execution of
    # this task fails.
    def invoke_with_call_chain(task_args, invocation_chain)
      new_chain = Rake::InvocationChain.append(self, invocation_chain)
      @lock.synchronize do
        trace = execution_trace
        invocation = trace.begin_invocation(self)
        begin
          if application.options.trace
            application.trace "** Invoke #{name} #{format_trace_flags}"
          end

          if @already_invoked
            trace.skipped_already_invoked(self, invocation)
            if @invocation_exception
              if application.options.trace
                application.trace "** Previous invocation of #{name} failed #{format_trace_flags}"
              end
              raise @invocation_exception
            else
              return
            end
          end

          @already_invoked = true

          invoke_prerequisites_with_trace(task_args, new_chain, trace, invocation)
          if needed?
            execute(task_args)
          else
            trace.skipped_not_needed(self, invocation)
            nil
          end
        rescue Exception => ex
          add_chain_to(ex, new_chain)
          @invocation_exception = ex
          raise ex
        ensure
          trace.finish_invocation(invocation)
        end
      end
    end
    protected :invoke_with_call_chain

    # The trace receiving structured execution events for this task.
    def execution_trace # :nodoc:
      app = application
      if app.respond_to?(:execution_trace)
        app.execution_trace
      else
        ExecutionTrace::NULL
      end
    end
    private :execution_trace

    # Invoke the prerequisites while recording how long the wait took.
    # A task with no prerequisites did not wait for anything, so it does
    # not produce a dependency wait event.
    def invoke_prerequisites_with_trace(task_args, invocation_chain, trace, invocation)
      if invocation.nil?
        return invoke_prerequisites(task_args, invocation_chain)
      end
      waited_on = declared_prerequisite_names
      return invoke_prerequisites(task_args, invocation_chain) if waited_on.empty?
      started_at = trace.now
      begin
        invoke_prerequisites(task_args, invocation_chain)
      ensure
        trace.dependency_wait(self, invocation, started_at, waited_on)
      end
    end
    private :invoke_prerequisites_with_trace

    # Declared prerequisite names, in resolution order, without forcing a
    # task lookup just to describe the wait.
    def declared_prerequisite_names # :nodoc:
      (prerequisites + order_only_prerequisites).map(&:to_s)
    end
    private :declared_prerequisite_names

    def add_chain_to(exception, new_chain) # :nodoc:
      exception.extend(InvocationExceptionMixin) unless
        exception.respond_to?(:chain)
      exception.chain = new_chain if exception.chain.nil?
    end
    private :add_chain_to

    # Invoke all the prerequisites of a task.
    def invoke_prerequisites(task_args, invocation_chain) # :nodoc:
      if application.options.always_multitask
        invoke_prerequisites_concurrently(task_args, invocation_chain)
      else
        prerequisite_tasks.each { |p|
          prereq_args = task_args.new_scope(p.arg_names)
          p.invoke_with_call_chain(prereq_args, invocation_chain)
        }
      end
    end

    # Invoke all the prerequisites of a task in parallel.
    def invoke_prerequisites_concurrently(task_args, invocation_chain)# :nodoc:
      trace = execution_trace
      parent_invocation = trace.current_invocation
      futures = prerequisite_tasks.map do |p|
        prereq_args = task_args.new_scope(p.arg_names)
        application.thread_pool.future(p) do |r|
          trace.with_current_invocation(parent_invocation) do
            r.invoke_with_call_chain(prereq_args, invocation_chain)
          end
        end
      end
      # Iterate in reverse to improve performance related to thread waiting and switching
      futures.reverse_each(&:value)
    end

    # Format the trace flags for display.
    def format_trace_flags
      flags = []
      flags << "first_time" unless @already_invoked
      flags << "not_needed" unless needed?
      flags.empty? ? "" : "(" + flags.join(", ") + ")"
    end
    private :format_trace_flags

    # Execute the actions associated with this task.
    #
    # Each action is bracketed by structured trace events when an
    # ExecutionTrace is active.  An action that raises produces a single
    # +:action_failed+ event and the original exception is re-raised, so
    # only the task whose action actually failed is recorded.  A dry run
    # executes nothing and therefore records no action events.
    def execute(args=nil)
      args ||= EMPTY_TASK_ARGS
      if application.options.dryrun
        application.trace "** Execute (dry run) #{name}"
        return
      end
      application.trace "** Execute #{name}" if application.options.trace
      application.enhance_with_matching_rule(name) if @actions.empty?
      trace = execution_trace
      invocation = trace.current_invocation
      standalone = false
      if invocation.nil? && trace.enabled?
        # Direct #execute calls still count as real work, even outside the
        # usual invoke path.
        invocation = trace.begin_invocation(self, nil)
        standalone = !invocation.nil?
      end
      opts = Hash.try_convert(args)
      opts = nil if opts && opts.empty?
      begin
        @actions.each_with_index { |act, index|
          started_at = trace.action_started(self, invocation, index)
          begin
            if opts
              act.call(self, args, **opts)
            else
              act.call(self, args)
            end
          rescue Exception => ex
            trace.action_failed(self, invocation, index, started_at, ex)
            raise
          else
            trace.action_succeeded(self, invocation, index, started_at)
          end
        }
      ensure
        trace.finish_invocation(invocation) if standalone
      end
    end

    # Is this task needed?
    def needed?
      true
    end

    # Timestamp for this task.  Basic tasks return the current time for their
    # time stamp.  Other tasks can be more sophisticated.
    def timestamp
      Time.now
    end

    # Add a description to the task.  The description can consist of an option
    # argument list (enclosed brackets) and an optional comment.
    def add_description(description)
      return unless description
      comment = description.strip
      add_comment(comment) if comment && !comment.empty?
    end

    def comment=(comment) # :nodoc:
      add_comment(comment)
    end

    def add_comment(comment) # :nodoc:
      return if comment.nil?
      @comments << comment unless @comments.include?(comment)
    end
    private :add_comment

    # Full collection of comments. Multiple comments are separated by
    # newlines.
    def full_comment
      transform_comments("\n")
    end

    # First line (or sentence) of all comments. Multiple comments are
    # separated by a "/".
    def comment
      transform_comments(" / ") { |c| first_sentence(c) }
    end

    # Transform the list of comments as specified by the block and
    # join with the separator.
    def transform_comments(separator, &block)
      if @comments.empty?
        nil
      else
        block ||= lambda { |c| c }
        @comments.map(&block).join(separator)
      end
    end
    private :transform_comments

    # Get the first sentence in a string. The sentence is terminated
    # by the first period, exclamation mark, or the end of the line.
    # Decimal points do not count as periods.
    def first_sentence(string)
      string.split(/(?<=\w)(\.|!)[ \t]|(\.$|!)|\n/).first
    end
    private :first_sentence

    # Set the names of the arguments for this task. +args+ should be
    # an array of symbols, one for each argument name.
    def set_arg_names(args)
      @arg_names = args.map(&:to_sym)
    end

    # Return a string describing the internal state of a task.  Useful for
    # debugging.
    def investigation
      result = "------------------------------\n".dup
      result << "Investigating #{name}\n"
      result << "class: #{self.class}\n"
      result <<  "task needed: #{needed?}\n"
      result <<  "timestamp: #{timestamp}\n"
      result << "pre-requisites: \n"
      prereqs = prerequisite_tasks
      prereqs.sort! { |a, b| a.timestamp <=> b.timestamp }
      prereqs.each do |p|
        result << "--#{p.name} (#{p.timestamp})\n"
      end
      latest_prereq = prerequisite_tasks.map(&:timestamp).max
      result <<  "latest-prerequisite time: #{latest_prereq}\n"
      result << "................................\n\n"
      return result
    end

    # Format dependencies parameter to pass to task.
    def self.format_deps(deps)
      deps = [deps] unless deps.respond_to?(:to_ary)
      deps.map { |d| Rake.from_pathname(d).to_s }
    end

    # Add order only dependencies.
    def |(deps)
      @order_only_prerequisites |= Task.format_deps(deps) - @prerequisites
      self
    end

    # ----------------------------------------------------------------
    # Rake Module Methods
    #
    class << self

      # Clear the task list.  This cause rake to immediately forget all the
      # tasks that have been assigned.  (Normally used in the unit tests.)
      def clear
        Rake.application.clear
      end

      # List of all defined tasks.
      def tasks
        Rake.application.tasks
      end

      # Return a task with the given name.  If the task is not currently
      # known, try to synthesize one from the defined rules.  If no rules are
      # found, but an existing file matches the task name, assume it is a file
      # task with no dependencies or actions.
      def [](task_name)
        Rake.application[task_name]
      end

      # TRUE if the task name is already defined.
      def task_defined?(task_name)
        Rake.application.lookup(task_name) != nil
      end

      # Define a task given +args+ and an option block.  If a rule with the
      # given name already exists, the prerequisites and actions are added to
      # the existing task.  Returns the defined task.
      def define_task(*args, &block)
        Rake.application.define_task(self, *args, &block)
      end

      # Define a rule for synthesizing tasks.
      def create_rule(*args, &block)
        Rake.application.create_rule(*args, &block)
      end

      # Apply the scope to the task name according to the rules for
      # this kind of task.  Generic tasks will accept the scope as
      # part of the name.
      def scope_name(scope, task_name)
        scope.path_with_task_name(task_name)
      end

    end # class << Rake::Task
  end # class Rake::Task
end
