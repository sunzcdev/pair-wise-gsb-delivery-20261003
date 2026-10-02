# frozen_string_literal: true

module AASM
  # An armed deadline for one specific, successful entry into a state.
  # It becomes void as soon as the state is left; re-entering the same
  # state arms a brand new record.
  class TimeoutRecord
    attr_reader :state, :event, :armed_at, :deadline

    def initialize(state:, event:, armed_at:, deadline:)
      @state = state
      @event = event
      @armed_at = armed_at
      @deadline = deadline
    end

    def expired?(now)
      now >= deadline
    end
  end

  # The explicit outcome of trying to fire the expire event of one
  # expired TimeoutRecord during AASM::InstanceBase#advance_timeouts.
  class TimeoutResult
    STATUSES = [:fired, :rejected, :error].freeze

    attr_reader :state_machine, :state, :event, :status, :exception

    def initialize(state_machine:, state:, event:, status:, exception: nil)
      raise ArgumentError, "unknown status #{status.inspect}" unless STATUSES.include?(status)
      @state_machine = state_machine
      @state = state
      @event = event
      @status = status
      @exception = exception
    end

    # the expire event ran its transition successfully
    def fired?
      status == :fired
    end

    # the expire event was refused (guard / invalid transition);
    # the record stays armed so the caller may retry or cancel explicitly
    def rejected?
      status == :rejected
    end

    # the expire event raised an exception;
    # the record stays armed so the caller may retry or cancel explicitly
    def error?
      status == :error
    end
  end
end
