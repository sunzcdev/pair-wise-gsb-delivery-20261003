module IceCube
  # Optional post-processing for Schedule occurrences: raw occurrences that
  # do not fall on a business day are shifted, day by day, to the nearest
  # business day in the configured direction.
  #
  # Whether a day is a business day is decided solely by a pure predicate
  # provided by the caller. The predicate receives a Time (in the schedule's
  # own time zone) and must return a truthy/falsy value without side effects.
  #
  # The shift is performed on whole calendar days in the occurrence's own
  # time zone (wall-clock time is preserved across DST changes); the system
  # time zone is never read or modified.
  #
  # If no business day exists within +max_days+ of a raw occurrence,
  # BusinessDayAdjustmentError is raised.
  class BusinessDayAdjustment
    DIRECTIONS = [:forward, :backward].freeze

    attr_reader :direction, :max_days, :predicate

    def initialize(direction: :forward, max_days: 7, &predicate)
      unless DIRECTIONS.include?(direction)
        raise ArgumentError, "direction must be one of #{DIRECTIONS.inspect}"
      end
      unless max_days.is_a?(Integer) && max_days >= 0
        raise ArgumentError, "max_days must be a non-negative Integer"
      end
      unless predicate
        raise ArgumentError, "a business day predicate block is required"
      end
      @direction = direction
      @max_days = max_days
      @predicate = predicate
    end

    def business_day?(time)
      !!predicate.call(time)
    end

    # Shift +time+ by whole calendar days to the nearest business day in the
    # configured direction, preserving wall-clock time in +time+'s own zone.
    # Returns +time+ unchanged when it already falls on a business day.
    def adjust(time)
      return time if business_day?(time)
      step = (direction == :forward) ? 1 : -1
      1.upto(max_days) do |n|
        candidate = shift_days(time, step * n)
        return candidate if business_day?(candidate)
      end
      raise BusinessDayAdjustmentError.new(time, direction, max_days)
    end

    private

    # Calendar-day shift via TimeWrapper so DST transitions keep the local
    # wall-clock time instead of a fixed UTC offset.
    def shift_days(time, days)
      wrapper = TimeUtil::TimeWrapper.new(time)
      wrapper.add(:day, days)
      wrapper.to_time
    end
  end
end
