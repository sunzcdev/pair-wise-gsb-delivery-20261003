# frozen_string_literal: true

module SpecSupport
  # A clock whose observed value changes on every call. This makes the lack of
  # a single observation time visible: code that reads the clock more than once
  # during one verification round gets different values.
  class JumpingClock
    attr_reader :call_count

    # @param start [Numeric] the first NumericDate returned, in seconds since the epoch.
    # @param step [Numeric] the amount added after every observation.
    def initialize(start:, step: 1)
      @current = start
      @step = step
      @call_count = 0
    end

    def call
      @call_count += 1
      value = @current
      @current += @step
      value
    end
  end
end
