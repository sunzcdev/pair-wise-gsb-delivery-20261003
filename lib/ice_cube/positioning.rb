module IceCube
  # Answers ordinal positioning ("what is the Nth occurrence?") and reverse
  # counting ("how many occurrences happen before a given time?") queries
  # for a Schedule. Positions always refer to the schedule's final,
  # deduplicated sequence of occurrences, after COUNT/UNTIL limits,
  # overlapping rules and exception times have been applied.
  #
  # A schedule made of a single plain interval rule (optionally bounded by
  # +count+ and/or +until+) is answered arithmetically in O(1) - no
  # occurrence is ever enumerated, so even the 100_000th occurrence of a
  # long schedule is found directly. Any other combination (multiple or
  # overlapping rules, BYxxx validations, exception rules or times, extra
  # recurrence times) falls back to walking the schedule's enumerator,
  # bounded by a budget so a huge ordinal on a non-terminating schedule
  # cannot run forever. The returned OrdinalResult always reports which
  # strategy produced the answer.
  module Positioning
    # Maximum number of occurrences the enumeration fallback will walk
    # before giving up with BudgetExceeded.
    DEFAULT_BUDGET = 100_000

    # Validation keys that keep a rule arithmetically computable: the
    # interval itself, the two limits, and schedule locks (which only pin
    # occurrences to the start time's clock components).
    ARITHMETIC_VALIDATIONS = [:interval, :count, :until].freeze

    # An arithmetic computation plan for a single-rule schedule.
    Plan = Struct.new(:start_time, :unit, :step, :count, :until_bound)

    module_function

    # Locate the occurrence at a 0-based index in the schedule's final,
    # deduplicated sequence of occurrences.
    def occurrence_at(schedule, index, budget: DEFAULT_BUDGET)
      unless index.is_a?(Integer) && index >= 0
        raise ArgumentError, "Expecting a non-negative Integer index, got #{index.inspect}"
      end
      if (plan = arithmetic_plan(schedule))
        OrdinalResult.new(plan_occurrence_at(plan, index), strategy: :arithmetic, iterations: 0)
      else
        enumerate_occurrence_at(schedule, index, budget)
      end
    end

    # Count how many of the schedule's occurrences happen strictly before
    # the given time.
    def occurrences_before(schedule, time, budget: DEFAULT_BUDGET)
      if (plan = arithmetic_plan(schedule))
        OrdinalResult.new(plan_count_before(plan, time), strategy: :arithmetic, iterations: 0)
      else
        enumerate_count_before(schedule, time, budget)
      end
    end

    # Build an arithmetic plan when the schedule is exactly computable:
    # no exceptions, no extra recurrence times, and at most one plain
    # interval recurrence rule. Returns nil when the schedule needs the
    # enumeration fallback.
    def arithmetic_plan(schedule)
      return nil unless schedule.exception_rules.empty? && schedule.exception_times.empty?

      rules = schedule.recurrence_rules
      return nil unless schedule.recurrence_times.empty?

      if rules.empty?
        # Only the implicit start time occurrence.
        return Plan.new(schedule.start_time, :sec, 1, 1, nil)
      end
      return nil unless rules.length == 1

      rule = rules.first
      unit, multiplier = interval_unit(rule)
      return nil unless unit && arithmetic_rule?(rule)

      start_time = schedule.start_time
      # Month arithmetic clamps the day of month on shorter months, which
      # makes stepwise enumeration drift away from a one-shot computation.
      return nil if [:month, :year].include?(unit) && start_time.day > 28

      until_bound = rule.until_time && TimeUtil.ensure_time(rule.until_time, start_time, true)
      step = rule.base_interval_validation.interval * multiplier
      Plan.new(start_time, unit, step, rule.occurrence_count, until_bound)
    end

    # A rule is arithmetically computable when it only carries its interval
    # validation, COUNT/UNTIL limits, and schedule locks.
    def arithmetic_rule?(rule)
      return false unless rule.is_a?(ValidatedRule)
      rule.validations.keys.all? do |key|
        ARITHMETIC_VALIDATIONS.include?(key) || key.to_s.start_with?("base_")
      end
    end

    # The TimeWrapper unit each interval step spans, and how many of those
    # units a single interval covers, per rule class.
    def interval_unit(rule)
      case rule
      when SecondlyRule then [:sec, 1]
      when MinutelyRule then [:min, 1]
      when HourlyRule then [:hour, 1]
      when DailyRule then [:day, 1]
      when WeeklyRule then [:day, 7]
      when MonthlyRule then [:month, 1]
      when YearlyRule then [:year, 1]
      end
    end

    # The occurrence at +index+ for an arithmetic plan, honoring COUNT and
    # UNTIL. Calendar units are advanced with a DST-aware TimeWrapper so
    # wall-clock time is preserved, matching stepwise enumeration.
    def advance(plan, index)
      steps = index * plan.step
      case plan.unit
      when :sec then plan.start_time + steps
      when :min then plan.start_time + steps * ONE_MINUTE
      when :hour then plan.start_time + steps * ONE_HOUR
      else
        wrapper = TimeUtil::TimeWrapper.new(plan.start_time, true)
        wrapper.add(plan.unit, steps)
        wrapper.to_time
      end
    end

    def plan_occurrence_at(plan, index)
      return nil if plan.count && index >= plan.count
      time = advance(plan, index)
      return nil if plan.until_bound && time > plan.until_bound
      time
    end

    def plan_count_before(plan, time)
      return 0 if time <= plan.start_time
      count = first_index_where(plan, time) { |t| t >= time }
      count = plan.count if plan.count && plan.count < count
      if plan.until_bound
        until_count = first_index_where(plan, plan.until_bound) { |t| t > plan.until_bound }
        count = until_count if until_count < count
      end
      count
    end

    # Smallest index whose occurrence satisfies the predicate, starting
    # from an O(1) estimate and adjusting by single steps.
    def first_index_where(plan, reference)
      index = estimate_index(plan, reference)
      index -= 1 while index > 0 && yield(advance(plan, index - 1))
      index += 1 until yield(advance(plan, index))
      index
    end

    def estimate_index(plan, time)
      start_time = plan.start_time
      estimate = case plan.unit
      when :sec then (time - start_time) / plan.step
      when :min then (time - start_time) / (plan.step * ONE_MINUTE)
      when :hour then (time - start_time) / (plan.step * ONE_HOUR)
      when :day then (time - start_time) / (plan.step * ONE_DAY)
      when :month
        ((time.year - start_time.year) * 12 + time.month - start_time.month) / plan.step.to_f
      when :year
        (time.year - start_time.year) / plan.step.to_f
      end
      [estimate.floor, 0].max
    end

    def enumerate_occurrence_at(schedule, index, budget)
      iterations = 0
      schedule.send(:enumerate_occurrences, schedule.start_time).each do |occurrence|
        if iterations == index
          return OrdinalResult.new(occurrence.to_time, strategy: :enumeration, iterations: iterations)
        end
        iterations += 1
        check_budget!(iterations, budget)
      end
      OrdinalResult.new(nil, strategy: :enumeration, iterations: iterations)
    end

    def enumerate_count_before(schedule, time, budget)
      count = 0
      schedule.send(:enumerate_occurrences, schedule.start_time).each do |occurrence|
        break unless occurrence < time
        count += 1
        check_budget!(count, budget)
      end
      OrdinalResult.new(count, strategy: :enumeration, iterations: count)
    end

    def check_budget!(iterations, budget)
      return unless iterations > budget
      raise BudgetExceeded,
        "Ordinal positioning for this schedule requires enumerating more " \
        "than #{budget} occurrences. This combination of rules cannot be " \
        "computed arithmetically; pass a larger :budget or simplify the " \
        "schedule's rules."
    end
  end
end
