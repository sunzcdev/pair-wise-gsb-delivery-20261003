# frozen_string_literal: true

module AASM
  # Read-only static analysis of state machine definitions.
  #
  # AASM::DefinitionChecker.check(klass) inspects every state machine
  # registered for +klass+ (including inherited ones) without instantiating
  # the class, firing events, or evaluating guards, callbacks or dynamic
  # initial states. It reports:
  #
  # [errors]
  #   transitions which reference states that were never declared
  # [warnings]
  #   states that cannot be reached from the initial state by any declared
  #   transition (conditional edges count as passable)
  #   transitions that can never fire because an earlier unconditional
  #   transition of the same event always matches first
  #   machines whose dynamic initial state makes the reachability check
  #   inconclusive
  #
  # Issues are ordered by machine name, then event declaration order, then
  # transition declaration order, so reports are stable across runs.
  class DefinitionChecker
    class Issue
      attr_reader :severity, :machine_name, :event_name, :type, :message

      def initialize(severity, machine_name, type, message, event_name = nil)
        @severity = severity
        @machine_name = machine_name
        @event_name = event_name
        @type = type
        @message = message
      end

      def error?
        @severity == :error
      end

      def warning?
        @severity == :warning
      end

      def to_s
        location = "machine :#{machine_name}"
        location += ", event :#{event_name}" if event_name
        "#{severity.to_s.upcase} [#{type}] (#{location}) #{message}"
      end
    end

    class Report
      attr_reader :issues

      def initialize(issues)
        @issues = issues
      end

      def errors
        @issues.select(&:error?)
      end

      def warnings
        @issues.select(&:warning?)
      end

      def ok?
        errors.empty?
      end
    end

    def self.check(klass)
      new(klass).check
    end

    def initialize(klass)
      @klass = klass
    end

    def check
      issues = []
      store = AASM::StateMachineStore.fetch(@klass, true)
      return Report.new(issues) unless store

      store.machine_names.map(&:to_s).sort.each do |machine_name|
        check_machine(machine_name.to_sym, store.machine(machine_name), issues)
      end
      Report.new(issues)
    end

    private

    def check_machine(machine_name, machine, issues)
      declared = machine.states.map { |state| state.name.to_sym }

      machine.events.each_value do |event|
        check_state_references(machine_name, event, declared, issues)
        check_shadowed_transitions(machine_name, event, declared, issues)
      end

      check_reachability(machine_name, machine, declared, issues)
    end

    def check_state_references(machine_name, event, declared, issues)
      event.transitions.each do |transition|
        from = transition.from
        if from && !declared.include?(from.to_sym)
          issues << Issue.new(:error, machine_name, :undeclared_state,
            "transition #{describe(transition)} starts from undeclared state :#{from}",
            event.name)
        end
        Array(transition.to).compact.each do |to|
          next if declared.include?(to.to_sym)
          issues << Issue.new(:error, machine_name, :undeclared_state,
            "transition #{describe(transition)} targets undeclared state :#{to}",
            event.name)
        end
      end
    end

    def check_shadowed_transitions(machine_name, event, declared, issues)
      unconditional_froms = []
      covers_all_states = false

      event.transitions.each do |transition|
        candidate_froms = transition.from.nil? ? declared : [transition.from.to_sym]
        unless candidate_froms.empty?
          blocked = candidate_froms.select do |state|
            covers_all_states || unconditional_froms.include?(state)
          end
          if blocked.length == candidate_froms.length
            issues << Issue.new(:warning, machine_name, :shadowed_transition,
              "transition #{describe(transition)} can never fire; " \
              'an earlier unconditional transition of this event always matches first',
              event.name)
          end
        end

        next unless unconditional?(transition)
        if transition.from.nil?
          covers_all_states = true
        else
          unconditional_froms << transition.from.to_sym
        end
      end
    end

    def check_reachability(machine_name, machine, declared, issues)
      initial = machine.initial_state
      if initial.is_a?(Proc)
        issues << Issue.new(:warning, machine_name, :dynamic_initial_state,
          'initial state is determined dynamically at runtime; ' \
          'structural reachability cannot be verified statically')
        return
      end
      return if initial.nil?

      edges = Hash.new { |hash, key| hash[key] = [] }
      machine.events.each_value do |event|
        event.transitions.each do |transition|
          targets = Array(transition.to).compact.map(&:to_sym) & declared
          next if targets.empty?
          sources = transition.from.nil? ? declared : [transition.from.to_sym]
          sources.each do |source|
            targets.each { |target| edges[source] << target }
          end
        end
      end

      reached = {}
      queue = [initial.to_sym]
      until queue.empty?
        state = queue.shift
        next if reached[state]
        reached[state] = true
        queue.concat(edges[state])
      end

      declared.each do |state|
        next if reached[state]
        issues << Issue.new(:warning, machine_name, :unreachable_state,
          "state :#{state} cannot be reached from initial state :#{initial}")
      end
    end

    def unconditional?(transition)
      opts = transition.opts
      guards = Array(opts[:guards]) + Array(opts[:guard]) + Array(opts[:if])
      guards.empty? && Array(opts[:unless]).empty?
    end

    def describe(transition)
      from = transition.from.nil? ? 'any state' : ":#{transition.from}"
      to = Array(transition.to).map { |target| ":#{target}" }.join(', ')
      "(#{from} -> #{to})"
    end
  end
end
