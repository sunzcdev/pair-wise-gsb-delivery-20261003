# frozen_string_literal: true

module StateMachines
  class Machine
    # Adds support for deferring events that cannot be fired in the object's
    # current state.  Deferred events are stored, together with their
    # arguments, in an object-level queue and replayed in order after the next
    # successful transition.
    #
    # Both the queue size and the number of events replayed in a single round
    # are bounded so that a pathological object (e.g. an event that keeps
    # deferring itself) can neither grow the queue without limit nor block the
    # caller forever.
    module Deferring
      # The default maximum number of deferred events held per object
      DEFAULT_DEFER_QUEUE_LIMIT = 25

      # The default maximum number of deferred events replayed per round
      DEFAULT_DEFER_REPLAY_LIMIT = 10

      # The maximum number of deferred events held in an object's queue.
      # Attempting to defer beyond this raises StateMachines::DeferredQueueFull.
      attr_accessor :defer_queue_limit

      # The maximum number of deferred events replayed in a single round.
      # Events still pending once the limit is reached stay queued for the
      # next round.
      attr_accessor :defer_replay_limit

      # The object-level queue of events waiting to be replayed.  Returns a
      # copy; entries are StateMachines::DeferredEvent instances.
      def deferred_events(object)
        defer_queue(object).dup
      end

      # Stores the event (with its arguments) on the object's deferred queue.
      # Returns the StateMachines::DeferredEvent entry that was queued.
      #
      # Raises StateMachines::DeferredQueueFull if the object's queue has
      # reached this machine's +defer_queue_limit+.
      def defer_event(object, event, args)
        queue = defer_queue(object)
        raise StateMachines::DeferredQueueFull.new(object, self) if queue.length >= defer_queue_limit

        entry = StateMachines::DeferredEvent.new(name, event.name, args)
        queue << entry
        entry
      end

      # Replays the object's deferred events in the order they were queued,
      # up to +defer_replay_limit+ events per round.  Each replayed entry is
      # annotated with its outcome (:performed, :deferred, :failed or :error)
      # so that re-defers, callback-triggered events and failures are all
      # observable.
      #
      # Events that defer again are appended back to the queue and may be
      # retried within the same round, but the per-round limit guarantees the
      # caller is never stuck replaying a single stubborn event forever.
      #
      # Returns the list of entries replayed during this round.  This is also
      # memoized on the object and readable via +last_defer_replay+.
      def replay_deferred_events(object)
        return [] if object.instance_variable_get(:@_state_machines_defer_replaying)

        queue = defer_queue(object)
        results = []
        return results if queue.empty?

        object.instance_variable_set(:@_state_machines_defer_replaying, true)
        begin
          remaining = defer_replay_limit
          while remaining.positive? && !queue.empty?
            remaining -= 1
            entry = queue.shift
            replay_deferred_event(object, entry)
            results << entry
          end
        ensure
          object.instance_variable_set(:@_state_machines_defer_replaying, false)
        end

        object.instance_variable_set(:@_state_machines_last_defer_replay, results)
        results
      end

      # The entries replayed during the most recent replay round for the
      # given object, each annotated with its outcome.
      def last_defer_replay(object)
        object.instance_variable_get(:@_state_machines_last_defer_replay) || []
      end

      private

      def replay_deferred_event(object, entry)
        machine = object.class.state_machine(entry.machine_name)
        event = machine.events.fetch(entry.event_name)

        outcome = event.fire(object, *entry.args)
        entry.outcome = case outcome
                        when :deferred then :deferred
                        when true then :performed
                        else :failed
                        end
      rescue StandardError => e
        entry.outcome = :error
        entry.error = e
      end

      def defer_queue(object)
        object.instance_variable_get(:@_state_machines_deferred_events) ||
          object.instance_variable_set(:@_state_machines_deferred_events, [])
      end
    end
  end
end
