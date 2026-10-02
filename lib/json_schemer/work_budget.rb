# frozen_string_literal: true
module JSONSchemer
  # Raised when a validation is run with a `work_budget` and the budget is
  # exhausted before a definite valid or invalid conclusion can be reached.
  class BudgetExceeded < StandardError
    attr_reader :instance_location, :keyword_location, :limit, :used

    def initialize(instance_location:, keyword_location:, limit:, used:)
      @instance_location = instance_location
      @keyword_location = keyword_location
      @limit = limit
      @used = used
      super(
        "work budget of #{limit} keyword evaluation(s) exhausted at " \
        "instance location `#{instance_location}` and schema location `#{keyword_location}`"
      )
    end
  end

  # Per-validation-call work budget. A unit of work is a single evaluation of
  # one keyword against one instance location (including keywords entered via
  # `$ref`). Evaluations that are never executed (for example because of
  # short-circuiting) are not counted.
  class WorkBudget
    attr_reader :limit, :used, :instance_location, :keyword_location

    def initialize(limit)
      unless limit.is_a?(Integer) && !limit.negative?
        raise ArgumentError, 'work budget must be a non-negative Integer'
      end

      @limit = limit
      @used = 0
      @exceeded = false
      @instance_location = nil
      @keyword_location = nil
    end

    # Reserves one unit of work for the keyword evaluation at the given
    # locations. Returns `false` and records the first stopped location when
    # the budget is exhausted; subsequent calls keep returning `false`.
    def charge(instance_location, keyword_location)
      return false if @exceeded

      if @used >= @limit
        @exceeded = true
        @instance_location = Location.resolve(instance_location)
        @keyword_location = Location.resolve(keyword_location)
        return false
      end

      @used += 1
      true
    end

    def exceeded?
      @exceeded
    end
  end
end
