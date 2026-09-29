# frozen_string_literal: true

module StateMachines
  class Machine
    # Adds support for explicitly deferring events that cannot be handled in
    # the object's current state.  Deferred events are stored, together with
    # their arguments, on an object-level pending queue and replayed in order
    # after the next successful transition.
    #
    # Events must be explicitly marked as deferrable, either via the
    # +defer_events+ helper or the <tt>:defer</tt> event option:
    #
    #   class Vehicle
    #     state_machine initial: :parked do
    #       event :ignite do
    #         transition parked: :idling
    #       end
    #
    #       event :shift_up, defer: true do
    #         transition idling: :first_gear
    #       end
    #     end
    #   end
    #
    #   vehicle = Vehicle.new
    #   vehicle.shift_up            # => :deferred (parked, cannot shift up yet)
    #   vehicle.deferred_events     # => [#<StateMachines::DeferredEvent event=:shift_up ...>]
    #   vehicle.ignite              # => true (replays :shift_up afterwards)
    #   vehicle.state               # => "first_gear"
    #
    # Events that are *not* declared deferrable keep the default behavior:
    # firing them in an invalid state returns false (or raises
    # StateMachines::InvalidTransition for the bang version).
    #
    # Both the pending queue and the work done in a single replay round are
    # bounded:
    # * +defer_queue_size+ - the maximum number of deferred events kept per
    #   object for this machine.  Deferring beyond the limit raises
    #   StateMachines::DeferredQueueOverflow.  Default is 100.
    # * +defer_replay_limit+ - the maximum number of entries replayed in a
    #   single round.  This guarantees that an event which keeps being
    #   deferred cannot block the caller forever.  Default is 10.
    #
    # Both can be configured as machine options:
    #
    #   state_machine initial: :parked, defer_queue_size: 10, defer_replay_limit: 3 do
    #     ...
    #   end
    #
    # Observability:
    # * Firing a deferrable event in an invalid state returns +:deferred+.
    # * The object's pending queue is available via +deferred_events+.
    # * Every replay round stores a StateMachines::DeferredReplayReport on the
    #   object, available via +deferred_replay_report+, recording which
    #   entries fired, which were deferred again, and which failed (including
    #   the error raised).  Events produced by callbacks during replay simply
    #   show up on the queue / in the report like any other deferred event.
    module Deferral
      # The maximum number of deferred events kept per object for this machine
      attr_accessor :defer_queue_size

      # The maximum number of deferred entries replayed in a single round
      attr_accessor :defer_replay_limit

      # The names of the events that may be deferred when they cannot be
      # fired in the object's current state
      attr_reader :deferred_event_names

      # Marks the given events as deferrable.  When a deferrable event is
      # fired in a state where no transition matches, it is stored on the
      # object's deferred queue (along with its arguments) instead of
      # failing, and replayed after the next successful transition.
      def defer_events(*names)
        names.each { |name| @deferred_event_names |= [name.to_sym] }
      end

      # Determines whether the given event may be deferred for this machine
      def deferred_event?(event_name)
        @deferred_event_names.include?(event_name.to_sym)
      end

      # Stores the event and its arguments on the object's deferred queue.
      # Raises StateMachines::DeferredQueueOverflow if the machine's queue
      # limit for this object has been reached.
      def defer_event(object, event_name, args)
        queue = deferred_queue_for(object)
        pending = queue.count { |entry| entry.machine_name == name }
        raise DeferredQueueOverflow.new(object, self, defer_queue_size) if pending >= defer_queue_size

        entry = DeferredEvent.new(name, event_name, args)
        queue << entry
        entry
      end

      # Replays this machine's deferred events for the object, in the order
      # they were deferred, bounded by +defer_replay_limit+ per round.  Each
      # entry is re-fired with its original arguments:
      # * events that still cannot transition are deferred again
      # * events that raise are recorded as failures and replay continues
      #
      # The resulting StateMachines::DeferredReplayReport is stored on the
      # object and also returned.  Re-entrant calls (triggered by transitions
      # performed during the replay itself) are no-ops; the outermost round
      # keeps processing the queue.
      def replay_deferred_events(object)
        queue = deferred_queue_for(object)
        return unless queue.any? { |entry| entry.machine_name == name }

        guards = deferred_replay_guards_for(object)
        return if guards[name]

        report = DeferredReplayReport.new
        deferred_replay_reports_for(object)[name] = report

        guards[name] = true
        begin
          # Only entries pending at the start of the round are replayed.
          # Entries deferred (again) during the round, including events
          # produced by callbacks, wait for the next round.
          batch = []
          queue.delete_if do |entry|
            entry.machine_name == name && batch.length < defer_replay_limit && (batch << entry)
          end
          batch.each { |entry| replay_deferred_entry(object, entry, report) }
        ensure
          guards.delete(name)
        end
        report
      end

      # The object's pending queue of deferred events.  The queue is stored
      # on the object itself so that it is shared by all of its machines.
      def deferred_queue_for(object)
        object.instance_variable_get(:@_state_machines_deferred_events) ||
          object.instance_variable_set(:@_state_machines_deferred_events, [])
      end

      private

      def replay_deferred_entry(object, entry, report)
        result = events.fetch(entry.event).fire(object, *entry.args)
        if result == :deferred
          report.redeferred << entry
        elsif result
          report.fired << entry
        else
          report.failed << [entry, nil]
        end
      rescue StandardError => e
        report.failed << [entry, e]
      end

      def deferred_replay_guards_for(object)
        object.instance_variable_get(:@_state_machines_deferred_replay_guards) ||
          object.instance_variable_set(:@_state_machines_deferred_replay_guards, {})
      end

      def deferred_replay_reports_for(object)
        object.instance_variable_get(:@_state_machines_deferred_replay_reports) ||
          object.instance_variable_set(:@_state_machines_deferred_replay_reports, {})
      end
    end
  end
end
