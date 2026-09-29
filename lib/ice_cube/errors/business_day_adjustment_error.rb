module IceCube
  # Raised when a business day adjustment cannot find a business day
  # within the configured number of days from a raw occurrence.
  class BusinessDayAdjustmentError < StandardError
    attr_reader :occurrence_time, :direction, :max_days

    def initialize(occurrence_time, direction, max_days)
      @occurrence_time = occurrence_time
      @direction = direction
      @max_days = max_days
      super(
        "No business day found within #{max_days} day(s) " \
        "#{direction} of #{occurrence_time}"
      )
    end
  end
end
