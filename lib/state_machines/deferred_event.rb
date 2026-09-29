# frozen_string_literal: true

module StateMachines
  # Represents an event that could not be fired in the object's current state
  # and was stored in the object's deferred event queue.  The event's
  # arguments are stored alongside it so that it can be replayed verbatim once
  # a later transition makes it fireable again.
  class DeferredEvent
    # The name of the machine the event belongs to
    attr_reader :machine_name

    # The name of the event that was deferred
    attr_reader :event_name

    # The arguments the event was originally fired with
    attr_reader :args

    # The outcome of the last replay attempt.  One of:
    # * <tt>:pending</tt> - not replayed yet
    # * <tt>:performed</tt> - the event transitioned successfully
    # * <tt>:deferred</tt> - the event still could not fire and was re-queued
    # * <tt>:failed</tt> - a transition matched but failed to perform
    # * <tt>:error</tt> - an exception was raised while replaying
    attr_accessor :outcome

    # The exception captured when the replay errored, if any
    attr_accessor :error

    def initialize(machine_name, event_name, args)
      @machine_name = machine_name.to_sym
      @event_name = event_name.to_sym
      @args = args
      @outcome = :pending
      @error = nil
    end

    def ==(other)
      other.instance_of?(self.class) &&
        other.machine_name == machine_name &&
        other.event_name == event_name &&
        other.args == args
    end

    def inspect
      "#<#{self.class} machine=#{machine_name.inspect} event=#{event_name.inspect} args=#{args.inspect} outcome=#{outcome.inspect}>"
    end
  end
end
