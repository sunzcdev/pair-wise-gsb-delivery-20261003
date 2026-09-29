module IceCube
  # Explains why a schedule does or does not occur at a specific time.
  #
  # The result is plain structured data (Hashes, Arrays, Symbols, Times and
  # the rule objects themselves) with no translated strings, so user
  # interfaces can localize it however they like.
  #
  #   schedule.explain(time) # =>
  #   {
  #     time:       Time,             # the explained time (schedule's zone)
  #     occurs:     false,            # whether the schedule occurs at +time+
  #     included:   true,             # any recurrence matched
  #     excluded:   true,             # any exception matched
  #     reasons:    [:excluded_by_exception],
  #     recurrence: [<entry>, ...],   # recurrence rules and times
  #     exceptions: [<entry>, ...]    # exception rules and times
  #   }
  #
  # Each entry looks like:
  #
  #   {
  #     kind:       :rule,            # or :single_occurrence for rdates/exdates
  #     rule:       Rule,             # the rule object (call to_s/to_hash on it)
  #     matched:    false,            # would emit an occurrence at +time+
  #     blocked_by: [:count],         # :start_time, :until, :count, :conditions
  #     limits:     {until: {time:, exceeded:}, count: {max:, used:, exceeded:}},
  #     conditions: [{key:, type:, value:, matched:, offset:}, ...]
  #   }
  #
  # +conditions+ locates the sub-conditions of composite rules (one entry per
  # validation); +offset+ is the distance to the next acceptable time in
  # units of the condition's +type+.
  #
  # Explanation is read-only: COUNT state is never consumed and rules are
  # reset afterwards, so explaining a time cannot change subsequent
  # enumeration results. Every probe is bounded by the explained time, so
  # unbounded (non-terminating) rules are never enumerated forever.
  class Explanation
    attr_reader :schedule, :time

    def self.explain(schedule, time)
      new(schedule, time).to_h
    end

    def initialize(schedule, time)
      @schedule = schedule
      @time = TimeUtil.match_zone(time, schedule.start_time) or
        raise ArgumentError, "Time required, got #{time.inspect}"
    end

    def to_h
      reset_rules
      recurrence = recurrence_explanations
      exceptions = exception_explanations
      included = recurrence.any? { |entry| entry[:matched] } || implicit_start_occurrence?
      excluded = exceptions.any? { |entry| entry[:matched] }
      {
        time: time,
        occurs: included && !excluded,
        included: included,
        excluded: excluded,
        reasons: reasons(included, excluded, recurrence),
        recurrence: recurrence,
        exceptions: exceptions
      }
    ensure
      reset_rules
    end

    private

    def recurrence_explanations
      schedule.recurrence_rules.map { |rule| explain_rule(rule) } +
        schedule.recurrence_times.map { |time| explain_single(time, bounded_by_start: true) }
    end

    def exception_explanations
      schedule.exception_rules.map { |rule| explain_rule(rule) } +
        schedule.exception_times.map { |time| explain_single(time, bounded_by_start: false) }
    end

    def explain_rule(rule)
      lands = rule_lands_on_time?(rule)
      blocked_by = []
      limits = {}

      blocked_by << :start_time if before_start_time?

      if (until_time = rule.respond_to?(:until_time) && rule.until_time)
        exceeded = time > TimeUtil.ensure_time(until_time, schedule.start_time, true)
        limits[:until] = {time: until_time, exceeded: exceeded}
        blocked_by << :until if exceeded
      end

      if (count = rule.respond_to?(:occurrence_count) && rule.occurrence_count)
        used = occurrences_through(rule)
        limits[:count] = {max: count, used: used, exceeded: used >= count}
        blocked_by << :count if lands && used >= count
      end

      blocked_by << :conditions unless lands

      {
        kind: :rule,
        rule: rule,
        matched: lands && blocked_by.empty?,
        blocked_by: blocked_by,
        limits: limits,
        conditions: conditions_for(rule)
      }
    end

    def explain_single(single_time, bounded_by_start:)
      blocked_by = []
      blocked_by << :start_time if bounded_by_start && before_start_time?
      {
        kind: :single_occurrence,
        time: single_time,
        matched: single_time.to_i == time.to_i && blocked_by.empty?,
        blocked_by: blocked_by,
        limits: {},
        conditions: []
      }
    end

    # One entry per sub-condition (validation) of a composite rule, so UIs
    # can point at exactly which part of the rule accepted or rejected the
    # time. COUNT and UNTIL are reported under :limits instead.
    def conditions_for(rule)
      return [] unless rule.respond_to?(:validations)
      rule.validations.flat_map do |key, validations|
        next [] if key == :count || key == :until
        validations.map do |validation|
          offset = validation.validate(time, schedule.start_time)
          condition = {
            key: key,
            type: validation.type,
            matched: offset.nil? || offset == 0
          }
          condition[:value] = validation.value if validation.respond_to?(:value)
          condition[:offset] = offset if offset && offset != 0
          condition
        end
      end
    end

    # Whether the rule lands exactly on +time+, ignoring COUNT state.
    # Probing is bounded by +time+ itself and never consumes count uses.
    def rule_lands_on_time?(rule)
      result = rule.next_time(time, schedule.start_time, time, increment: false)
      !result.nil? && result.to_i == time.to_i
    rescue CountExceeded, UntilExceeded
      false
    end

    # Number of the rule's own occurrences in [start_time, time), i.e. how
    # many COUNT uses were consumed before the explained time. Bounded by
    # the explained time, so this is always a finite walk even for rules
    # without COUNT or UNTIL.
    def occurrences_through(rule)
      count = 0
      t = schedule.start_time
      loop do
        nxt = rule.next_time(t, schedule.start_time, time, increment: false)
        break if nxt.nil? || nxt.to_i >= time.to_i
        count += 1
        t = nxt + 1
      end
      count
    rescue CountExceeded, UntilExceeded
      count
    end

    def implicit_start_occurrence?
      schedule.recurrence_rules.empty? && time.to_i == schedule.start_time.to_i
    end

    def before_start_time?
      time < schedule.start_time
    end

    def reasons(included, excluded, recurrence)
      return [] if included && !excluded
      result = []
      result << :before_start_time if before_start_time?
      if included && excluded
        result << :excluded_by_exception
      elsif !included
        result << :no_matching_recurrence
        result << :count_exceeded if recurrence.any? { |entry| entry[:blocked_by].include?(:count) }
        result << :until_exceeded if recurrence.any? { |entry| entry[:blocked_by].include?(:until) }
      end
      result
    end

    def reset_rules
      (schedule.recurrence_rules + schedule.exception_rules).each(&:reset)
    end
  end
end
