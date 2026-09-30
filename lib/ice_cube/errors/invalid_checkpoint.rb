module IceCube
  # Raised when a resume checkpoint cannot be applied to a schedule.
  # This happens when the format version is unknown, when the schedule's
  # rules or times have changed, or when the enumeration boundaries differ.
  class InvalidCheckpoint < StandardError
  end
end
