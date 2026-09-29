# frozen_string_literal: true

module StateMachines
  # A TransitionPlan captures what *would* happen if an event were fired on
  # an object, without actually running the transition, its action, or any
  # of its callbacks.  Plans allow callers to inspect the starting state,
  # target state, selected branch, and the callback categories that would be
  # triggered, and then decide whether to go through with the transition.
  #
  # Plans are created via StateMachines::Event#plan_for (or the generated
  # <tt>{event}_plan</tt> instance helper) and executed via #execute.
  #
  # == Guards
  #
  # Guard conditions declared as precheckable (via the <tt>:precheck</tt>
  # option when defining transitions) are evaluated while the plan is built.
  # All other guards are recorded on the plan as deferred and are only
  # verified when the plan gets executed.
  #
  # == Staleness
  #
  # A plan is a snapshot of the object at planning time.  If the object's
  # state or any guard conditions change afterwards (e.g. another event is
  # fired on the object), the plan becomes stale.  Executing a stale plan
  # raises StateMachines::StalePlanError instead of forcing the outdated
  # transition through.
  #
  # == Example
  #
  #   plan = vehicle.ignite_plan
  #   plan.from_name       # => :parked
  #   plan.to_name         # => :idling
  #   plan.callback_types  # => [:before, :around, :after]
  #
  #   if plan.precheckable?
  #     plan.execute       # => performs the transition
  #   end
  class TransitionPlan
    # Describes a single guard condition on the branch selected by a plan.
    # Guards that were explicitly declared as precheckable (or state guards,
    # which only read state) are evaluated while planning; all others are
    # marked as deferred and re-verified when the plan is executed.
    class GuardCheck
      # The kind of guard, e.g. :if, :unless, :if_state, :unless_state
      attr_reader :kind

      # The guard condition (a symbol, proc, string, or state guard hash)
      attr_reader :condition

      # Creates a new guard check.  +passed+ should be nil for guards that
      # were not evaluated while planning.
      def initialize(kind, condition, precheckable, passed = nil) # :nodoc:
        @kind = kind
        @condition = condition
        @precheckable = precheckable
        @passed = passed
      end

      # Whether this guard was declared safe to evaluate while planning
      def precheckable?
        @precheckable
      end

      # Whether this guard was actually evaluated while planning
      def checked?
        !@passed.nil?
      end

      # Whether this guard passed.  Returns nil for guards that could not be
      # safely pre-checked and were deferred until execution.
      def passed?
        @passed
      end

      def inspect
        "#<#{self.class} kind=#{kind.inspect} condition=#{condition.inspect} precheckable=#{precheckable?} passed=#{passed?.inspect}>"
      end
    end

    # The object the transition was planned for
    attr_reader :object

    # The state machine for which this plan was created
    attr_reader :machine

    # The event this plan was created for
    attr_reader :event

    # The branch that was selected when the plan was built
    attr_reader :branch

    # The state name the transition would start from
    attr_reader :from_name

    # The state name the transition would end in
    attr_reader :to_name

    # The original state value when the plan was created
    attr_reader :from

    # The state value the transition would persist
    attr_reader :to

    # The checks describing each guard condition on the selected branch
    attr_reader :guards

    # The requirements the plan was built with (e.g. a pinned :from state)
    attr_reader :requirements

    # The event arguments the plan was built with
    attr_reader :event_args

    def initialize(object, machine, event, from_name, to_name, branch, guards, requirements = {}, event_args = [], read_state = true) # :nodoc:
      @object = object
      @machine = machine
      @event = event
      @branch = branch
      @from_name = from_name
      @to_name = to_name
      @from_state = machine.states.fetch(from_name)
      @from = read_state ? machine.read(object, :state) : @from_state.value
      @to = machine.states.fetch(to_name).value
      @guards = guards.freeze
      @requirements = requirements.dup.freeze
      @event_args = event_args.freeze
    end

    # The name of the event this plan was created for
    def event_name
      event.name
    end

    # The categories of callbacks the transition would pass through if it
    # were performed, in execution order.  Possible values are :before,
    # :around, and :after.  Callbacks are matched on the transition context
    # only; no callback (or callback condition) is ever run while planning.
    def callback_types
      @callback_types ||= begin
        context = { on: event_name, from: from_name, to: to_name, guard: false }
        types = machine.callbacks[:before].select { |callback| callback.branch.matches?(object, context) }.map(&:type)
        types << :after if machine.callbacks[:after].any? { |callback| callback.branch.matches?(object, context) }
        %i[before around after] & types
      end
    end

    # Whether every guard on the selected branch could be pre-checked while
    # planning.  If false, one or more guards were deferred and will only be
    # verified when the plan is executed.
    def precheckable?
      guards.all?(&:precheckable?)
    end

    # The guard checks that could not be safely pre-checked while planning
    # and will only be verified when the plan is executed
    def deferred_guards
      guards.reject(&:precheckable?)
    end

    # Whether the object's current state no longer matches the state the
    # plan was built from
    def stale?
      machine.read(object, :state) != from
    end

    # Re-verifies that the plan can still be executed: the object must still
    # be in the planned starting state and all guards (including deferred
    # ones) must still pass, resolving to the planned target state.
    def valid?
      !resolve.nil?
    end

    # Re-verifies this plan and, if it is still valid, performs the planned
    # transition (including its action and callbacks).  Raises
    # StateMachines::StalePlanError if the object's state or requirements
    # changed since the plan was created.
    #
    # Any arguments are passed through to StateMachines::Transition#perform.
    # If no arguments are given, the arguments the plan was built with are
    # used instead.
    def execute(*args)
      machine.reset(object)

      transition = resolve
      raise StalePlanError, self unless transition

      transition.perform(*(args.empty? ? event_args : args))
    end

    # Generates a nicely formatted description of this plan's contents.
    #
    # For example,
    #
    #   plan = vehicle.ignite_plan
    #   plan   # => #<StateMachines::TransitionPlan event=:ignite from_name=:parked to_name=:idling precheckable=true>
    def inspect
      "#<#{self.class} event=#{event_name.inspect} from_name=#{from_name.inspect} to_name=#{to_name.inspect} precheckable=#{precheckable?}>"
    end

    private

    # Resolves the transition that would be performed *now* for the planned
    # event and requirements.  Returns nil if no transition is available or
    # it no longer matches the planned starting / target states.
    def resolve
      # The object must still be in the state the plan was built from
      return if stale?

      transition = event.transition_for(object, requirements.dup, *event_args)
      transition if transition && transition.from_name == from_name && transition.to_name == to_name
    end
  end
end
