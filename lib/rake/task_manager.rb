# frozen_string_literal: true
require_relative "rule_search"

module Rake

  # The TaskManager module is a mixin for managing tasks.
  module TaskManager
    # Track the last comment made in the Rakefile.
    attr_accessor :last_description

    def initialize # :nodoc:
      super
      @tasks = Hash.new
      @rules = Array.new
      @rules_revision = 0
      @scope = Scope.make
      @last_description = nil
      @rule_search_budget = RuleSearchBudget.new
      self.last_rule_search_report = nil
    end

    # Budget applied when resolving an implicit task through rules.
    #
    # The default allows up to RuleSearchBudget::DEFAULT_MAX_DEPTH nested
    # rule levels and RuleSearchBudget::DEFAULT_MAX_CANDIDATES candidate
    # sources per lookup.  Assign a Rake::RuleSearchBudget instance (or a
    # hash of its parameters) to tighten or widen the search.
    attr_reader :rule_search_budget

    def rule_search_budget=(budget)
      @rule_search_budget =
        case budget
        when RuleSearchBudget
          budget
        when Hash
          RuleSearchBudget.new(**budget)
        else
          fail ArgumentError, "rule_search_budget must be a Rake::RuleSearchBudget or Hash"
        end
      # A report carries the budget it was produced under; do not expose it
      # after the budget has been reconfigured.
      self.last_rule_search_report = nil
    end

    # The report produced by the most recent implicit rule lookup on this
    # thread.
    #
    # Returns nil when no lookup ran, and nil when the rules have changed
    # since the report was produced.  The report is read-only diagnostic
    # data; it is never consulted to resolve a later lookup, so a plan
    # produced under one rule configuration cannot be reused under another.
    def last_rule_search_report
      report = thread_last_report
      return nil if report && report.stale?(rules_revision)
      report
    end

    def last_rule_search_report=(report) # :nodoc:
      Thread.current[thread_last_report_key] = report
    end

    # Number of rule configuration changes applied to this task manager.
    # Every create_rule and clear bumps the revision.
    def rules_revision
      @rules_revision
    end

    # The rule search report currently in flight on this thread, if any.
    # This is separate from #last_rule_search_report, which is the published
    # result of the most recent completed lookup.
    def current_rule_search_report # :nodoc:
      Thread.current[thread_search_report_key]
    end

    def current_rule_search_report=(report) # :nodoc:
      Thread.current[thread_search_report_key] = report
    end

    def create_rule(*args, &block) # :nodoc:
      pattern, args, deps, order_only = resolve_args(args)
      pattern = Regexp.new(Regexp.quote(pattern) + "$") if String === pattern
      @rules << [pattern, args, deps, order_only, block]
      @rules_revision += 1
      self.last_rule_search_report = nil
    end

    def define_task(task_class, *args, &block) # :nodoc:
      task_name, arg_names, deps, order_only = resolve_args(args)

      original_scope = @scope
      if String === task_name and
         not task_class.ancestors.include? Rake::FileTask
        task_name, *definition_scope = *(task_name.split(":").reverse)
        @scope = Scope.make(*(definition_scope + @scope.to_a))
      end

      task_name = task_class.scope_name(@scope, task_name)
      task = intern(task_class, task_name)
      task.set_arg_names(arg_names) unless arg_names.empty?
      if Rake::TaskManager.record_task_metadata
        add_location(task)
        task.add_description(get_description)
      end
      task.enhance(Task.format_deps(deps), &block)
      task | order_only unless order_only.nil?
      task
    ensure
      @scope = original_scope
    end

    # Lookup a task.  Return an existing task if found, otherwise
    # create a task of the current type.
    def intern(task_class, task_name)
      @tasks[task_name.to_s] ||= task_class.new(task_name, self)
    end

    # Find a matching task for +task_name+.
    #
    # Resolution tries, in order: an already defined task, a task
    # synthesized from the rules, and finally an existing file with that
    # name.  If a rule search stops because it exhausted its depth or
    # candidate budget, the plan is undetermined.  The target is only
    # reported as missing when that search finished without a match and no
    # file with the target name exists.
    def [](task_name, scopes=nil)
      task_name = task_name.to_s

      # A rule dependent may be a proc that itself looks up a task.  A nested
      # lookup shares the in-flight report so the budget, cycle chain and
      # depth count cannot be reset from inside a rule.  The report is kept
      # per thread because prerequisites may be resolved concurrently.
      active_report = current_rule_search_report
      if active_report
        task = lookup(task_name, scopes)
        return task if task
        task = enhance_with_matching_rule(task_name, active_report.current_level, active_report)
        return task if task
        task = synthesize_file_task(task_name)
        return task if task
        fail generate_message_for_undefined_task(task_name)
      end

      # Each top level lookup starts with no rule report, so the report always
      # describes the lookup that just ran rather than an earlier one.
      self.last_rule_search_report = nil
      task = lookup(task_name, scopes)
      return task if task

      begin
        task = enhance_with_matching_rule(task_name)
        return task if task
      rescue Rake::RuleRecursionOverflowError => ex
        # Budget exhaustion, the depth limit and a rule cycle all leave the
        # plan undetermined.  None of them proves the target file is missing,
        # so they are reported through this dedicated error instead of
        # falling through to "Don't know how to build task".  The error
        # records whether the target file happens to exist on disk.
        ex.target_file_exists = File.exist?(task_name)
        ex.report.target_file_exists = ex.target_file_exists if ex.report
        fail ex
      end

      task = synthesize_file_task(task_name)
      if task
        # The rules did not match, but a file with the target name exists.
        # Record the resolution so the report does not read as a missing
        # target.  The rules themselves were already fully explored.
        report = self.last_rule_search_report
        report.resolved_by_file! if report
        return task
      end

      fail generate_message_for_undefined_task(task_name)
    end

    def generate_message_for_undefined_task(task_name)
      message = "Don't know how to build task '#{task_name}' "\
                "(See the list of available tasks with `#{Rake.application.name} --tasks`)"
      message + generate_did_you_mean_suggestions(task_name)
    end

    def generate_did_you_mean_suggestions(task_name)
      return "" unless defined?(::DidYouMean::SpellChecker)

      suggestions = ::DidYouMean::SpellChecker.new(dictionary: @tasks.keys).correct(task_name.to_s)
      if ::DidYouMean.respond_to?(:formatter)# did_you_mean v1.2.0 or later
        ::DidYouMean.formatter.message_for(suggestions)
      elsif defined?(::DidYouMean::Formatter) # before did_you_mean v1.2.0
        ::DidYouMean::Formatter.new(suggestions).to_s
      else
        ""
      end
    end

    def synthesize_file_task(task_name) # :nodoc:
      return nil unless File.exist?(task_name)
      define_task(Rake::FileTask, task_name)
    end

    # Resolve the arguments for a task/rule.  Returns a tuple of
    # [task_name, arg_name_list, prerequisites, order_only_prerequisites].
    def resolve_args(args)
      if args.last.is_a?(Hash)
        deps = args.pop
        resolve_args_with_dependencies(args, deps)
      else
        resolve_args_without_dependencies(args)
      end
    end

    # Resolve task arguments for a task or rule when there are no
    # dependencies declared.
    #
    # The patterns recognized by this argument resolving function are:
    #
    #   task :t
    #   task :t, [:a]
    #
    def resolve_args_without_dependencies(args)
      task_name = args.shift
      if args.size == 1 && args.first.respond_to?(:to_ary)
        arg_names = args.first.to_ary
      else
        arg_names = args
      end
      [task_name, arg_names, [], nil]
    end
    private :resolve_args_without_dependencies

    # Resolve task arguments for a task or rule when there are
    # dependencies declared.
    #
    # The patterns recognized by this argument resolving function are:
    #
    #   task :t, order_only: [:e]
    #   task :t => [:d]
    #   task :t => [:d], order_only: [:e]
    #   task :t, [a] => [:d]
    #   task :t, [a] => [:d], order_only: [:e]
    #
    def resolve_args_with_dependencies(args, hash) # :nodoc:
      fail "Task Argument Error" if
        hash.size != 1 &&
        (hash.size != 2 || !hash.key?(:order_only))
      order_only = hash.delete(:order_only)
      key, value = hash.map { |k, v| [k, v] }.first
      if args.empty?
        task_name = key
        arg_names = []
        deps = value || []
      else
        task_name = args.shift
        arg_names = key || args.shift|| []
        deps = value || []
      end
      deps = [deps] unless deps.respond_to?(:to_ary)
      [task_name, arg_names, deps, order_only]
    end
    private :resolve_args_with_dependencies

    # If a rule can be found that matches the task name, enhance the
    # task with the prerequisites and actions from the rule.  Set the
    # source attribute of the task appropriately for the rule.  Return
    # the enhanced task or nil if no rule was found.
    #
    # +report+ carries the bounded search state for the whole lookup.  A
    # nested resolution reuses the same report so that candidate counting,
    # cycle detection and depth control apply to the complete search rather
    # than to one recursion level.  +incoming_rule+ is the rule edge that
    # introduced +task_name+ (nil at the top level).  No task action is
    # executed here; only task objects and their dependency edges are
    # created.
    def enhance_with_matching_rule(task_name, level=0, report=nil, incoming_rule=nil)
      top_level = report.nil?
      report ||= begin
        new_report =
          RuleSearchReport.new(task_name, rule_search_budget,
                               rules_revision: rules_revision)
        self.last_rule_search_report = new_report
        new_report
      end

      # Share the in-flight report with any lookup performed by a rule
      # dependent, so a proc cannot start a parallel search with a fresh
      # budget.
      previous_active = current_rule_search_report
      self.current_rule_search_report = report
      begin
        if level >= report.budget.max_depth
          report.mark_depth_exceeded!(task_name, incoming_rule)
          error = RuleRecursionOverflowError.new("Rule Recursion Too Deep")
          error.report = report
          fail error
        end

        report.enter(task_name, incoming_rule) do
          @rules.each do |pattern, args, extensions, order_only, block|
            next unless pattern && pattern.match(task_name)
            task = attempt_rule(task_name, pattern, args, extensions, block, level, report)
            task | order_only unless order_only.nil?
            if task
              report.resolved!(:rule) if top_level
              return task
            end
          end
        end

        if top_level
          if report.cycle?
            report.mark_cycle!
            fail RuleCycleError.new(report, task_name)
          end
          report.not_found!
        end
        nil
      ensure
        self.current_rule_search_report = previous_active
      end
    rescue Rake::RuleRecursionOverflowError => ex
      ex.add_target(task_name)
      fail ex
    end

    # List of all defined tasks in this application.
    def tasks
      @tasks.values.sort_by { |t| t.name }
    end

    # List of all the tasks defined in the given scope (and its
    # sub-scopes).
    def tasks_in_scope(scope)
      prefix = scope.path
      tasks.select { |t|
        /^#{prefix}:/ =~ t.name
      }
    end

    # Clear all tasks in this application.
    def clear
      @tasks.clear
      @rules.clear
      @rules_revision += 1
      self.last_rule_search_report = nil
    end

    # Lookup a task, using scope and the scope hints in the task name.
    # This method performs straight lookups without trying to
    # synthesize file tasks or rules.  Special scope names (e.g. '^')
    # are recognized.  If no scope argument is supplied, use the
    # current scope.  Return nil if the task cannot be found.
    def lookup(task_name, initial_scope=nil)
      initial_scope ||= @scope
      task_name = task_name.to_s
      if task_name =~ /^rake:/
        scopes = Scope.make
        task_name = task_name.sub(/^rake:/, "")
      elsif task_name =~ /^(\^+)/
        scopes = initial_scope.trim($1.size)
        task_name = task_name.sub(/^(\^+)/, "")
      else
        scopes = initial_scope
      end
      lookup_in_scope(task_name, scopes)
    end

    # Lookup the task name
    def lookup_in_scope(name, scope)
      loop do
        tn = scope.path_with_task_name(name)
        task = @tasks[tn]
        return task if task
        break if scope.empty?
        scope = scope.tail
      end
      nil
    end
    private :lookup_in_scope

    # Return the list of scope names currently active in the task
    # manager.
    def current_scope
      @scope
    end

    # Evaluate the block in a nested namespace named +name+.  Create
    # an anonymous namespace if +name+ is nil.
    def in_namespace(name)
      name ||= generate_name
      @scope = Scope.new(name, @scope)
      ns = NameSpace.new(self, @scope)
      yield(ns)
      ns
    ensure
      @scope = @scope.tail
    end

    private

    # Per-thread slot for the in-flight rule search report.
    def thread_search_report_key
      @thread_search_report_key ||= :"rake_rule_search_report_#{object_id}"
    end

    # Per-thread slot for the most recently published report.
    def thread_last_report
      Thread.current[thread_last_report_key]
    end

    def thread_last_report_key
      @thread_last_report_key ||= :"rake_last_rule_search_report_#{object_id}"
    end

    # Add a location to the locations field of the given task.
    def add_location(task)
      loc = find_location
      task.locations << loc if loc
      task
    end

    # Find the location that called into the dsl layer.
    def find_location
      locations = caller
      i = 0
      while locations[i]
        return locations[i + 1] if locations[i] =~ /rake\/dsl_definition.rb/
        i += 1
      end
      nil
    end

    # Generate an anonymous namespace name.
    def generate_name
      @seed ||= 0
      @seed += 1
      "_anon_#{@seed}"
    end

    def trace_rule(level, message) # :nodoc:
      options.trace_output.puts "#{"    " * level}#{message}" if
        Rake.application.options.trace_rules
    end

    # Attempt to create a rule given the list of prerequisites.
    #
    # Every candidate source produced by the rule is accounted for in
    # +report+.  A candidate that exists on disk or as a task is accepted; a
    # candidate that can be expanded by another rule is accepted through the
    # nested lookup; anything else rejects this rule and lets the caller try
    # the next matching rule.  Candidates come only from the rule's own
    # dependent list (extensions, pathmaps and procs), never from scanning
    # the filesystem.
    def attempt_rule(task_name, task_pattern, args, extensions, block, level, report)
      sources = make_sources(task_name, task_pattern, extensions)
      prereqs = sources.map { |source|
        report.consume_candidate!(task_name, task_pattern, source, level)
        trace_rule level, "Attempting Rule #{task_name} => #{source}"
        if File.exist?(source)
          trace_rule level, "(#{task_name} => #{source} ... EXIST)"
          report.record_attempt(task_name, task_pattern, source, level,
                                :accepted, :existing_file)
          source
        elsif Rake::Task.task_defined?(source)
          trace_rule level, "(#{task_name} => #{source} ... EXIST)"
          report.record_attempt(task_name, task_pattern, source, level,
                                :accepted, :defined_task)
          source
        elsif report.cycle_target?(source)
          # Not a file and not a task, yet already on the search path: this
          # branch is a genuine rule cycle.  Reject it so a sibling rule can
          # still resolve the target, and remember the chain for the report.
          trace_rule level, "(#{task_name} => #{source} ... CYCLE)"
          report.note_cycle!(source, task_pattern)
          return nil
        elsif parent = enhance_with_matching_rule(source, level + 1, report, task_pattern)
          trace_rule level, "(#{task_name} => #{source} ... ENHANCE)"
          report.record_attempt(task_name, task_pattern, source, level,
                                :accepted, :synthesized_rule)
          parent.name
        else
          trace_rule level, "(#{task_name} => #{source} ... FAIL)"
          report.record_attempt(task_name, task_pattern, source, level,
                                :rejected, :missing_candidate)
          return nil
        end
      }
      task = FileTask.define_task(task_name, { args => prereqs }, &block)
      task.sources = prereqs
      task
    end

    # Make a list of sources from the list of file name extensions /
    # translation procs.
    def make_sources(task_name, task_pattern, extensions)
      result = extensions.map { |ext|
        case ext
        when /%/
          task_name.pathmap(ext)
        when %r{/}
          ext
        when /^\./
          source = task_name.sub(task_pattern, ext)
          source == ext ? task_name.ext(ext) : source
        when String, Symbol
          ext.to_s
        when Pathname
          Rake.from_pathname(ext)
        when Proc, Method
          if ext.arity == 1
            ext.call(task_name)
          else
            ext.call
          end
        else
          fail "Don't know how to handle rule dependent: #{ext.inspect}"
        end
      }
      result.flatten
    end

    # Return the current description, clearing it in the process.
    def get_description
      desc = @last_description
      @last_description = nil
      desc
    end

    class << self
      attr_accessor :record_task_metadata # :nodoc:
      TaskManager.record_task_metadata = false
    end
  end

end
