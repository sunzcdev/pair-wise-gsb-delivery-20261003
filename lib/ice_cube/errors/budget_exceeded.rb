module IceCube
  # Raised when an ordinal lookup (Schedule#occurrence_at or
  # Schedule#occurrences_before) cannot use the direct arithmetic fast
  # path and walking the occurrence sequence would exceed the caller's
  # budget. Retry with a larger :budget (or budget: nil to disable the
  # limit) if enumerating that many occurrences is acceptable.
  class BudgetExceeded < StandardError
  end
end
