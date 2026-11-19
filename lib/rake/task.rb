# frozen_string_literal: true
require_relative "invocation_exception_mixin"
require_relative "invocation_identity"

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

    # How multiple invocations of this task are instantiated:
    #
    # :single:: (default) the task is built at most once.  A later
    #           invocation carrying a different invocation identity is a
    #           conflict, resolved according to #conflict_policy.
    # :multi::  the task is declared multi-instantiable: each distinct
    #           invocation identity is an independent instance with its
    #           own invoked state and is built once per identity.
    attr_reader :invocation_policy

    # What a :single instance task does when it is invoked with an
    # invocation identity that conflicts with an earlier invocation:
    #
    # :warn::  (default) warn about the conflicting arguments and reuse
    #          the first instance.
    # :error:: raise a Rake::TaskArgumentError before anything runs.
    # :reuse:: silently reuse the first instance (legacy behavior).
    attr_reader :conflict_policy

    # Declared default values for this task's formal parameters
    # (a hash of Symbol => value, possibly empty).
    def arg_defaults
      @arg_defaults ||= {}
    end

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
      @arg_defaults    = nil
      @locations       = []
      @invocation_policy = :single
      @conflict_policy   = :warn
      @invocations     = {}
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

    # Set the invocation policy (+:single+ or +:multi+).  See
    # #invocation_policy.
    def invocation_policy=(policy)
      policy = policy.to_sym
      unless [:single, :multi].include?(policy)
        fail TaskArgumentError,
          "unknown invocation policy #{policy.inspect} for task '#{name}' " \
          "(expected :single or :multi)"
      end
      @invocation_policy = policy
    end

    # Set the conflict policy (+:warn+, +:error+ or +:reuse+).  See
    # #conflict_policy.
    def conflict_policy=(policy)
      policy = policy.to_sym
      unless [:warn, :error, :reuse].include?(policy)
        fail TaskArgumentError,
          "unknown conflict policy #{policy.inspect} for task '#{name}' " \
          "(expected :warn, :error or :reuse)"
      end
      @conflict_policy = policy
    end

    # Declare this task multi-instantiable: each distinct invocation
    # identity is built as an independent instance.  Returns self.
    def multi_instance!
      self.invocation_policy = :multi
      self
    end

    # Is this task declared multi-instantiable?
    def multi_instance?
      @invocation_policy == :multi
    end

    # The invocation identities recorded for this task so far, in
    # invocation order.  A single-instance task records at most one.
    def invocation_identities
      @invocations.keys
    end

    # The canonical invocation identity that a call with the given
    # arguments would have, with declared defaults applied and named
    # arguments bound to the real formal parameters.  Unknown named
    # arguments are reported (Rake::TaskArgumentError) just as they would
    # be by #invoke.
    def identity_for(*args)
      task_args = bind_invocation_arguments(args)
      task_args.with_defaults(arg_defaults)
      invocation_identity_for(task_args)
    end

    # Would two invocations with the given argument lists be the same
    # build instance of this task?  For a multi-instance task this is
    # true exactly when both argument lists produce the same invocation
    # identity; for a single-instance task the first invocation wins and
    # any later invocation reuses (or conflicts with) that one instance.
    def same_invocation_instance?(args_a, args_b)
      !multi_instance? ||
        identity_for(*args_a) == identity_for(*args_b)
    end

    # Reenable the task, allowing its tasks to be executed if the task
    # is invoked again.  All recorded invocation identities are forgotten.
    def reenable
      @already_invoked = false
      @invocations.clear
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
      @arg_defaults = nil
      self
    end

    # Invoke the task if it is needed.  Prerequisites are invoked first.
    #
    # Arguments are bound to the task's declared formal parameters.
    # Positional values bind in declaration order; a trailing hash of
    # Symbol keys is treated as named arguments and binds by parameter
    # name.  Named arguments that do not match a declared formal
    # parameter are reported (Rake::TaskArgumentError) before any action
    # runs.
    def invoke(*args)
      task_args = bind_invocation_arguments(args)
      invoke_with_call_chain(task_args, InvocationChain::EMPTY)
    end

    # Build the TaskArguments for a top-level invocation, binding
    # positional and named arguments to the declared formal parameters.
    def bind_invocation_arguments(args) # :nodoc:
      args = args.dup
      named = {}
      if args.last.is_a?(Hash) &&
         args.last.keys.all? { |key| key.respond_to?(:to_sym) }
        named = args.pop
      end

      unknown = named.keys.map(&:to_sym) - arg_names
      unless unknown.empty?
        fail TaskArgumentError,
          "unknown argument#{unknown.size == 1 ? '' : 's'} " \
          "#{unknown.map { |name| ":#{name}" }.join(', ')} " \
          "for task '#{name}' " \
          "(declared arguments: #{arg_names.map { |n| ":#{n}" }.join(', ')})"
      end

      values = args
      positional_count = args.length
      named.each do |key, value|
        index = arg_names.index(key.to_sym)
        if index < positional_count
          fail TaskArgumentError,
            "argument :#{key} for task '#{name}' was given both " \
            "positionally and by name"
        end
        values[index] = value
      end
      TaskArguments.new(arg_names, values)
    end
    private :bind_invocation_arguments

    # The canonical invocation identity of this task for the given
    # (already bound) task arguments.
    def invocation_identity_for(task_args)
      InvocationIdentity.for_task(self, task_args)
    end
    private :invocation_identity_for

    # Same as invoke, but explicitly pass a call chain to detect
    # circular dependencies.
    #
    # If multiple tasks depend on this
    # one in parallel, they will all fail if the first execution of
    # this task fails.
    def invoke_with_call_chain(task_args, invocation_chain)
      new_chain = Rake::InvocationChain.append(self, invocation_chain)
      @lock.synchronize do
        task_args.with_defaults(arg_defaults)
        identity = invocation_identity_for(task_args)

        if application.options.trace
          application.trace "** Invoke #{identity} #{format_trace_flags(identity)}"
        end

        if @invocations.has_key?(identity)
          # This exact instance was already invoked: replay its outcome.
          if exception = @invocations[identity]
            if application.options.trace
              application.trace "** Previous invocation of #{identity} failed #{format_trace_flags(identity)}"
            end
            raise exception
          end
          return
        end

        if @already_invoked && !multi_instance?
          # A single-instance task invoked with a conflicting identity:
          # never silently pass the first call's TaskArguments to this
          # call.  Resolve according to the conflict policy.
          first_identity, first_exception = @invocations.first
          handle_conflicting_invocation(identity, first_identity)
          raise first_exception if first_exception
          return
        end

        begin
          @already_invoked = true
          @invocations[identity] = nil

          invoke_prerequisites(task_args, new_chain)
          execute(task_args) if needed?
        rescue Exception => ex
          add_chain_to(ex, new_chain)
          @invocations[identity] = ex
          raise ex
        end
      end
    end
    protected :invoke_with_call_chain

    # Resolve an invocation whose identity conflicts with the identity of
    # the single instance that has already been invoked.  Raises
    # TaskArgumentError for the :error policy, warns for the :warn policy
    # and stays silent for the :reuse (legacy) policy.
    def handle_conflicting_invocation(identity, first_identity) # :nodoc:
      case conflict_policy
      when :error
        fail TaskArgumentError,
          "task '#{name}' was already invoked as #{first_identity} and " \
          "cannot also be invoked as #{identity}; declare the task " \
          "multi-instance (invocation_policy = :multi) or reenable it " \
          "to build it once per argument set"
      when :warn
        warn "Rake: task '#{name}' was already invoked as #{first_identity}; " \
             "reusing that instance and ignoring the conflicting " \
             "invocation #{identity}"
      end
      nil
    end
    private :handle_conflicting_invocation

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
      futures = prerequisite_tasks.map do |p|
        prereq_args = task_args.new_scope(p.arg_names)
        application.thread_pool.future(p) do |r|
          r.invoke_with_call_chain(prereq_args, invocation_chain)
        end
      end
      # Iterate in reverse to improve performance related to thread waiting and switching
      futures.reverse_each(&:value)
    end

    # Format the trace flags for display.  The flags state whether this
    # invocation is the first one, a repeat of an already seen instance,
    # a new instance of a multi-instance task, or a conflicting argument
    # set for a single-instance task.
    def format_trace_flags(identity=nil)
      flags = []
      if identity && @invocations.has_key?(identity)
        flags << "same_instance"
      elsif @already_invoked && !multi_instance?
        flags << "conflicting_instance"
      elsif @already_invoked
        flags << "new_instance"
      else
        flags << "first_time"
      end
      flags << "not_needed" unless needed?
      flags.empty? ? "" : "(" + flags.join(", ") + ")"
    end
    private :format_trace_flags

    # Execute the actions associated with this task.
    def execute(args=nil)
      args ||= EMPTY_TASK_ARGS
      if application.options.dryrun
        application.trace "** Execute (dry run) #{name}"
        return
      end
      application.trace "** Execute #{name}" if application.options.trace
      application.enhance_with_matching_rule(name) if @actions.empty?
      if opts = Hash.try_convert(args) and !opts.empty?
        @actions.each { |act| act.call(self, args, **opts) }
      else
        @actions.each { |act| act.call(self, args) }
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
    # an array of symbols, one for each argument name.  A trailing hash
    # declares additional formal parameters together with their default
    # values, e.g. `set_arg_names([:a, { b: 2 }])` declares parameter
    # +:a+ and parameter +:b+ defaulting to 2.
    def set_arg_names(args)
      args = args.dup
      if args.last.is_a?(Hash)
        defaults = args.pop
        @arg_defaults = defaults.each_with_object({}) do |(key, value), hash|
          hash[key.to_sym] = value
        end
        args += @arg_defaults.keys
      end
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
      result <<  "invocation policy: #{invocation_policy}" \
                 " (conflict policy: #{conflict_policy})\n"
      if @invocations.empty?
        result << "invocations: none\n"
      else
        result << "invocations:\n"
        @invocations.each_key do |identity|
          result << "--#{identity}\n"
        end
      end
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
