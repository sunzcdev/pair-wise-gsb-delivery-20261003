# frozen_string_literal: true

module AASM
  module Core
    # A single deadline created when a state with a +timeout+ declaration is
    # successfully entered. The deadline belongs to that particular entry into
    # the state and is discarded as soon as the state is left (including
    # self-transitions); re-entering a state always arms a brand new entry with
    # a new, monotonically increasing token.
class TimeoutEntry
      attr_reader :state, :deadline, :event, :token

      def initialize(state:, deadline:, event:, token:)
        @state = state
        @deadline = deadline
        @event = event
        @token = token
      end

      # An entry is due when its deadline has been reached. A deadline equal to
      # +now+ is already due (<=), which makes boundaries exact.
      def due?(now)
        deadline <= now
      end

      # Remaining time until the deadline, floored at zero.
      def remaining(now)
        deadline - now > 0 ? deadline - now : 0
      end
    end
  end
end
