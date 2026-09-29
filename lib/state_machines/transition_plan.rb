# frozen_string_literal: true

module StateMachines
  # A transition plan captures what *would* happen if an event were fired on
  # an object, without actually running the transition's action or any of its
  # callbacks.  Plans allow callers to inspect the outcome of an event (the
  # states involved, the branch selected, and the kinds of callbacks that
  # would run) before deciding whether to go through with it.
  #
  # Plans are built via StateMachines::Event#plan (or the generated
  # <tt>{event}_plan</tt> instance helper) and executed via
  # StateMachines::TransitionPlan#perform.
  #
  # == Guards
  #
  # Planning only evaluates guards that can be safely pre-checked: state
  # guards (which only read state) and if/unless conditions explicitly
  # declared as pure via StateMachines::Machine#pure_guard.  Any other guard
  # is skipped during planning and reported in +unchecked_guards+ so callers
  # know the plan is conditional.  All guards are fully re-evaluated when the
  # plan is performed.
  #
  # == Staleness
  #
  # A plan is a snapshot.  If the object's state (or the data backing the
  # event's guards) changes after the plan is built -- for example, because
  # another event was fired in the meantime -- the plan becomes stale and
  # will *not* be forced onto the object: performing it re-validates the
  # current state and all preconditions, and raises
  # StateMachines::StalePlanError if they no longer line up with the plan.
  #
  # == Example
  #
  #   plan = vehicle.ignite_plan
  #   plan.from_name       # => :parked
  #   plan.to_name         # => :idling
  #   plan.callback_kinds  # => [:before, :after]
  #   plan.verified?       # => true (all guards were pre-checkable)
  #
  #   vehicle.park         # another event changes the object's state...
  #   plan.perform         # => raises StateMachines::StalePlanError
  class TransitionPlan
    # The transition that was planned.  This transition has never been run;
    # performing the plan always builds and runs a fresh transition.
    attr_reader :transition

    # The branch that was selected when the plan was built
    attr_reader :branch

    # The guards that could not be safely pre-checked when the plan was
    # built.  Each entry is a hash of the form
    # <tt>{ type: :if, condition: ... }</tt> or
    # <tt>{ type: :unless, condition: ... }</tt>.  These guards are only
    # verified when the plan is performed.
    attr_reader :unchecked_guards

    def initialize(transition, branch, unchecked_guards = []) # :nodoc:
      @transition = transition
      @branch = branch
      @unchecked_guards = unchecked_guards.freeze
    end

    # The object the plan was built for
    def object
      transition.object
    end

    # The state machine the plan was built for
    def machine
      transition.machine
    end

    # The name of the event that was planned
    def event
      transition.event
    end

    # The name of the state the object is expected to transition from
    def from_name
      transition.from_name
    end

    # The name of the state the object is expected to transition to
    def to_name
      transition.to_name
    end

    # The state value the object is expected to transition from
    def from
      transition.from
    end

    # The state value the object is expected to transition to
    def to
      transition.to
    end

    # The kinds of transition callbacks that would be run if the planned
    # event were fired, in the order their chains are traversed.  Possible
    # values are :before, :around, and :after.
    #
    # *Note* that this is based on the event/from/to requirements of each
    # callback only; callback if/unless conditions are *not* evaluated so
    # that planning never runs arbitrary application code.
    def callback_kinds
      @callback_kinds ||= begin
        query = { on: event, from: from_name, to: to_name, guard: false }
        kinds = machine.callbacks[:before].select { |callback| callback.branch.matches?(object, query) }.map(&:type)
        kinds += machine.callbacks[:after].select { |callback| callback.branch.matches?(object, query) }.map(&:type)
        kinds.uniq.freeze
      end
    end

    # Whether every guard on the selected branch could be safely pre-checked
    # when this plan was built.  If false, +unchecked_guards+ lists the
    # guards that remain unverified until the plan is performed.
    def verified?
      unchecked_guards.empty?
    end

    # The object's current state name for this plan's machine
    def current_state_name
      machine.states.match!(object).name
    end

    # Whether the object's current state no longer matches the state this
    # plan was built from.  A stale plan can never be performed.
    def stale?
      current_state_name != from_name
    end

    # Performs the planned event.  Before anything is run, the object's
    # current state and the event's preconditions (including guards that
    # could not be pre-checked when the plan was built) are re-evaluated:
    #
    # * If the object's state no longer matches the planned +from+ state, a
    #   StateMachines::StalePlanError is raised.
    # * If the event's guards no longer resolve to the planned transition, a
    #   StateMachines::StalePlanError is raised.
    #
    # Otherwise a fresh transition is performed (running the machine's action
    # and any matching callbacks) and its result is returned.
    def perform(*args)
      machine.reset(object)

      if stale?
        raise StalePlanError.new(object, self, "expected #{machine.name} to be #{from_name.inspect}, but it is now #{current_state_name.inspect}")
      end

      fresh = machine.events.fetch(event).transition_for(object, {}, *args)
      if fresh.nil? || fresh.to_name != to_name
        raise StalePlanError.new(object, self, 'the event no longer resolves to the planned transition')
      end

      fresh.perform(*args)
    end

    # Generates a nicely formatted description of this plan's contents.
    def inspect
      "#<#{self.class} event=#{event.inspect} from_name=#{from_name.inspect} to_name=#{to_name.inspect} verified=#{verified?} stale=#{stale?}>"
    end
  end
end
