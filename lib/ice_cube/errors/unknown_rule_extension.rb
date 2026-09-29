module IceCube
  # Raised when a parsed RRULE carries unrecognized extension components and
  # the caller has not explicitly chosen to ignore them.
  class UnknownRuleExtension < ArgumentError
  end
end
