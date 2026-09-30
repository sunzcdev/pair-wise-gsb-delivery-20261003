# frozen_string_literal: true

module StateMachines
  class Machine
    module Sealing
      # Determines whether this machine's definition has been sealed against
      # further modification.  See #seal! for more information.
      def sealed?
        @sealed
      end

      # Seals this machine's definition, preventing any further changes to its
      # states, events (and their branches), transition callbacks, and the
      # internal indices used to look them up.  This is useful for locking
      # down a machine once the application has booted so that a dynamic
      # extension cannot accidentally change the rules of a running machine.
      #
      # Any attempt to modify the definition afterwards will raise a
      # StateMachines::MachineSealedError indicating which part of the
      # definition was being modified.  For example:
      #
      #   class Vehicle
      #     state_machine initial: :parked do
      #       event :ignite do
      #         transition parked: :idling
      #       end
      #     end
      #   end
      #
      #   Vehicle.state_machine.seal!
      #   Vehicle.state_machine.state :stalled
      #   # => StateMachines::MachineSealedError: Cannot modify states of sealed state machine :state on Vehicle
      #
      # Sealing only affects the machine's *definition*.  Objects that use the
      # machine are never frozen and can keep transitioning as usual.
      #
      # Subclasses can still derive their own editable copy of the machine by
      # explicitly redefining it (passing options or a block to
      # +state_machine+).  The derived machine is a deep copy, so changes made
      # in one subclass never leak into the sealed parent or into sibling
      # subclasses:
      #
      #   class Car < Vehicle
      #     state_machine do
      #       state :stalled
      #     end
      #   end
      #
      #   Car.state_machine.sealed?      # => false
      #   Vehicle.state_machine.sealed?  # => true
      def seal!
        return self if sealed?

        states.seal!
        events.seal!
        @callbacks.each_value do |callbacks|
          callbacks.each(&:freeze)
          callbacks.freeze
        end
        @callbacks.freeze
        @sealed = true
        self
      end

      private

      # Raises a StateMachines::MachineSealedError identifying the given part
      # of the definition if this machine has been sealed
      def assert_not_sealed!(part)
        raise StateMachines::MachineSealedError.new(self, part) if sealed?
      end
    end
  end
end
