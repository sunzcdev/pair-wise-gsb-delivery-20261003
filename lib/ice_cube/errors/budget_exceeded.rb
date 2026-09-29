module IceCube
  # An exception for when an ordinal positioning or reverse counting query
  # cannot be answered within the allowed enumeration budget
  class BudgetExceeded < StandardError
  end
end
