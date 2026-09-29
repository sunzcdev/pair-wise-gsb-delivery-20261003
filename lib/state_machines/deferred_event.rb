# frozen_string_literal: true

module StateMachines
  # Represents an event that could not be fired in the object's current state
  # and was explicitly deferred for later replay.  The event's arguments are
  # stored alongside the event so that it can be re-fired exactly as it was
  # originally invoked once a transition succeeds.
  class DeferredEvent
    # The name of the machine the event belongs to
    attr_reader :machine_name

    # The name of the event that was deferred
    attr_reader :event

    # The arguments the event was originally invoked with
    attr_reader :args

    def initialize(machine_name, event, args)
      @machine_name = machine_name.to_sym
      @event = event.to_sym
      @args = args.dup.freeze
    end

    def ==(other)
      other.is_a?(DeferredEvent) &&
        machine_name == other.machine_name &&
        event == other.event &&
        args == other.args
    end
    alias eql? ==

    def inspect
      "#<#{self.class} machine=#{machine_name.inspect} event=#{event.inspect} args=#{args.inspect}>"
    end
  end
end
