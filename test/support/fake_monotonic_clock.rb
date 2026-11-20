# frozen_string_literal: true

# Deterministic monotonic clock for tests which need to drive timer deadlines
# without blocking on real time. HTTPX::Utils is the single source of monotonic
# time used by the Timers deadlines and the selector bookkeeping, so stubbing it
# is enough to make every timer fully deterministic.
module FakeMonotonicClockHelpers
  class FakeMonotonicClock
    def initialize(start = 10_000.0)
      @now = start
    end

    attr_reader :now

    def advance(seconds)
      @now += seconds
    end
  end

  def with_fake_clock
    clock = FakeMonotonicClock.new
    utils = HTTPX::Utils.singleton_class

    # capture whatever is currently bound (the real methods, or an outer fake
    # clock in a nested call), so restoration always returns to the right state.
    saved_now = HTTPX::Utils.method(:now)
    saved_elapsed = HTTPX::Utils.method(:elapsed_time)

    HTTPX::Utils.define_singleton_method(:now) { clock.now }
    HTTPX::Utils.define_singleton_method(:elapsed_time) { |timestamp| clock.now - timestamp }

    yield clock
  ensure
    HTTPX::Utils.define_singleton_method(:now, saved_now) if saved_now
    HTTPX::Utils.define_singleton_method(:elapsed_time, saved_elapsed) if saved_elapsed
  end
end
