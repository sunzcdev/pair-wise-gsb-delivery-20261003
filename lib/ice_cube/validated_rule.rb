require "ice_cube/input_alignment"

module IceCube
  class ValidatedRule < Rule
    include Validations::ScheduleLock

    include Validations::Count
    include Validations::Until

    # Validations ordered for efficiency in sequence of:
    # * descending intervals
    # * boundary limits
    # * base values by cardinality (n = 60, 60, 31, 24, 12, 7)
    # * locks by cardinality (n = 365, 60, 60, 31, 24, 12, 7)
    # * interval multiplier
    VALIDATION_ORDER = [
      :year, :month, :day, :wday, :hour, :min, :sec, :count, :until,
      :base_sec, :base_min, :base_day, :base_hour, :base_month, :base_wday,
      :day_of_year, :second_of_minute, :minute_of_hour, :day_of_month,
      :hour_of_day, :month_of_year, :day_of_week,
      :interval,
      # BYSETPOS selects the nth occurrence within the set after all other
      # BYxxx filters/expansions are applied (RFC 5545), so it must run last.
      :by_set_pos
    ]

    attr_reader :validations

    def initialize(interval = 1)
      @validations = {}
    end

    # Reset the uses on the rule to 0
    def reset
      @time = nil
      @start_time = nil
      @uses = 0
    end

    def base_interval_validation
      @validations[:interval].first
    end

    def other_interval_validations
      Array(@validations[base_interval_validation.type])
    end

    # Compute the next time after (or including) the specified time in respect
    # to the given start time
    # When increment is false, callers are probing for the next candidate and
    # must not consume COUNT.
    def next_time(time, start_time, closing_time, increment: true)
      @time = time
      @start_time ||= realign(time, start_time)
      @time = @start_time if @time < @start_time

      return nil unless find_acceptable_time_before(closing_time)

      @uses += 1 if @time && increment
      @time
    end

    def increment_uses
      # Count is consumed only when the rule's occurrence is emitted.
      @uses += 1
    end

    def realign(opening_time, start_time)
      start_time
    end

    def full_required?
      !occurrence_count.nil?
    end

    def to_s
      builder = StringBuilder.new
      @validations.each_value do |validations|
        validations.each do |validation|
          validation.build_s(builder)
        end
      end
      builder.to_s
    end

    def to_hash
      builder = HashBuilder.new(self)
      @validations.each_value do |validations|
        validations.each do |validation|
          validation.build_hash(builder)
        end
      end
      builder.to_hash
    end

    def to_ical
      builder = IcalBuilder.new
      @validations.each_value do |validations|
        validations.each do |validation|
          validation.build_ical(builder)
        end
      end
      builder.to_s
    end

    # Get the collection that contains validations of a certain type
    def validations_for(key)
      @validations[key] ||= []
    end

    # Explain whether this rule generates an occurrence at the given time,
    # and if not, why not. Returns structured data (no localized strings) so
    # that interfaces can translate the result themselves:
    #
    #   {
    #     rule: self,
    #     matched: true/false,
    #     blockers: [:before_start_time, :condition_mismatch,
    #                :count_exceeded, :until_exceeded],
    #     conditions: [{name:, index:, matched:, offset:, expected:, actual:}],
    #     limits: {count: {limit:, used:, remaining:, exceeded:},
    #              until: {time:, exceeded:}}
    #   }
    #
    # The rule is never mutated: COUNT usage is replayed on a disposable copy
    # rebuilt from #to_hash, and only for rules carrying a COUNT validation,
    # so the replay terminates after at most +count+ emissions and never
    # walks an unbounded sequence.
    def explain(time, start_time)
      effective_start = full_required? ? realign(start_time, start_time) : realign(time, start_time)
      blockers = []
      conditions = []
      limits = {}

      blockers << :before_start_time if time < effective_start

      validation_names.each do |name|
        group = []
        @validations[name].each_with_index do |validation, index|
          if validation.type == :limit
            limits[name] = explain_limit(name, validation, time, effective_start)
            blockers << :"#{name}_exceeded" if limits[name][:exceeded]
          else
            group << explain_condition(name, index, validation, time, effective_start)
          end
        end
        # Validations within a type group are alternatives: the group accepts
        # the time as soon as any of its validations does.
        unless group.empty? || group.any? { |condition| condition[:matched] }
          blockers << :condition_mismatch
        end
        conditions.concat(group)
      end

      {
        rule: self,
        matched: blockers.empty?,
        blockers: blockers.uniq,
        conditions: conditions,
        limits: limits
      }
    end

    # Fully replace validations
    def replace_validations_for(key, arr)
      if arr.nil?
        @validations.delete(key)
      else
        @validations[key] = arr
      end
    end

    # Remove the specified base validations
    def clobber_base_validations(*types)
      types.each do |type|
        @validations.delete(:"base_#{type}")
      end
    end

    private

    def normalized_interval(interval)
      int = interval.to_i
      raise ArgumentError, "'#{interval}' is not a valid input for interval. Please pass a postive integer." unless int > 0
      int
    end

    def finds_acceptable_time?
      validation_names.all? do |type|
        validation_accepts_or_updates_time?(@validations[type])
      end
    end

    # Evaluate a single non-limit validation against +time+ and describe the
    # outcome. The +name+ and +index+ pair locates the sub-condition within
    # the rule so callers can point at the exact failing part.
    def explain_condition(name, index, validation, time, start_time)
      offset = validation.validate(time, start_time)
      matched = offset.nil? || offset == 0
      condition = {name: name, index: index, matched: matched}
      condition[:offset] = offset unless matched
      if validation.respond_to?(:value)
        expected = validation.value
        expected = start_time.send(validation.type) if expected.nil? && time.respond_to?(validation.type)
        condition[:expected] = expected
        condition[:actual] = time.send(validation.type) if time.respond_to?(validation.type)
      elsif validation.respond_to?(:occ)
        condition[:expected] = {day: validation.day, occurrence: validation.occ}
      elsif validation.respond_to?(:interval)
        condition[:expected] = validation.interval
      elsif validation.respond_to?(:by_set_pos)
        condition[:expected] = validation.by_set_pos
      end
      condition
    end

    # Describe a COUNT or UNTIL limit at the given time.
    def explain_limit(name, validation, time, start_time)
      case name
      when :count
        used = count_uses_before(time, start_time)
        {
          limit: validation.count,
          used: used,
          remaining: [validation.count - used, 0].max,
          exceeded: used >= validation.count
        }
      when :until
        end_time = TimeUtil.ensure_time(validation.time, start_time, true)
        {time: end_time, exceeded: time > end_time}
      end
    end

    # Number of occurrences this rule emits strictly before +time+. Replayed
    # on a disposable copy so the rule's own enumeration state is untouched.
    # Only called for rules with a COUNT validation, so the replay stops
    # after at most +count+ emissions (CountExceeded) and never traverses an
    # unbounded sequence.
    def count_uses_before(time, start_time)
      copy = Rule.from_hash(to_hash)
      copy.reset
      uses = 0
      t = start_time
      while (candidate = copy.next_time(t, start_time, time - 1))
        break if candidate >= time
        uses += 1
        t = candidate + 1
      end
      uses
    rescue CountExceeded, UntilExceeded
      uses
    end

    def find_acceptable_time_before(boundary)
      until finds_acceptable_time?
        return false if past_closing_time?(boundary)
      end
      true
    end

    # Returns true if all validations for the current rule match
    # otherwise false and shifts to the first (largest) unmatched offset
    #
    def validation_accepts_or_updates_time?(validations_for_type)
      res = validations_for_type.each_with_object([]) do |validation, offsets|
        r = validation.validate(@time, @start_time)
        return true if r.nil? || r == 0
        offsets << r
      end
      shift_time_by_validation(res, validations_for_type.first)
      false
    end

    def shift_time_by_validation(res, validation)
      return unless (interval = res.min)
      wrapper = TimeUtil::TimeWrapper.new(@time, validation.dst_adjust?)
      wrapper.add(validation.type, interval)
      wrapper.clear_below(validation.type)

      # Move over DST if blocked, no adjustments
      if wrapper.to_time <= @time
        wrapper = TimeUtil::TimeWrapper.new(wrapper.to_time, false)
        until wrapper.to_time > @time
          wrapper.add(:min, 10) # smallest interval
        end
      end

      # And then get the correct time out
      @time = wrapper.to_time
    end

    def past_closing_time?(closing_time)
      closing_time && @time > closing_time
    end

    def validation_names
      VALIDATION_ORDER & @validations.keys
    end

    def verify_alignment(value, freq, rule_part)
      InputAlignment.new(self, value, rule_part).verify(freq) do |error|
        yield error
      end
    end
  end
end
