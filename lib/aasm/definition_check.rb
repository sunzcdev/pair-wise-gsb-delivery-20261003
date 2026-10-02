# frozen_string_literal: true

module AASM
  # Read-only static analysis of a state machine definition.
  #
  # The check never instantiates the owning class and never evaluates guards,
  # dynamic initial states or callbacks. It only reads the declared states,
  # events and transitions and reports structural problems with the definition.
  #
  # Two severities are distinguished:
  #
  # * +:error+   - the definition references something that is not declared
  #   and would fail at runtime.
  # * +:warning+ - the definition is self-consistent but contains dead
  #   structure (unreachable states) or candidates that an earlier
  #   unconditional transition always shadows.
  #
  # Guarded transitions are treated as edges that *may* be taken, so they are
  # neither considered blockers nor proof of unreachability.
  module DefinitionCheck
    # A single finding. +severity+ is either +:error+ or +:warning+, +code+
    # is a stable symbol identifying the kind of finding and the remaining
    # attributes locate it within the definition.
    Issue = Struct.new(:severity, :code, :machine, :state, :event,
                       :transition_index, :from, :to, :message,
                       keyword_init: true) do
      def error?
        severity == :error
      end

      def warning?
        severity == :warning
      end

      def to_s
        "#{severity.to_s.upcase} [#{code}] #{message}"
      end
    end

    # Result of analysing one or more state machines. Issues are stored in a
    # stable order (machine name, then event/transition/state declaration
    # order) and are never sorted further.
    class Report
      attr_reader :issues

      def initialize(issues = [])
        @issues = issues
      end

      def errors
        issues.select(&:error?)
      end

      def warnings
        issues.select(&:warning?)
      end

      def ok?
        issues.none?(&:error?)
      end

      def empty?
        issues.empty?
      end

      def merge(other)
        Report.new(issues + other.issues)
      end

      def by_machine(machine_name)
        Report.new(issues.select { |issue| issue.machine == machine_name.to_sym })
      end

      def to_s
        issues.map(&:to_s).join("\n")
      end
    end

    class << self
      # Analyse a single state machine attached to +klass+. When +machine_name+
      # is omitted the +:default+ machine is used. This never instantiates
      # +klass+ and never runs guards, dynamic initial states or callbacks.
      def check(klass, machine_name = :default)
        machine_name = machine_name.to_sym
        store = AASM::StateMachineStore.fetch(klass, false)
        machine = store && store.machine(machine_name)
        return Report.new unless machine

        Checker.new(machine).report
      end

      # Analyse every state machine attached to +klass+. Machines are reported
      # in name order for stable output.
      def check_all(klass)
        store = AASM::StateMachineStore.fetch(klass, false)
        return Report.new unless store

        store.machine_names.sort.reduce(Report.new) do |report, name|
          report.merge(check(klass, name))
        end
      end
    end

    # Performs the analysis of a single AASM::StateMachine.
    class Checker
      def initialize(machine)
        @machine = machine
        @name = machine.name
        @issues = []
      end

      def report
        check_transitions
        check_initial_state
        check_reachability
        Report.new(@issues)
      end

      private

      def declared_states
        @declared_states ||= @machine.states.map(&:name)
      end

      def declared_state?(state_name)
        state_name && declared_states.include?(state_name)
      end

      # Yields every transition in event declaration order followed by
      # transition declaration order within each event.
      def each_transition
        @machine.events.each_value do |event|
          event.transitions.each_with_index do |transition, index|
            yield event, transition, index
          end
        end
      end

      def each_from(transition)
        if transition.from.nil?
          declared_states
        else
          Array(transition.from)
        end
      end

      def each_to(transition)
        to = transition.to
        return [] if to.nil?
        Array(to)
      end

      # Pass 1: transitions must only reference declared states, and a fully
      # shadowed candidate is dead structure.
      def check_transitions
        each_transition do |event, transition, index|
          check_transition_states(event, transition, index)
          check_shadowing(event, transition, index)
        end
      end

      def check_transition_states(event, transition, index)
        unless transition.from.nil? || declared_state?(transition.from)
          add_issue(
            :error, :unknown_from_state,
            :event => event.name, :transition_index => index,
            :from => transition.from, :to => transition.to,
            :message => "machine '#{@name}' event ':#{event.name}' transition ##{index + 1} " \
                        "starts from undeclared state ':#{transition.from}'"
          )
        end

        each_to(transition).each do |target|
          next if declared_state?(target)

          add_issue(
            :error, :unknown_to_state,
            :event => event.name, :transition_index => index,
            :from => transition.from, :to => target,
            :message => "machine '#{@name}' event ':#{event.name}' transition ##{index + 1} " \
                        "targets undeclared state ':#{target}'"
          )
        end
      end

      # A candidate is dead only if a single earlier unconditional transition
      # applies to every state the candidate may start from. Guarded earlier
      # transitions may or may not pass and therefore never shadow anything,
      # and a candidate that can still be reached from at least one of its
      # declared origins is not reported.
      def check_shadowing(event, transition, index)
        return if transition.guarded?

        applicable_origins = each_from(transition).select { |origin| declared_state?(origin) }
        return if applicable_origins.empty?

        earlier = event.transitions[0...index]
        blocker = earlier.find do |candidate|
          next false if candidate.guarded?

          applicable_origins.all? { |origin| applies_unconditionally?(candidate, origin) }
        end
        return unless blocker

        blocker_index = event.transitions.index(blocker)
        add_issue(
          :warning, :shadowed_transition,
          :event => event.name, :transition_index => index,
          :from => transition.from, :to => transition.to,
          :message => "machine '#{@name}' event ':#{event.name}' transition ##{index + 1} " \
                      "is fully shadowed by earlier unconditional transition ##{blocker_index + 1}"
        )
      end

      def applies_unconditionally?(candidate, origin)
        return false if candidate.guarded?

        candidate.from.nil? || Array(candidate.from).include?(origin)
      end

      # Pass 2: the initial state must itself be declared.
      def check_initial_state
        initial = @machine.initial_state
        return if initial.nil? || dynamic_initial_state?
        return if declared_state?(initial)

        add_issue(
          :error, :unknown_initial_state,
          :state => initial,
          :message => "machine '#{@name}' declares initial state ':#{initial}' which is not a declared state"
        )
      end

      def dynamic_initial_state?
        initial = @machine.initial_state
        initial.respond_to?(:call)
      end

      # Pass 3: states that cannot be reached from the initial state. Guarded
      # edges count as possible paths, and transitions without a +:from+ apply
      # from every declared state.
      def check_reachability
        initial = @machine.initial_state
        if dynamic_initial_state?
          add_issue(
            :warning, :dynamic_initial_state,
            :message => "machine '#{@name}' uses a dynamic initial state; " \
                        "unreachable-state analysis could not be performed statically"
          )
          return
        end
        return if initial.nil? || !declared_state?(initial)

        reachable = {}
        frontier = [initial]
        until frontier.empty?
          state = frontier.pop
          next if reachable[state]

          reachable[state] = true
          outgoing_targets(state).each { |target| frontier << target }
        end

        declared_states.each do |state|
          next if reachable[state]

          add_issue(
            :warning, :unreachable_state,
            :state => state,
            :message => "machine '#{@name}' state ':#{state}' is not reachable from the initial state ':#{initial}'"
          )
        end
      end

      def outgoing_targets(origin)
        @machine.events.each_value.flat_map do |event|
          event.transitions.flat_map do |transition|
            next [] unless transition.from.nil? || Array(transition.from).include?(origin)

            each_to(transition).select { |target| declared_state?(target) }
          end
        end
      end

      def add_issue(severity, code, attributes = {})
        @issues << Issue.new(
          {
            :severity => severity,
            :code => code,
            :machine => @name,
            :state => nil,
            :event => nil,
            :transition_index => nil,
            :from => nil,
            :to => nil,
            :message => nil,
          }.merge(attributes)
        )
      end
    end
  end
end
