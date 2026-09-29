# frozen_string_literal: true

module StateMachines
  # Represents a sequence of events that are fired one after another on the
  # same object.  Each event is fired only after the previous one has
  # completed, so every event sees the states left behind by the events
  # before it (unlike MachineCollection#fire_events, which fires events in
  # parallel and therefore requires a different attribute for each one).
  #
  # The sequence is all-or-nothing:
  # * The action configured for each machine (e.g. +save+) is only run once
  #   *all* events have transitioned successfully, committing the new states.
  # * If any event (or the final commit) fails, the states of every machine
  #   involved are restored to their original values, along with any pending
  #   event attributes (e.g. +state_event+), and a
  #   StateMachines::InvalidEventSequence error is raised identifying the
  #   step that failed.
  #
  # External side effects (e.g. records created in callbacks) are *not*
  # rolled back automatically.  Callers that need atomic persistence should
  # provide their own transaction boundary.  See
  # StateMachines::InstanceMethods#fire_event_sequence for more information.
  class EventSequence
    # The object the events are being fired on
    attr_reader :object

    # The names of the events in the sequence
    attr_reader :event_names

    # The per-step results of the sequence.  Each entry is a hash with the
    # +:event+ name, the +:machine+ it belongs to, and its +:status+
    # (+:pending+, +:success+, or +:failed+).
    attr_reader :steps

    def initialize(object, machines, event_names, options = {}) # :nodoc:
      @object = object
      @machines = machines
      @event_names = event_names
      @use_transactions = options.fetch(:use_transactions, false)
      @steps = []
      @transitions = []
    end

    # Fires each event in order.  Returns true if every event transitioned
    # and the commit succeeded; raises StateMachines::InvalidEventSequence
    # otherwise.
    def perform
      events = resolve_events
      machines = events.map(&:machine).uniq
      snapshots = snapshot(machines)
      @steps = events.map { |event| { event: event.name, machine: event.machine.name, status: :pending } }

      begin
        run = lambda do
          run_steps(events)
          commit
        end

        if @use_transactions && (machine = machines.first)
          machine.within_transaction(object, &run)
        else
          run.call
        end
      rescue StandardError
        rollback(snapshots)
        raise
      end

      true
    end

    private

    # Finds the actual event for each name in the sequence, raising
    # StateMachines::InvalidEvent if any of them is unknown
    def resolve_events
      @event_names.map do |event_name|
        event = nil
        @machines.detect { |_name, machine| event = machine.events[event_name, :qualified_name] }
        raise(InvalidEvent.new(object, event_name)) unless event

        event
      end
    end

    # Fires each event in order, building each transition from the state left
    # behind by the previous event.  Actions are skipped so that persistence
    # is only committed once the entire sequence has succeeded.
    def run_steps(events)
      events.each_with_index do |event, index|
        machine = event.machine
        machine.reset(object)
        transition = event.transition_for(object)

        if transition && perform_step(transition)
          @transitions << transition
          steps[index][:status] = :success
        else
          event.on_failure(object) unless transition
          steps[index][:status] = :failed
          raise InvalidEventSequence.new(object, @event_names, index, steps)
        end
      end
    end

    # Runs a single transition's callbacks and persists its state in-memory
    # without running the machine's action
    def perform_step(transition)
      TransitionCollection.new([transition], actions: false, use_transactions: false).perform
    end

    # Commits the new states by running each involved machine's action once
    def commit
      @transitions.map(&:action).compact.uniq.each do |action|
        next if object.send(action)

        raise InvalidEventSequence.new(object, @event_names, nil, steps)
      end
    end

    # Captures the current state and pending event attributes for each
    # machine involved in the sequence
    def snapshot(machines)
      machines.each_with_object({}) do |machine, snapshots|
        values = { state: machine.read(object, :state) }
        if machine.action
          values[:event] = machine.read(object, :event, true)
          values[:event_transition] = machine.read(object, :event_transition, true)
        end
        snapshots[machine] = values
      end
    end

    # Restores the states and pending event attributes captured before the
    # sequence started
    def rollback(snapshots)
      @transitions.reverse_each(&:rollback)

      snapshots.each do |machine, values|
        machine.write(object, :state, values[:state])
        machine.write(object, :event, values[:event], true) if values.key?(:event)
        machine.write(object, :event_transition, values[:event_transition], true) if values.key?(:event_transition)
      end
    end
  end
end
