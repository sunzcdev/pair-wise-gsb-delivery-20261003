require "yaml"

module IceCube
  class Schedule
    extend Deprecated

    # Get the start time
    attr_reader :start_time
    deprecated_alias :start_date, :start_time

    # Get the end time
    attr_reader :end_time
    deprecated_alias :end_date, :end_time

    # Create a new schedule
    def initialize(start_time = nil, options = {})
      self.start_time = start_time || TimeUtil.now
      self.end_time = self.start_time + options[:duration] if options[:duration]
      self.end_time = options[:end_time] if options[:end_time]
      @all_recurrence_rules = []
      @all_exception_rules = []
      yield self if block_given?
    end

    # Set start_time
    def start_time=(start_time)
      @start_time = TimeUtil.ensure_time start_time
    end
    deprecated_alias :start_date=, :start_time=

    # Set end_time
    def end_time=(end_time)
      @end_time = TimeUtil.ensure_time end_time
    end
    deprecated_alias :end_date=, :end_time=

    def duration
      end_time ? end_time - start_time : 0
    end

    def duration=(seconds)
      @end_time = start_time + seconds
    end

    # Add a recurrence time to the schedule
    def add_recurrence_time(time)
      return if time.nil?
      rule = SingleOccurrenceRule.new(time)
      add_recurrence_rule rule
      time
    end
    alias_method :rtime, :add_recurrence_time
    deprecated_alias :rdate, :rtime
    deprecated_alias :add_recurrence_date, :add_recurrence_time

    # Add an exception time to the schedule
    def add_exception_time(time)
      return if time.nil?
      rule = SingleOccurrenceRule.new(time)
      add_exception_rule rule
      time
    end
    alias_method :extime, :add_exception_time
    deprecated_alias :exdate, :extime
    deprecated_alias :add_exception_date, :add_exception_time

    # Add a recurrence rule to the schedule
    def add_recurrence_rule(rule)
      return if rule.nil?
      @all_recurrence_rules << rule unless @all_recurrence_rules.include?(rule)
    end
    alias_method :rrule, :add_recurrence_rule

    # Remove a recurrence rule
    def remove_recurrence_rule(rule)
      res = @all_recurrence_rules.delete(rule)
      res.nil? ? [] : [res]
    end

    # Add an exception rule to the schedule
    def add_exception_rule(rule)
      return if rule.nil?
      @all_exception_rules << rule unless @all_exception_rules.include?(rule)
    end
    alias_method :exrule, :add_exception_rule

    # Remove an exception rule
    def remove_exception_rule(rule)
      res = @all_exception_rules.delete(rule)
      res.nil? ? [] : [res]
    end

    # Get the recurrence rules
    def recurrence_rules
      @all_recurrence_rules.reject { |r| r.is_a?(SingleOccurrenceRule) }
    end
    alias_method :rrules, :recurrence_rules

    # Get the exception rules
    def exception_rules
      @all_exception_rules.reject { |r| r.is_a?(SingleOccurrenceRule) }
    end
    alias_method :exrules, :exception_rules

    # Get the recurrence times that are on the schedule
    def recurrence_times
      @all_recurrence_rules.select { |r| r.is_a?(SingleOccurrenceRule) }.map(&:time)
    end
    alias_method :rtimes, :recurrence_times
    deprecated_alias :rdates, :rtimes
    deprecated_alias :recurrence_dates, :recurrence_times

    # Remove a recurrence time
    def remove_recurrence_time(time)
      found = false
      @all_recurrence_rules.delete_if do |rule|
        found = true if rule.is_a?(SingleOccurrenceRule) && rule.time == time
      end
      time if found
    end
    alias_method :remove_rtime, :remove_recurrence_time
    deprecated_alias :remove_recurrence_date, :remove_recurrence_time
    deprecated_alias :remove_rdate, :remove_rtime

    # Get the exception times that are on the schedule
    def exception_times
      @all_exception_rules.select { |r| r.is_a?(SingleOccurrenceRule) }.map(&:time)
    end
    alias_method :extimes, :exception_times
    deprecated_alias :exdates, :extimes
    deprecated_alias :exception_dates, :exception_times

    # Remove an exception time
    def remove_exception_time(time)
      found = false
      @all_exception_rules.delete_if do |rule|
        found = true if rule.is_a?(SingleOccurrenceRule) && rule.time == time
      end
      time if found
    end
    alias_method :remove_extime, :remove_exception_time
    deprecated_alias :remove_exception_date, :remove_exception_time
    deprecated_alias :remove_exdate, :remove_extime

    # Get all of the occurrences from the start_time up until a
    # given Time
    def occurrences(closing_time)
      enumerate_occurrences(start_time, closing_time).to_a
    end

    # All of the occurrences
    def all_occurrences
      require_terminating_rules
      enumerate_occurrences(start_time).to_a
    end

    # Emit an enumerator based on the start time
    def all_occurrences_enumerator
      enumerate_occurrences(start_time)
    end

    # Iterate forever
    def each_occurrence(&block)
      enumerate_occurrences(start_time, &block).to_a
      self
    end

    # The next n occurrences after now
    def next_occurrences(num, from = nil, options = {})
      from = TimeUtil.match_zone(from, start_time) || TimeUtil.now(start_time)
      enumerate_occurrences(from + 1, nil, options).take(num)
    end

    # The next occurrence after now (overridable)
    def next_occurrence(from = nil, options = {})
      from = TimeUtil.match_zone(from, start_time) || TimeUtil.now(start_time)
      enumerate_occurrences(from + 1, nil, options).next
    rescue StopIteration
      nil
    end

    # The previous occurrence from a given time
    def previous_occurrence(from)
      from = TimeUtil.match_zone(from, start_time) or raise ArgumentError, "Time required, got #{from.inspect}"
      return nil if from <= start_time
      enumerate_occurrences(start_time, from - 1).to_a.last
    end

    # The previous n occurrences before a given time
    def previous_occurrences(num, from)
      from = TimeUtil.match_zone(from, start_time) or raise ArgumentError, "Time required, got #{from.inspect}"
      return [] if from <= start_time
      a = enumerate_occurrences(start_time, from - 1).to_a
      (a.size > num) ? a[-1 * num, a.size] : a
    end

    # The remaining occurrences (same requirements as all_occurrences)
    def remaining_occurrences(from = nil, options = {})
      require_terminating_rules
      from ||= TimeUtil.now(@start_time)
      enumerate_occurrences(from, nil, options).to_a
    end

    # Returns an enumerator for all remaining occurrences
    def remaining_occurrences_enumerator(from = nil, options = {})
      from ||= TimeUtil.now(@start_time)
      enumerate_occurrences(from, nil, options)
    end

    # Occurrences between two times
    def occurrences_between(begin_time, closing_time, options = {})
      enumerate_occurrences(begin_time, closing_time, options).to_a
    end

    # Return a boolean indicating if an occurrence falls between two times
    def occurs_between?(begin_time, closing_time, options = {})
      enumerate_occurrences(begin_time, closing_time, options).next
      true
    rescue StopIteration
      false
    end

    # Return a boolean indicating if an occurrence is occurring between two
    # times, inclusive of its duration. This counts zero-length occurrences
    # that intersect the start of the range and within the range, but not
    # occurrences at the end of the range since none of their duration
    # intersects the range.
    def occurring_between?(opening_time, closing_time)
      occurs_between?(opening_time, closing_time, spans: true)
    end

    # Return a boolean indicating if an occurrence falls on a certain date
    def occurs_on?(date)
      date = TimeUtil.ensure_date(date)
      begin_time = TimeUtil.beginning_of_date(date, start_time)
      closing_time = TimeUtil.end_of_date(date, start_time)
      occurs_between?(begin_time, closing_time)
    end

    # Determine if the schedule is occurring at a given time
    def occurring_at?(time)
      time = TimeUtil.match_zone(time, start_time) or raise ArgumentError, "Time required, got #{time.inspect}"
      if duration > 0
        return false if exception_time?(time)
        occurs_between?(time - duration + 1, time)
      else
        occurs_at?(time)
      end
    end

    # Determine if this schedule conflicts with another schedule
    # @param [IceCube::Schedule] other_schedule - The schedule to compare to
    # @param [Time] closing_time - the last time to consider
    # @return [Boolean] whether or not the schedules conflict at all
    def conflicts_with?(other_schedule, closing_time = nil)
      closing_time = TimeUtil.ensure_time(closing_time)
      unless terminating? || other_schedule.terminating? || closing_time
        raise ArgumentError, "One or both schedules must be terminating to use #conflicts_with?"
      end
      # Pick the terminating schedule, and other schedule
      # No need to reverse if terminating? or there is a closing time
      terminating_schedule = self
      unless terminating? || closing_time
        terminating_schedule, other_schedule = other_schedule, terminating_schedule
      end
      # Go through each occurrence of the terminating schedule and determine
      # if the other occurs at that time
      #
      last_time = nil
      terminating_schedule.each_occurrence do |time|
        if closing_time && time > closing_time
          last_time = closing_time
          break
        end
        last_time = time
        return true if other_schedule.occurring_at?(time)
      end
      # Due to durations, we need to walk up to the end time, and verify in the
      # other direction
      if last_time
        last_time += terminating_schedule.duration
        other_schedule.each_occurrence do |time|
          break if time > last_time
          return true if terminating_schedule.occurring_at?(time)
        end
      end
      # No conflict, return false
      false
    end

    # Determine if the schedule occurs at a specific time
    def occurs_at?(time)
      occurs_between?(time, time)
    end

    # Get the first n occurrences, or the first occurrence if n is skipped
    def first(n = nil)
      occurrences = enumerate_occurrences(start_time).take(n || 1)
      n.nil? ? occurrences.first : occurrences
    end

    # Get the final n occurrences of a terminating schedule
    # or the final one if no n is given
    def last(n = nil)
      require_terminating_rules
      occurrences = enumerate_occurrences(start_time).to_a
      n.nil? ? occurrences.last : occurrences[-n..]
    end

    # The default maximum number of occurrences that ordinal lookups will
    # walk when the schedule's rules can't be computed arithmetically.
    DEFAULT_OCCURRENCE_BUDGET = 10_000

    # Locate the nth occurrence (1-based) of the schedule in the final,
    # deduplicated sequence of occurrences - the same ordering produced by
    # enumerating from the start time. Returns an Occurrence, or nil when
    # the schedule has fewer than n occurrences.
    #
    # Schedules reducing to a single pure-interval rule (with optional
    # COUNT/UNTIL) are computed arithmetically in constant time. Any other
    # combination (multiple or overlapping rules, exception rules or
    # times, BYxxx selections) falls back to walking the sequence, which
    # is limited to +budget+ occurrences and raises BudgetExceeded when
    # that isn't enough. Pass a larger :budget, or budget: nil to allow
    # an unbounded walk.
    def occurrence_at(n, budget: DEFAULT_OCCURRENCE_BUDGET)
      unless n.is_a?(Integer) && n >= 1
        raise ArgumentError, "Expecting a positive Integer ordinal, got #{n.inspect}"
      end
      case (step = fast_path_step)
      when :single_occurrence
        (n == 1) ? Occurrence.new(start_time, start_time + duration) : nil
      when nil
        occurrence_at_by_enumeration(n, budget)
      else
        unit, amount, dst_adjust, rule = step
        fast_path_occurrence_at(n, unit, amount, dst_adjust, rule)
      end
    end

    # Count how many occurrences of the final, deduplicated sequence fall
    # strictly before the given time. Uses the same arithmetic fast path
    # and budgeted enumeration fallback as #occurrence_at.
    def occurrences_before(time, budget: DEFAULT_OCCURRENCE_BUDGET)
      time = TimeUtil.match_zone(time, start_time) or
        raise ArgumentError, "Time required, got #{time.inspect}"
      case (step = fast_path_step)
      when :single_occurrence
        (time > start_time) ? 1 : 0
      when nil
        occurrences_before_by_enumeration(time, budget)
      else
        unit, amount, dst_adjust, rule = step
        fast_path_occurrences_before(time, unit, amount, dst_adjust, rule)
      end
    end

    # String serialization
    def to_s
      pieces = []
      rd = recurrence_times_with_start_time - extimes
      pieces.concat rd.sort.map { |t| IceCube::I18n.l(t, format: IceCube.to_s_time_format) }
      pieces.concat rrules.map { |t| t.to_s }
      pieces.concat exrules.map { |t| IceCube::I18n.t("ice_cube.not", target: t.to_s) }
      pieces.concat extimes.sort.map { |t|
        target = IceCube::I18n.l(t, format: IceCube.to_s_time_format)
        IceCube::I18n.t("ice_cube.not_on", target: target)
      }
      pieces.join(IceCube::I18n.t("ice_cube.pieces_connector"))
    end

    # Serialize this schedule to_ical
    def to_ical(force_utc = false)
      pieces = []
      pieces << "DTSTART#{IcalBuilder.ical_format(start_time, force_utc)}"
      pieces.concat recurrence_rules.map { |r| "RRULE:#{r.to_ical}" }
      pieces.concat exception_rules.map { |r| "EXRULE:#{r.to_ical}" }
      pieces.concat recurrence_times_without_start_time.map { |t| "RDATE#{IcalBuilder.ical_format(t, force_utc)}" }
      pieces.concat exception_times.map { |t| "EXDATE#{IcalBuilder.ical_format(t, force_utc)}" }
      pieces << "DTEND#{IcalBuilder.ical_format(end_time, force_utc)}" if end_time
      pieces.join("\n")
    end

    # Load the schedule from ical
    def self.from_ical(ical, options = {})
      IcalParser.schedule_from_ical(ical, options)
    end

    # Hook for YAML.dump, enables to_yaml
    def encode_with(coder)
      coder.represent_object nil, to_hash
    end

    # Load the schedule from yaml
    def self.from_yaml(yaml, options = {})
      YamlParser.new(yaml).to_schedule do |schedule|
        Deprecated.schedule_options(schedule, options)
        yield schedule if block_given?
      end
    end

    # Convert the schedule to a hash
    def to_hash
      data = {}
      data[:start_time] = TimeUtil.serialize_time(start_time)
      data[:start_date] = data[:start_time] if IceCube.compatibility <= 11
      data[:end_time] = TimeUtil.serialize_time(end_time) if end_time
      data[:rrules] = recurrence_rules.map(&:to_hash)
      data[:exrules] = exception_rules.map(&:to_hash) if exception_rules.any?
      data[:rtimes] = recurrence_times.map do |rt|
        TimeUtil.serialize_time(rt)
      end
      data[:extimes] = exception_times.map do |et|
        TimeUtil.serialize_time(et)
      end
      data
    end
    alias_method :to_h, :to_hash

    # Load the schedule from a hash
    def self.from_hash(original_hash, options = {})
      HashParser.new(original_hash).to_schedule do |schedule|
        Deprecated.schedule_options(schedule, options)
        yield schedule if block_given?
      end
    end

    # Determine if the schedule will end
    # @return [Boolean] true if ending, false if repeating forever
    def terminating?
      @all_recurrence_rules.all?(&:terminating?)
    end

    def hash
      [
        TimeUtil.hash(start_time), duration,
        *@all_recurrence_rules.map(&:hash).sort!,
        *@all_exception_rules.map(&:hash).sort!
      ].hash
    end

    def eql?(other)
      hash == other.hash
    end
    alias_method :==, :eql?

    def self.dump(schedule)
      return schedule if schedule.nil? || schedule == ""
      schedule.to_yaml
    end

    def self.load(yaml)
      return yaml if yaml.nil? || yaml == ""
      from_yaml(yaml)
    end

    private

    # Reset all rules for another run
    def reset
      @all_recurrence_rules.each(&:reset)
      @all_exception_rules.each(&:reset)
    end

    # Find all of the occurrences for the schedule between opening_time
    # and closing_time
    # Iteration is unrolled in pairs to skip duplicate times in end of DST
    def enumerate_occurrences(opening_time, closing_time = nil, options = {})
      opening_time = TimeUtil.match_zone(opening_time, start_time)
      closing_time = TimeUtil.match_zone(closing_time, start_time)
      opening_time += TimeUtil.subsec(start_time) - TimeUtil.subsec(opening_time)
      opening_time = start_time if opening_time < start_time
      spans = options[:spans] == true && duration != 0
      Enumerator.new do |yielder|
        reset
        t1 = full_required? ? start_time : opening_time
        t1 -= duration if spans
        t1 = start_time if t1 < start_time
        loop do
          break unless (t0 = next_time(t1, closing_time))
          break if closing_time && t0 > closing_time
          if spans ? (t0.end_time > opening_time) : (t0 >= opening_time)
            yielder << (block_given? ? yield(t0) : t0)
          end
          t1 = t0 + 1
        end
      end
    end

    # Get the next time after (or including) a specific time
    def next_time(time, closing_time)
      loop do
        # Probe all rules without consuming counts so we can pick the earliest
        # candidate, then charge counts only to rules that emitted that time.
        min_time = nil
        candidates = []
        recurrence_rules_with_implicit_start_occurrence.each do |rule|
          candidate = rule.next_time(time, start_time, min_time || closing_time, increment: false)
          next unless candidate
          candidates << [rule, candidate]
          min_time = candidate if min_time.nil? || candidate < min_time
        rescue StopIteration
          next
        end
        break unless min_time
        candidates.each do |rule, candidate|
          rule.increment_uses if candidate == min_time && rule.respond_to?(:increment_uses)
        end
        next (time = min_time + 1) if exception_time?(min_time)
        break Occurrence.new(min_time, min_time + duration)
      end
    end

    # Indicate if any rule needs to be run from the start of time
    # If we have rules with counts, we need to walk from the beginning of time
    def full_required?
      @all_recurrence_rules.any?(&:full_required?) ||
        @all_exception_rules.any?(&:full_required?)
    end

    # Return a boolean indicating whether or not a specific time
    # is excluded from the schedule
    def exception_time?(time)
      @all_exception_rules.any? do |rule|
        rule.on?(time, start_time)
      end
    end

    def require_terminating_rules
      return true if terminating?
      method_name = caller(1..1).first.split(" ").last
      raise ArgumentError, "All recurrence rules must specify .until or .count to use #{method_name}"
    end

    def implicit_start_occurrence_rule
      SingleOccurrenceRule.new(start_time)
    end

    def recurrence_times_without_start_time
      recurrence_times.reject { |t| t == start_time }
    end

    def recurrence_times_with_start_time
      if recurrence_rules.empty?
        [start_time].concat recurrence_times_without_start_time
      else
        recurrence_times
      end
    end

    def recurrence_rules_with_implicit_start_occurrence
      if recurrence_rules.empty?
        [implicit_start_occurrence_rule].concat @all_recurrence_rules
      else
        @all_recurrence_rules
      end
    end

    # Returns the arithmetic shape of the schedule when its occurrences
    # can be computed directly: :single_occurrence for a schedule with no
    # recurrence rules, or [unit, step, dst_adjust, rule] for a lone
    # pure-interval rule. Returns nil when the combination of rules,
    # recurrence times or exceptions requires enumeration.
    def fast_path_step
      return nil unless @all_exception_rules.empty?
      return :single_occurrence if @all_recurrence_rules.empty?
      return nil unless @all_recurrence_rules.size == 1
      rule = @all_recurrence_rules.first
      return nil unless rule.is_a?(ValidatedRule)
      step = rule.fast_path_step
      step && [*step, rule]
    end

    # The time of the kth (0-based) occurrence of a pure-interval rule,
    # computed directly from the start time.
    def fast_path_time_at(k, unit, step, dst_adjust)
      wrapper = TimeUtil::TimeWrapper.new(start_time, dst_adjust)
      wrapper.add(unit, step * k)
      wrapper.to_time
    end

    def fast_path_occurrence_at(n, unit, step, dst_adjust, rule)
      count = rule.occurrence_count
      return nil if count && n > count
      time = fast_path_time_at(n - 1, unit, step, dst_adjust)
      until_time = rule.until_time
      if until_time && time > TimeUtil.ensure_time(until_time, start_time, true)
        return nil
      end
      Occurrence.new(time, time + duration)
    end

    def fast_path_occurrences_before(time, unit, step, dst_adjust, rule)
      return 0 if time <= start_time
      count = fast_path_count_before(time, unit, step, dst_adjust)
      if (max = rule.occurrence_count) && count > max
        count = max
      end
      if (until_time = rule.until_time)
        until_time = TimeUtil.ensure_time(until_time, start_time, true)
        total = fast_path_count_before(until_time, unit, step, dst_adjust)
        total += 1 if fast_path_time_at(total, unit, step, dst_adjust) <= until_time
        count = total if count > total
      end
      count
    end

    # The number of pure-interval occurrences strictly before +time+,
    # ignoring COUNT/UNTIL limits. An approximate index is derived
    # arithmetically and then corrected exactly; the estimate is only ever
    # off by a step or two (clock-of-day, month lengths, DST), so this
    # stays constant time regardless of how far +time+ is in the future.
    def fast_path_count_before(time, unit, step, dst_adjust)
      k = fast_path_index_estimate(time, unit, step)
      k = 0 if k < 0
      k += 1 while fast_path_time_at(k, unit, step, dst_adjust) < time
      k -= 1 while k > 0 && fast_path_time_at(k - 1, unit, step, dst_adjust) >= time
      k
    end

    def fast_path_index_estimate(time, unit, step)
      case unit
      when :sec
        ((time - start_time) / step).ceil
      when :min
        ((time - start_time) / (step * ONE_MINUTE)).ceil
      when :hour
        ((time - start_time) / (step * ONE_HOUR)).ceil
      when :day
        ((time.to_date - start_time.to_date) / step.to_f).ceil
      when :month
        months = (time.year - start_time.year) * 12 + time.month - start_time.month
        (months / step.to_f).ceil
      when :year
        ((time.year - start_time.year) / step.to_f).ceil
      end
    end

    def occurrence_at_by_enumeration(n, budget)
      validate_ordinal_budget!(budget)
      result = nil
      walked = 0
      enumerate_occurrences(start_time).each do |occurrence|
        walked += 1
        check_ordinal_budget!(walked, budget, "occurrence_at(#{n})")
        if walked == n
          result = occurrence
          break
        end
      end
      result
    end

    def occurrences_before_by_enumeration(time, budget)
      validate_ordinal_budget!(budget)
      count = 0
      walked = 0
      enumerate_occurrences(start_time, time).each do |occurrence|
        walked += 1
        check_ordinal_budget!(walked, budget, "occurrences_before(#{time})")
        break unless occurrence < time
        count += 1
      end
      count
    end

    def validate_ordinal_budget!(budget)
      return if budget.nil? || (budget.is_a?(Integer) && budget >= 1)
      raise ArgumentError, "Expecting a positive Integer or nil budget, got #{budget.inspect}"
    end

    def check_ordinal_budget!(walked, budget, description)
      return unless budget && walked > budget
      raise BudgetExceeded,
        "#{description} cannot be computed arithmetically for this " \
        "combination of rules and walking more than #{budget} occurrences " \
        "exceeds the budget; pass a larger :budget or budget: nil to allow " \
        "the enumeration fallback"
    end
  end
end
