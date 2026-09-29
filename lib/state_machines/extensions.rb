# frozen_string_literal: true

module StateMachines
  module ClassMethods
    def self.extended(base) # :nodoc:
      base.class_eval do
        @state_machines = MachineCollection.new
      end
    end

    # Gets the current list of state machines defined for this class.  This
    # class-level attribute acts like an inheritable attribute.  The attribute
    # is available to each subclass, each having a copy of its superclass's
    # attribute.
    #
    # The hash of state machines maps <tt>:attribute</tt> => +machine+, e.g.
    #
    #   Vehicle.state_machines # => {:state => #<StateMachines::Machine:0xb6f6e4a4 ...>}
    def state_machines
      @state_machines ||= superclass.state_machines.dup
    end
  end

  module InstanceMethods
    # Runs one or more events in parallel.  All events will run through the
    # following steps:
    # * Before callbacks
    # * Persist state
    # * Invoke action
    # * After callbacks
    #
    # For example, if two events (for state machines A and B) are run in
    # parallel, the order in which steps are run is:
    # * A - Before transition callbacks
    # * B - Before transition callbacks
    # * A - Persist new state
    # * B - Persist new state
    # * A - Invoke action
    # * B - Invoke action (only if different than A's action)
    # * A - After transition callbacks
    # * B - After transition callbacks
    #
    # *Note* that multiple events on the same state machine / attribute cannot
    # be run in parallel.  If this is attempted, an ArgumentError will be
    # raised.
    #
    # == Halting callbacks
    #
    # When running multiple events in parallel, special consideration should
    # be taken with regard to how halting within callbacks affects the flow.
    #
    # For *before* callbacks, any <tt>:halt</tt> error that's thrown will
    # immediately cancel the perform for all transitions.  As a result, it's
    # possible for one event's transition to affect the continuation of
    # another.
    #
    # On the other hand, any <tt>:halt</tt> error that's thrown within an
    # *after* callback with only affect that event's transition.  Other
    # transitions will continue to run their own callbacks.
    #
    # == Example
    #
    #   class Vehicle
    #     state_machine :initial => :parked do
    #       event :ignite do
    #         transition :parked => :idling
    #       end
    #
    #       event :park do
    #         transition :idling => :parked
    #       end
    #     end
    #
    #     state_machine :alarm_state, :namespace => 'alarm', :initial => :on do
    #       event :enable do
    #         transition all => :active
    #       end
    #
    #       event :disable do
    #         transition all => :off
    #       end
    #     end
    #   end
    #
    #   vehicle = Vehicle.new                         # => #<Vehicle:0xb7c02850 @state="parked", @alarm_state="active">
    #   vehicle.state                                 # => "parked"
    #   vehicle.alarm_state                           # => "active"
    #
    #   vehicle.fire_events(:ignite, :disable_alarm)  # => true
    #   vehicle.state                                 # => "idling"
    #   vehicle.alarm_state                           # => "off"
    #
    #   # If any event fails, the entire event chain fails
    #   vehicle.fire_events(:ignite, :enable_alarm)   # => false
    #   vehicle.state                                 # => "idling"
    #   vehicle.alarm_state                           # => "off"
    #
    #   # Exception raised on invalid event
    #   vehicle.fire_events(:park, :invalid)          # => StateMachines::InvalidEvent: :invalid is an unknown event
    #   vehicle.state                                 # => "idling"
    #   vehicle.alarm_state                           # => "off"
    def fire_events(*events)
      self.class.state_machines.fire_events(self, *events)
    end

    # Run one or more events in parallel.  If any event fails to run, then
    # a StateMachines::InvalidTransition exception will be raised.
    #
    # See StateMachines::InstanceMethods#fire_events for more information.
    #
    # == Example
    #
    #   class Vehicle
    #     state_machine :initial => :parked do
    #       event :ignite do
    #         transition :parked => :idling
    #       end
    #
    #       event :park do
    #         transition :idling => :parked
    #       end
    #     end
    #
    #     state_machine :alarm_state, :namespace => 'alarm', :initial => :active do
    #       event :enable do
    #         transition all => :active
    #       end
    #
    #       event :disable do
    #         transition all => :off
    #       end
    #     end
    #   end
    #
    #   vehicle = Vehicle.new                         # => #<Vehicle:0xb7c02850 @state="parked", @alarm_state="active">
    #   vehicle.fire_events(:ignite, :disable_alarm)  # => true
    #
    #   vehicle.fire_events!(:ignite, :disable_alarm) # => StateMachines::InvalidParallelTransition: Cannot run events in parallel: ignite, disable_alarm
    def fire_events!(*events)
      run_action = [true, false].include?(events.last) ? events.pop : true
      fire_events(*(events + [run_action])) || raise(StateMachines::InvalidParallelTransition.new(self, events))
    end

    # Runs a sequence of events in order.  Each event is fired only after the
    # previous one has completed, so every event sees the states left behind
    # by the events before it.  This allows multiple events to be chained on
    # the *same* state machine, unlike #fire_events which runs events in
    # parallel on different machines.
    #
    # The sequence is all-or-nothing:
    # * The action configured for each machine (e.g. +save+) is only run once
    #   *all* events have transitioned successfully, committing the new
    #   states.
    # * If any event (or the final commit) fails, the states of every machine
    #   involved are restored to their original values, along with any pending
    #   event attributes (e.g. +state_event+), and a
    #   StateMachines::InvalidEventSequence error is raised.  The error
    #   identifies exactly which step failed via +failed_event+,
    #   +failed_index+, and the per-step +steps+ statuses.
    #
    # External side effects (e.g. records created in callbacks) are *not*
    # rolled back automatically.  If the sequence must be atomic with respect
    # to a database, provide a transaction boundary with a block.  The block
    # is yielded a runner that performs the sequence; a failing sequence
    # raises through the block so the surrounding transaction rolls back:
    #
    #   vehicle.fire_event_sequence(:ignite, :shift_up) do |sequence|
    #     Vehicle.transaction { sequence.call }
    #   end
    #
    # Event sequences cannot be nested: calling this method from within a
    # running sequence (e.g. from a transition callback) raises
    # StateMachines::InvalidContext.
    #
    # == Example
    #
    #   class Vehicle
    #     state_machine :initial => :parked do
    #       event :ignite do
    #         transition :parked => :idling
    #       end
    #
    #       event :shift_up do
    #         transition :idling => :first_gear
    #       end
    #
    #       event :park do
    #         transition :idling => :parked
    #       end
    #     end
    #   end
    #
    #   vehicle = Vehicle.new                           # => #<Vehicle:0xb7c02850 @state="parked">
    #   vehicle.fire_event_sequence(:ignite, :shift_up) # => true
    #   vehicle.state                                   # => "first_gear"
    #
    #   # If any event fails, nothing is committed and the failing step is
    #   # reported
    #   vehicle.fire_event_sequence(:park, :ignite, :shift_up)
    #   # => StateMachines::InvalidEventSequence: Cannot run event sequence: step 1 (:park) of :park, :ignite, :shift_up failed
    #   vehicle.state                                   # => "first_gear"
    def fire_event_sequence(*events, &block)
      self.class.state_machines.fire_event_sequence(self, *events, &block)
    end

    protected

    def initialize_state_machines(options = {}, &) # :nodoc:
      self.class.state_machines.initialize_states(self, options, &)
    end
  end
end
