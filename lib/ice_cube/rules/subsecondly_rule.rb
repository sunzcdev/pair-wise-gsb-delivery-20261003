module IceCube
  # A rule for sub-second local scheduling. The interval is held as an exact
  # Rational number of seconds, so occurrences always land on
  # start_time + (n * interval) with no floating point drift, preserving the
  # nanoseconds of the schedule's start time.
  class SubsecondlyRule < ValidatedRule
    include Validations::SubsecondlyInterval

    def initialize(interval = 1)
      super
      interval(interval)
      reset
    end

    # Successive occurrences are spaced exactly one interval apart, which may
    # be a fraction of a second.
    def occurrence_step
      base_interval_validation.interval
    end

    # Sub-second occurrences must match the exact instant, not just the
    # whole second.
    def on?(time, schedule)
      next_time(time, schedule, time, increment: false) == time
    end
  end
end
