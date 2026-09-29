module IceCube
  # Optional schedule post-processing that moves occurrences falling on
  # non-business days to a nearby business day (e.g. monthly settlement
  # dates that land on weekends or holidays).
  #
  # Whether a day is a business day is decided entirely by a pure callback
  # supplied by the caller. The callback receives a Date and must return
  # truthy when the day is a business day. IceCube ships no holiday
  # calendar and never consults or mutates the system time zone for this
  # feature.
  #
  #   adjustment = IceCube::BusinessDayAdjustment.new(direction: :forward, max_days: 3) do |date|
  #     (1..5).cover?(date.wday) && !HOLIDAYS.include?(date)
  #   end
  #
  # direction:
  #   :forward  - move occurrences to a later date
  #   :backward - move occurrences to an earlier date
  #
  # If no business day exists within max_days calendar days of the raw
  # occurrence, IceCube::BusinessDayAdjustmentError is raised.
  class BusinessDayAdjustment
    DIRECTIONS = [:forward, :backward].freeze

    attr_reader :direction, :max_days

    def initialize(direction: :forward, max_days: 7, &business_day)
      unless DIRECTIONS.include?(direction)
        raise ArgumentError, "direction must be one of #{DIRECTIONS.inspect}, got #{direction.inspect}"
      end
      unless max_days.is_a?(Integer) && max_days > 0
        raise ArgumentError, "max_days must be a positive Integer, got #{max_days.inspect}"
      end
      raise ArgumentError, "a block deciding business days is required" unless business_day

      @direction = direction
      @max_days = max_days
      @business_day = business_day
    end

    def shifts_later?
      direction == :forward
    end

    def shifts_earlier?
      direction == :backward
    end

    def business_day?(date)
      !!@business_day.call(date)
    end

    # Move an occurrence to a business day, preserving its time of day and
    # duration. Returns the occurrence unchanged when it already falls on
    # a business day. Raises BusinessDayAdjustmentError when no business
    # day exists within max_days.
    def adjust_occurrence(occurrence)
      days = days_to_business_day(occurrence.start_time)
      return occurrence if days.zero?
      Occurrence.new(
        shift_days(occurrence.start_time, days),
        shift_days(occurrence.end_time, days)
      )
    end

    private

    def days_to_business_day(time)
      step = shifts_later? ? 1 : -1
      date = TimeUtil.ensure_date(time)
      return 0 if business_day?(date)
      (1..max_days).each do |n|
        return step * n if business_day?(date + step * n)
      end
      raise BusinessDayAdjustmentError.new(time, direction, max_days)
    end

    # DST-safe day shift preserving the wall-clock time of day
    def shift_days(time, days)
      wrapper = TimeUtil::TimeWrapper.new(time, true)
      wrapper.add(:day, days)
      wrapper.to_time
    end
  end
end
