# frozen_string_literal: true

module StateMachines
  class Machine
    module PureGuards
      # Declares one or more guard methods as *pure*: they are expected to be
      # side-effect-free checks whose result depends only on the object's
      # current state.  Only guards declared this way (and state guards such
      # as :if_state, which only read state) are evaluated when an event is
      # planned via StateMachines::Event#plan.  Any other guard is reported
      # as unchecked on the resulting plan and is only verified when the plan
      # is executed.
      #
      # == Examples
      #
      #   class Vehicle
      #     state_machine do
      #       pure_guard :seatbelt_on?
      #
      #       event :ignite do
      #         transition :parked => :idling, :if => :seatbelt_on?
      #       end
      #     end
      #   end
      def pure_guard(*names)
        names.flatten.each { |name| @pure_guards |= [name.to_sym] }
      end

      # The names of the guard methods declared as pure via +pure_guard+
      def pure_guards
        @pure_guards
      end
    end
  end
end
