# frozen_string_literal: true

module AASM
  module Core
    # Provides the monotonic time used to arm and evaluate state timeouts.
    #
    # The default time source is +Process.clock_gettime(Process::CLOCK_MONOTONIC)+.
    # AASM itself never spawns a thread and never starts a timer: the caller is
    # responsible for driving timeout evaluation by handing the current
    # monotonic time to {AASM::InstanceBase#time_out}.
    #
    # While a timeout is being advanced (or retried), the "now" supplied by the
    # caller is pushed onto a fiber-local stack so that any deadline armed by a
    # successful transition during that same advance is anchored to the exact
    # same time base used for the evaluation.
    module MonotonicClock
      OVERRIDE_KEY = :_aasm_monotonic_now_overrides
      private_constant :OVERRIDE_KEY

      # Returns the currently effective monotonic time: an override pushed by
      # {.with_now} takes precedence, otherwise +fallback+ (typically the
      # including object's own clock hook), otherwise the system clock.
      def self.now_or(fallback = nil)
        overrides = Thread.current[OVERRIDE_KEY]
        overrides ? overrides.last : (fallback || default_now)
      end

      # Runs the given block with +now+ treated as the current monotonic time.
      # Overrides nest and are always removed again.
      def self.with_now(now)
        Thread.current[OVERRIDE_KEY] ||= []
        overrides = Thread.current[OVERRIDE_KEY]
        overrides.push(now)
        begin
          yield
        ensure
          overrides.pop
          Thread.current[OVERRIDE_KEY] = nil if overrides.empty?
        end
      end

      def self.default_now
        Process.clock_gettime(Process::CLOCK_MONOTONIC)
      end
    end
  end
end
