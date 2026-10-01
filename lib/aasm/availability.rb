# frozen_string_literal: true

module AASM
  # Snapshot of which events and transitions are currently available on an
  # object. Returned by AASM::InstanceBase#check.
  #
  # Unlike calling +events(permitted: true)+, +states(permitted: true)+ and
  # +permitted_transitions+ separately, this result is produced by a single
  # pass over the candidates: every guard is executed at most once, even when
  # it is shared between several candidates, so the event, transition and
  # rejection lists can never disagree about guards that count calls or read
  # external state.
  #
  # The result is a presentation-only snapshot. It is not stored on the object
  # and is never consulted when an event is fired, so guards are always run
  # again against the object's current attributes.
  class Availability
    # Evaluation of a single guard within a candidate transition.
    #
    # +status+ is +:passed+, +:failed+ or +:not_run+. A guard shared by
    # several candidates is executed only once during the check: every later
    # occurrence keeps the shared outcome while reporting #invoked? as false.
    # Guards skipped by short-circuit evaluation are +:not_run+, they are
    # never assumed to have failed.
    GuardCheck = Struct.new(:subject, :kind, :status, :invoked, keyword_init: true) do
      def passed?
        status == :passed
      end

      def failed?
        status == :failed
      end

      def not_run?
        status == :not_run
      end

      def invoked?
        invoked
      end
    end

    # One candidate transition: an event paired with one concrete target
    # state. Transitions declared with an array of +:to+ states are expanded
    # into one check per target state, sharing the same guard results.
    TransitionCheck = Struct.new(:event, :from, :to, :guards, keyword_init: true) do
      def allowed?
        guards.none?(&:failed?)
      end

      def blocked?
        !allowed?
      end

      # The GuardCheck entries that prevented this transition, in evaluation
      # order. Empty when the transition is allowed.
      def blocked_by
        guards.select(&:failed?)
      end
    end

    # One candidate event together with all of its candidate transitions.
    EventCheck = Struct.new(:name, :event, :transitions, keyword_init: true) do
      def allowed?
        transitions.any?(&:allowed?)
      end

      def blocked?
        !allowed?
      end

      def blocked_transitions
        transitions.select(&:blocked?)
      end
    end

    attr_reader :state_machine_name, :current_state, :events

    def initialize(state_machine_name, current_state, event_checks, states)
      @state_machine_name = state_machine_name
      @current_state = current_state
      @events = event_checks
      @states_by_name = states.each_with_object({}) { |state, map| map[state.name] = state }
    end

    # The full EventCheck for the given event name, or nil when the event is
    # unknown or has no transition from the current state.
    def event(name)
      @events.find { |event_check| event_check.name == name.to_sym }
    end

    # True when at least one candidate transition of the event is allowed.
    # Unknown events and events without a candidate transition return false.
    def may_fire?(event_name)
      event_check = event(event_name)
      !!event_check&.allowed?
    end

    # Every candidate transition check, allowed and blocked, in event and
    # transition definition order.
    def transitions
      @events.flat_map(&:transitions)
    end

    # Event objects for which at least one candidate transition is allowed.
    def allowed_events
      @events.select(&:allowed?).map(&:event)
    end

    # Event objects for which every candidate transition is blocked.
    def blocked_events
      @events.select(&:blocked?).map(&:event)
    end

    # {event: event_name, state: target_state} pairs for every allowed
    # candidate transition. Mirrors the shape of InstanceBase#permitted_transitions.
    def allowed_transitions
      transitions.select(&:allowed?).map do |transition_check|
        {event: transition_check.event.name, state: transition_check.to}
      end
    end

    # TransitionCheck entries for every candidate transition that is blocked.
    def blocked_transitions
      transitions.select(&:blocked?)
    end

    # State objects reachable through an allowed transition, de-duplicated and
    # in the order the targets are first encountered.
    def allowed_states
      target_names = allowed_transitions.map { |pair| pair[:state] }.uniq
      target_names.map { |name| @states_by_name[name] }.compact
    end

    # Rejection reasons keyed by event name. Each value is the ordered list of
    # blocked TransitionCheck entries for that event; use #blocked_by on them
    # to inspect the guards that failed.
    def rejections
      @events.select(&:blocked?).each_with_object({}) do |event_check, memo|
        memo[event_check.name] = event_check.blocked_transitions
      end
    end
  end

  module Core
    # Performs a single read-only pass over all candidate events of a state
    # machine and evaluates their guards. A new instance is created for every
    # Availability check, so evaluation state never leaks between objects or
    # between consecutive checks.
    class AvailabilityChecker
      def initialize(instance, state_machine, *args)
        @instance = instance
        @state_machine = state_machine
        @args = args
        @outcomes = {}
      end

      def result
        current_state = @instance.aasm(@state_machine.name).current_state

        event_checks = @state_machine.events.values.map do |event|
          candidates = event.transitions_from_state(current_state)
          next if candidates.empty?

          transition_checks = candidates.flat_map do |transition|
            build_transition_checks(event, transition)
          end

          Availability::EventCheck.new(
            name: event.name,
            event: event,
            transitions: transition_checks
          )
        end.compact

        Availability.new(
          @state_machine.name,
          current_state,
          event_checks,
          @state_machine.states
        )
      end

      private

      # Evaluates the :if chain first and then the :unless chain, mirroring
      # Transition#allowed?. Each chain short-circuits at the first failing
      # guard and marks every remaining guard in that chain as not run. When
      # the :if chain fails the whole :unless chain is skipped.
      def build_transition_checks(event, transition)
        prepare_guard_context(transition)

        if_checks = evaluate_chain(transition.guards, :if)
        if if_checks.any?(&:failed?)
          unless_checks = transition.unless_guards.map do |subject|
            not_run_check(subject, :unless)
          end
        else
          unless_checks = evaluate_chain(transition.unless_guards, :unless)
        end

        guard_checks = if_checks + unless_checks

        Array(transition.to).map do |target_state|
          Availability::TransitionCheck.new(
            event: event,
            from: transition.from,
            to: target_state,
            guards: guard_checks
          )
        end
      end

      def evaluate_chain(subjects, kind)
        subjects.each_with_object([]) do |subject, checks|
          checks << if checks.any?(&:failed?)
                      not_run_check(subject, kind)
                    else
                      evaluate_guard(subject, kind)
                    end
        end
      end

      # Returns the cached outcome when the guard has already been executed by
      # another candidate during this check. The shared GuardCheck reports the
      # cached status with invoked? == false, since this occurrence did not
      # execute the guard itself.
      def evaluate_guard(subject, kind)
        cache_key = [kind, subject]
        if @outcomes.key?(cache_key)
          return Availability::GuardCheck.new(
            subject: subject,
            kind: kind,
            status: @outcomes[cache_key],
            invoked: false
          )
        end

        truthy = invoke_guard(subject, kind)
        passed = kind == :if ? truthy : !truthy
        status = passed ? :passed : :failed
        @outcomes[cache_key] = status

        Availability::GuardCheck.new(
          subject: subject,
          kind: kind,
          status: status,
          invoked: true
        )
      end

      def invoke_guard(subject, kind)
        options = kind == :if ? {guard: true} : {unless: true}
        result = Invoker.new(subject, @instance, @args)
                     .with_options(options)
                     .invoke
        !!result
      end

      def not_run_check(subject, kind)
        Availability::GuardCheck.new(
          subject: subject,
          kind: kind,
          status: :not_run,
          invoked: false
        )
      end

      # Mirrors Transition#invoke_callbacks_compatible_with_guard so guards
      # observe the same from/to metadata as during the regular may_fire? path.
      def prepare_guard_context(transition)
        aasm = @instance.aasm(@state_machine.name)
        aasm.from_state = transition.from if aasm.respond_to?(:from_state=)
        aasm.to_state = transition.to if aasm.respond_to?(:to_state=)
      end
    end
  end
end
