# frozen_string_literal: true

module StateMachines
  # An error occurred during a state machine invocation
  class Error < StandardError
    # The object that failed
    attr_reader :object

    def initialize(object, message = nil) # :nodoc:
      @object = object

      super(message)
    end
  end

  # An invalid integration was specified
  class IntegrationNotFound < Error
    def initialize(name)
      super(nil, "#{name.inspect} is an invalid integration. #{error_message}")
    end

    def valid_integrations
      "Valid integrations are: #{valid_integrations_name}"
    end

    def valid_integrations_name
      Integrations.list.collect(&:integration_name)
    end

    def no_integrations
      'No integrations registered'
    end

    def error_message
      if Integrations.list.size.zero?
        no_integrations
      else
        valid_integrations
      end
    end
  end

  # An invalid integration was registered
  class IntegrationError < StandardError
  end

  # An invalid event was specified
  class InvalidEvent < Error
    # The event that was attempted to be run
    attr_reader :event

    def initialize(object, event_name) # :nodoc:
      @event = event_name

      super(object, "#{event.inspect} is an unknown state machine event")
    end
  end

  # An invalid transition was attempted
  class InvalidTransition < Error
    # The machine attempting to be transitioned
    attr_reader :machine

    # The current state value for the machine
    attr_reader :from

    def initialize(object, machine, event) # :nodoc:
      @machine = machine
      @from_state = machine.states.match!(object)
      @from = machine.read(object, :state)
      @event = machine.events.fetch(event)
      errors = machine.errors_for(object)

      message = "Cannot transition #{machine.name} via :#{self.event} from #{from_name.inspect}"
      message << " (Reason(s): #{errors})" unless errors.empty?
      super(object, message)
    end

    # The event that triggered the failed transition
    def event
      @event.name
    end

    # The fully-qualified name of the event that triggered the failed transition
    def qualified_event
      @event.qualified_name
    end

    # The name for the current state
    def from_name
      @from_state.name
    end

    # The fully-qualified name for the current state
    def qualified_from_name
      @from_state.qualified_name
    end
  end

  # A set of transition failed to run in parallel
  class InvalidParallelTransition < Error
    # The set of events that failed the transition(s)
    attr_reader :events

    def initialize(object, events) # :nodoc:
      @events = events

      super(object, "Cannot run events in parallel: #{events * ', '}")
    end
  end

  # A method was called in an invalid state context
  class InvalidContext < Error
  end

  # A transition plan was executed after the object's state or guard
  # conditions changed, making the plan's assumptions no longer valid
  class StalePlanError < Error
    # The plan that could no longer be executed
    attr_reader :plan

    def initialize(plan) # :nodoc:
      @plan = plan

      super(plan.object, "Cannot execute plan for #{plan.machine.name.inspect} via #{plan.event_name.inspect} from #{plan.from_name.inspect}: the object's state or requirements changed since the plan was created")
    end
  end
end
