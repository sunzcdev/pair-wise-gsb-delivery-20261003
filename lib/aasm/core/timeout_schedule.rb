# frozen_string_literal: true

module AASM
  module Core
    # Owns the outstanding {TimeoutEntry} of one state machine instance.
    #
    # A state machine can only ever have a single outstanding deadline: entering
    # any state invalidates the deadline of the state that is being left.
    class TimeoutSchedule
      attr_reader :entry

      def initialize
        @entry = nil
        @counter = 0
      end

      # Arms a deadline for +state+ entered at monotonic time +now+.
      def arm(state, now, timeout_duration, event)
        @counter += 1
        @entry = TimeoutEntry.new(
          state: state,
          deadline: now + timeout_duration,
          event: event,
          token: @counter
        )
      end

      # Drops the deadline currently identified by +token+. Does nothing when
      # the token no longer identifies the live deadline (e.g. the state was
      # already left and a fresh deadline was armed).
      def disarm(token)
        @entry = nil if @entry && @entry.token == token
      end

      def clear
        @entry = nil
      end

      # Snapshot of entries that were already due at +now+ at the moment of the
      # call. Entries armed afterwards (for instance by a transition into a
      # zero-wait state during the same advance) are deliberately excluded so
      # they only get handled on the next advance, preventing spinning.
      def due(now)
        @entry && @entry.due?(now) ? [@entry] : []
      end
    end
  end
end
