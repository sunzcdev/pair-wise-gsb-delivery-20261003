module IceCube
  # Raised when a BusinessDayAdjustment cannot find a business day within
  # its configured max_days budget for an occurrence.
  class BusinessDayAdjustmentError < StandardError
    # The unadjusted occurrence start time that could not be moved
    attr_reader :occurrence_time

    # The configured shift direction (:forward or :backward)
    attr_reader :direction

    # The configured movement budget in calendar days
    attr_reader :max_days

    def initialize(occurrence_time, direction, max_days)
      @occurrence_time = occurrence_time
      @direction = direction
      @max_days = max_days
      super("no business day within #{max_days} day(s) #{direction} of #{occurrence_time}")
    end
  end
end
