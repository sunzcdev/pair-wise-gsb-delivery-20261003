# frozen_string_literal: true
module Rake

  ##
  # A read-only description of the task graph Rake would walk for a set of
  # targets.  Building a plan never invokes a task action, never marks a task
  # as invoked and never clears or replaces a previously recorded invocation
  # failure.
  #
  # Names are resolved through the real task manager, so namespace lookup,
  # implicit rule expansion and file task synthesis behave like they do during
  # an actual invocation.  Dependencies are reported by task identity (the
  # resolved task name), so two spellings that resolve to the same task
  # collapse into a single edge.
  #
  # Implicit rules whose sources are plain names or extensions are expanded
  # completely.  Rules whose sources come from a Proc or Method are arbitrary
  # Ruby and are reported as unknown instead of being called.  Likewise, tasks
  # created later by other Ruby code are not predicted.
  class ExecutionPlan
    # A single task in the plan.  +prerequisites+ and
    # +order_only_prerequisites+ hold resolved task identities (task names),
    # not the raw strings from the Rakefile.
    class Node
      attr_accessor :name, :type, :prerequisites, :order_only_prerequisites,
                    :derivation, :action_count, :already_invoked, :dynamic,
                    :duplicate_prerequisites, :unresolved_prerequisites,
                    :unknown_prerequisites, :depth, :deferred_rule

      def initialize(name)
        @name = name
        @type = :task
        @prerequisites = []
        @order_only_prerequisites = []
        @duplicate_prerequisites = []
        @unresolved_prerequisites = []
        @unknown_prerequisites = []
        @derivation = nil
        @deferred_rule = nil
        @action_count = 0
        @already_invoked = false
        @dynamic = false
        @depth = 0
      end

      # :unknown when part of the task depends on dynamic Ruby, :unresolved
      # when a declared dependency could not be resolved, otherwise :ok.
      def status
        return :unknown if dynamic || !unknown_prerequisites.empty?
        return :unresolved unless unresolved_prerequisites.empty?
        :ok
      end

      # Snapshot of this node as a Hash.
      def to_h
        {
          name: name,
          type: type,
          prerequisites: prerequisites.dup,
          order_only_prerequisites: order_only_prerequisites.dup,
          derivation: derivation && derivation.dup,
          deferred_rule: deferred_rule && deferred_rule.dup,
          action_count: action_count,
          already_invoked: already_invoked,
          dynamic: dynamic,
          depth: depth,
          status: status,
          duplicate_prerequisites: duplicate_prerequisites.map(&:dup),
          unresolved_prerequisites: unresolved_prerequisites.map(&:dup),
          unknown_prerequisites: unknown_prerequisites.map(&:dup),
        }
      end

      def inspect # :nodoc:
        "#<#{self.class} #{name} [#{type}] #{prerequisites.inspect}>"
      end
    end

    # What the plan deliberately does not claim to know.
    LIMITATIONS = [
      "The plan only covers tasks and rules that exist when it is built. " \
        "Tasks created later by arbitrary Ruby code, including inside task " \
        "actions, are not predicted.",
      "Rule sources and dependencies written as Proc or Method are arbitrary " \
        "Ruby.  They are never called while planning: the affected task is " \
        "marked dynamic and the sources stay unknown.",
      "Building a plan runs no task action, leaves already_invoked unchanged " \
        "and neither clears nor replaces a previous invocation failure.",
    ].freeze

    # Names requested by the caller, in order.
    attr_reader :target_names

    # One entry per requested target:
    #   { requested:, name:, status:, message: }
    attr_reader :targets

    # Reachable tasks keyed by task name.
    attr_reader :nodes

    # Readable cycle descriptions, e.g. "a => b => a".
    attr_reader :cycles

    # Dependencies that could not be resolved:
    #   { name:, required_by:, message: }
    attr_reader :unresolved

    # Parts of the graph that depend on arbitrary Ruby:
    #   { name:, required_by:, reason: }
    attr_reader :unknown

    class << self
      # Build a plan for +target_names+ using +task_manager+.
      def build(task_manager, target_names)
        Builder.new(task_manager).build(target_names)
      end
    end

    def initialize
      @target_names = []
      @targets = []
      @nodes = {}
      @ordered = []
      @cycles = []
      @unresolved = []
      @unknown = []
    end

    # True when every target and dependency was determined and no cycle was
    # found.  Dynamic Ruby makes the plan incomplete rather than wrong.
    def ok?
      return false unless unresolved.empty? && unknown.empty? && cycles.empty?
      targets.all? { |target| target[:status] == :ok }
    end

    # Look up a planned node by task name.
    def [](name)
      nodes[name.to_s]
    end

    # Planned nodes in execution order (prerequisites before dependents).
    def each_node(&block)
      return enum_for(:each_node) unless block
      @ordered.each(&block)
    end

    # Human readable plan.  No task action is ever described as executed.
    def to_s
      lines = []
      lines << "Execution plan (no task actions run)"
      lines << "status: #{ok? ? "ok" : "incomplete"}"
      lines << "targets:"
      if targets.empty?
        lines << "  (none)"
      else
        targets.each { |target| lines << target_line(target) }
      end

      lines << "nodes:"
      if @ordered.empty?
        lines << "  (none)"
      else
        @ordered.each { |node| append_node(lines, node) }
      end

      lines << "cycles:"
      if cycles.empty?
        lines << "  (none)"
      else
        cycles.each { |cycle| lines << "  #{cycle}" }
      end

      lines << "unresolved:"
      append_entries(lines, unresolved)

      lines << "unknown (dynamic Ruby):"
      append_entries(lines, unknown, include_reason: true)

      lines << "limitations:"
      LIMITATIONS.each { |limitation| lines << "  - #{limitation}" }
      lines.join("\n")
    end

    # Snapshot of the whole plan as nested Hashes.
    def to_h
      {
        status: ok? ? "ok" : "incomplete",
        targets: targets.map(&:dup),
        nodes: @ordered.map(&:to_h),
        cycles: cycles.dup,
        unresolved: unresolved.map(&:dup),
        unknown: unknown.map(&:dup),
        limitations: LIMITATIONS.dup,
      }
    end

    # :stopdoc:
    # Internal construction API used by Builder.

    def add_target(entry) # :nodoc:
      @targets << entry
    end

    def add_node(node) # :nodoc:
      @nodes[node.name] = node
    end

    def append_ordered(node) # :nodoc:
      @ordered << node
    end

    def add_cycle(description) # :nodoc:
      @cycles << description unless @cycles.include?(description)
    end

    def add_unresolved(entry) # :nodoc:
      @unresolved << entry
    end

    def add_unknown(entry) # :nodoc:
      @unknown << entry
    end

    def target_names=(names) # :nodoc:
      @target_names = names
    end
    # :startdoc:

    private

    def target_line(target)
      description = "  #{target[:requested]}"
      description += " => #{target[:name]}" if target[:name] != target[:requested]
      case target[:status]
      when :ok
        description
      when :unknown
        "#{description} [unknown: #{target[:message]}]"
      else
        "#{description} [unresolved: #{target[:message]}]"
      end
    end

    def append_entries(lines, entries, include_reason: false)
      if entries.empty?
        lines << "  (none)"
        return
      end
      entries.each do |entry|
        line = "  #{entry[:name]} (required by #{entry[:required_by]})"
        line += ": #{entry[:reason] || entry[:message]}" if include_reason
        lines << line
      end
    end
    def append_node(lines, node)
      header = "  #{node.name} [#{node.type}]"
      header += " #{derivation_text(node.derivation)}" if node.derivation
      header += " actions=#{node.action_count}"
      header += " already_invoked=true" if node.already_invoked
      header += " dynamic=true" if node.dynamic
      lines << header
      details = "    "
      lines << "#{details}prerequisites: #{node.prerequisites.join(", ")}" unless node.prerequisites.empty?
      unless node.order_only_prerequisites.empty?
        lines << "#{details}order-only: #{node.order_only_prerequisites.join(", ")}"
      end
      unless node.duplicate_prerequisites.empty?
        duplicates = node.duplicate_prerequisites.map { |dup| "#{dup[:name]} => #{dup[:identity]}" }
        lines << "#{details}duplicates: #{duplicates.join(", ")}"
      end
      if (rule = node.deferred_rule)
        text = "#{details}deferred rule: #{rule[:pattern]}"
        text += " => #{rule[:sources].join(", ")}" unless rule[:sources].empty?
        text += " (order-only: #{rule[:order_only].join(", ")})" unless rule[:order_only].empty?
        text += " (dynamic)" if rule[:dynamic]
        text += " (unresolved: #{rule[:error]})" if rule[:error]
        lines << text
      end
      node.unresolved_prerequisites.each do |entry|
        lines << "#{details}unresolved: #{entry[:name]} (#{entry[:message]})"
      end
      node.unknown_prerequisites.each do |entry|
        lines << "#{details}unknown: #{entry[:name]} (#{entry[:reason]})"
      end
    end

    def derivation_text(derivation)
      case derivation[:kind]
      when :rule
        text = "rule #{derivation[:pattern]} => #{Array(derivation[:extensions]).join(", ")}"
        text += " (dynamic sources)" if derivation[:dynamic]
        text
      when :existing_file
        "existing file"
      else
        "defined"
      end
    end

    # Walks the task graph through the real task manager without invoking it.
    class Builder
      # What a name resolved to: a real task, or an unknown dynamic rule.
      Resolution = Struct.new(:task, :rule, :name) do
        def dynamic?
          task.nil?
        end
      end

      def initialize(task_manager)
        @task_manager = task_manager
        @plan = ExecutionPlan.new
        @path = []
      end

      def build(target_names)
        names = Array(target_names).map { |name| Rake.from_pathname(name).to_s }
        @plan.target_names = names
        names.each { |name| resolve_target(name) }
        @plan
      end

      private

      def resolve_target(name)
        resolution = resolve(name, nil)
        if resolution.dynamic?
          reason = rule_error(resolution)
          @plan.add_target(requested: name, name: name, status: :unknown, message: reason)
          @plan.add_unknown(name: name, required_by: "target", reason: reason)
          return
        end
        @plan.add_target(requested: name, name: resolution.task.name, status: :ok)
        visit(resolution.task, 0)
      rescue StandardError => ex
        @plan.add_target(requested: name, name: name, status: :unresolved, message: ex.message)
        @plan.add_unresolved(name: name, required_by: "target", message: ex.message)
      end

      def visit(task, depth)
        if (index = @path.index(task.name))
          @plan.add_cycle((@path[index..-1] + [task.name]).join(" => "))
          return
        end
        return if @plan.nodes.key?(task.name)

        @path.push(task.name)
        node = build_node(task, depth)
        @plan.add_node(node)

        dependencies = []
        identities = {}
        resolve_dependencies(task, task.prerequisites, node, node.prerequisites, identities, dependencies)
        resolve_dependencies(task, task.order_only_prerequisites, node,
          node.order_only_prerequisites, identities, dependencies)

        dependencies.each { |dependency| visit(dependency, depth + 1) }

        @path.pop
        @plan.append_ordered(node)
      end

      def build_node(task, depth)
        node = Node.new(task.name)
        node.type = task_type(task)
        node.action_count = task.actions.length
        node.already_invoked = task.already_invoked
        node.depth = depth
        node.derivation = derivation_for(task)
        node.dynamic = !!(node.derivation && node.derivation[:dynamic])
        add_unknown_node(node, task) if node.dynamic
        apply_deferred_rule(task, node) if task.actions.empty?
        node
      end

      # A rule sourced from a Proc or Method is arbitrary Ruby.  Report it
      # without calling the proc; its sources stay unknown.
      def add_unknown_node(node, task)
        node.unknown_prerequisites << {
          name: task.name, required_by: task.name,
          reason: "sources computed by Ruby code",
        }
        @plan.add_unknown(name: task.name, required_by: task.name,
          reason: "sources computed by Ruby code")
      end

      # A task without actions may still match a rule when it is executed
      # (Task#execute calls enhance_with_matching_rule).  Report that rule
      # without defining or enhancing anything.
      def apply_deferred_rule(task, node)
        record = @task_manager.preview_rule(task.name)
        return unless record
        node.deferred_rule = record
        return unless record[:dynamic]
        node.dynamic = true
        reason = record[:error] || "sources computed by Ruby code"
        node.unknown_prerequisites << { name: task.name, required_by: task.name, reason: reason }
        @plan.add_unknown(name: task.name, required_by: task.name, reason: reason)
      end

      def resolve_dependencies(task, raw_names, node, list, identities, dependencies)
        raw_names.each do |raw|
          raw_name = Rake.from_pathname(raw).to_s
          begin
            resolution = resolve_prerequisite(task, raw)
          rescue StandardError => ex
            record_unresolved(task, raw_name, list, node, ex)
            next
          end

          if resolution.dynamic?
            reason = rule_error(resolution)
            node.unknown_prerequisites << { name: raw_name, required_by: task.name, reason: reason }
            @plan.add_unknown(name: raw_name, required_by: task.name, reason: reason)
            next
          end

          dependency = resolution.task
          if identities.key?(dependency.name)
            node.duplicate_prerequisites << { name: raw_name, identity: dependency.name }
          else
            identities[dependency.name] = true
            list << dependency.name
            dependencies << dependency
          end
        end
      end

      def rule_error(resolution)
        (resolution.rule && resolution.rule[:error]) || "sources computed by Ruby code"
      end

      # Mirror Task#lookup_prerequisite: prefer the lookup scoped to the
      # dependent, but fall back to the unscoped lookup when that would return
      # the dependent itself.
      def resolve_prerequisite(task, raw)
        scoped = resolve(raw, task.scope)
        return scoped if scoped.dynamic?
        return scoped unless scoped.task.equal?(task)
        resolve(raw, nil)
      end

      def record_unresolved(task, raw_name, list, node, exception)
        entry = { name: raw_name, required_by: task.name, message: exception.message }
        @plan.add_unresolved(entry)
        node.unresolved_prerequisites << entry
        list << raw_name unless list.include?(raw_name)
      end

      # Resolve a name without running arbitrary Ruby.  A stored task is used
      # as is; an implicit rule with constant sources is expanded through the
      # real task manager; a rule with a Proc/Method dependent is reported as
      # dynamic instead of being called.
      def resolve(name, scope)
        task = @task_manager.lookup(name, scope)
        return Resolution.new(task) if task

        preview = @task_manager.preview_rule(name)
        if preview && preview[:dynamic]
          return Resolution.new(nil, preview, name)
        end

        Resolution.new(@task_manager[name, scope])
      end

      def task_type(task)
        case task
        when Rake::FileCreationTask
          :file_creation_task
        when Rake::MultiTask
          :multi_task
        when Rake::FileTask
          :file_task
        else
          task.class == Rake::Task ? :task : task.class.name
        end
      end

      def derivation_for(task)
        record = @task_manager.rule_derivations[task.name]
        return { kind: :defined } unless record
        copy = record.dup
        copy[:extensions] = record[:extensions].dup if record[:extensions]
        copy
      end
    end
  end
end
