# frozen_string_literal: true

module AASM
  # Outcome of a single timeout handling attempt (an advance or an explicit
  # retry/cancel). A rejected transition or an error never raises by itself:
  # the deadline stays in place and is represented by a result object so the
  # caller can decide whether to retry or cancel it.
  class TimeoutResult
    STATUSES = [:fired, :rejected, :error, :cancelled, :none].freeze

    attr_reader :status, :state, :event, :from_state, :to_state, :token, :error

    def initialize(status:, state: nil, event: nil, from_state: nil, to_state: nil, token: nil, error: nil)
      raise ArgumentError, "unknown timeout result status: #{status.inspect}" unless STATUSES.include?(status)
      @status = status
      @state = state
      @event = event
      @from_state = from_state
      @to_state = to_state
      @token = token
      @error = error
    end

    def fired?
      status == :fired
    end

    def rejected?
      status == :rejected
    end

    def error?
      status == :error
    end

    def cancelled?
      status == :cancelled
    end

    def handled?
      !none?
    end

    def none?
      status == :none
    end

    # A rejected or failed deadline is kept and the caller may explicitly retry
    # or cancel it later.
    def retryable?
      rejected? || error?
    end

    def success?
      fired?
    end

    def to_h
      {
        status: status,
        state: state,
        event: event,
        from_state: from_state,
        to_state: to_state,
        token: token,
        error: error
      }
    end

    def inspect
      "#<AASM::TimeoutResult #{to_h.reject { |_, value| value.nil? }.inspect}>"
    end
    alias_method :to_s, :inspect
  end
end
