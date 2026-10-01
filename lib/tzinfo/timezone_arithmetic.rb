# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  class Timezone
    # The valid values of the `gap` parameter of {add_local_days} and
    # {add_local_months}.
    #
    # @!visibility private
    LOCAL_GAP_STRATEGIES = [:raise, :forward, :backward].freeze
    private_constant :LOCAL_GAP_STRATEGIES

    # The valid values of the `ambiguous` parameter of {add_local_days} and
    # {add_local_months}.
    #
    # @!visibility private
    LOCAL_AMBIGUOUS_STRATEGIES = [:raise, :first, :last, :dst, :standard].freeze
    private_constant :LOCAL_AMBIGUOUS_STRATEGIES

    # The valid values of the `month_overflow` parameter of
    # {add_local_months}.
    #
    # @!visibility private
    MONTH_OVERFLOW_STRATEGIES = [:clamp, :overflow, :raise].freeze
    private_constant :MONTH_OVERFLOW_STRATEGIES

    # The number of days in each month of a non-leap year.
    #
    # @!visibility private
    DAYS_IN_MONTH_NON_LEAP = [31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31].freeze
    private_constant :DAYS_IN_MONTH_NON_LEAP

    # Moves a time forwards (or backwards) by a number of elapsed seconds and
    # returns the result as a local time for this time zone.
    #
    # This is an absolute (elapsed time) calculation: the result is always
    # exactly `seconds` seconds after `time`, regardless of any offset
    # transitions that occur in between. The local wall clock reading of the
    # result will therefore not necessarily be `seconds` seconds later than
    # the wall clock reading of `time`. For example, adding 86,400 seconds (24
    # elapsed hours) across a transition that moves clocks forward by 30
    # minutes will produce a result with a wall clock reading 24 hours and 30
    # minutes later than the input:
    #
    #     tz = TZInfo::Timezone.get('Australia/Lord_Howe')
    #     t = tz.local_time(2016, 10, 1, 12, 0, 0)
    #     tz.add_seconds(t, 86400)   # => 2016-10-02 12:30:00 +11:00
    #     tz.add_local_days(t, 1)    # => 2016-10-02 12:00:00 +11:00
    #
    # Use {add_local_days} or {add_local_months} instead to keep the local
    # wall clock reading fixed.
    #
    # The result is of the same type as `time`: {TimeWithOffset} for a `Time`,
    # {DateTimeWithOffset} for a `DateTime` and {TimestampWithOffset} for a
    # {Timestamp}. The UTC offset and associated {TimezoneOffset} of the
    # result reflect the offset observed by this time zone at the resulting
    # instant. Any sub-second component of `time` is preserved.
    #
    # The system (local) time zone is never consulted. The UTC offset of
    # `time` is only used to establish the instant it represents; all other
    # calculations are performed using the offset data of this {Timezone}.
    #
    # @param time [Object] a `Time`, `DateTime` or {Timestamp} to be moved.
    # @param seconds [Integer] the number of elapsed seconds to move by (may
    #   be negative).
    # @return [Object] `time` moved by `seconds` elapsed seconds, as a
    #   {TimeWithOffset}, {DateTimeWithOffset} or {TimestampWithOffset}
    #   (matching the type of `time`).
    # @raise [ArgumentError] if `time` is `nil`.
    # @raise [ArgumentError] if `seconds` is not an `Integer`.
    # @raise [ArgumentError] if `time` is a {Timestamp} with an unspecified
    #   UTC offset.
    def add_seconds(time, seconds)
      raise ArgumentError, 'time must be specified' unless time
      raise ArgumentError, 'seconds must be an Integer' unless seconds.kind_of?(Integer)

      Timestamp.for(time) do |ts|
        raise ArgumentError, 'time must have a specified utc_offset' unless ts.utc_offset
        to_local(ts.add_and_set_utc_offset(seconds, :utc))
      end
    end

    # Moves a time forwards (or backwards) by a number of whole local calendar
    # days, keeping the local wall clock time-of-day unchanged, and returns
    # the result as a local time for this time zone.
    #
    # This is a calendar (local time) calculation, not an elapsed time
    # calculation: if an offset transition occurs in between, the result will
    # not be exactly `days * 86,400` seconds after `time`. Use {add_seconds}
    # instead to move by an exact number of elapsed seconds.
    #
    # If the resulting local time does not exist (because of a forward offset
    # change), the `gap` parameter selects how it is resolved:
    #
    # - `:raise` (the default) - a {PeriodNotFound} exception is raised.
    # - `:forward` - the result is moved forward by the length of the gap (for
    #   example, if 02:15 does not exist because clocks moved forward from
    #   02:00 to 02:30, the result is 02:45).
    # - `:backward` - the result is moved backward by the length of the gap
    #   (for example, if 02:15 does not exist because clocks moved forward
    #   from 02:00 to 02:30, the result is 01:45).
    #
    # If the resulting local time occurs twice (because of a backward offset
    # change), the `ambiguous` parameter selects how it is resolved:
    #
    # - `:raise` (the default) - an {AmbiguousTime} exception is raised.
    # - `:first` - the earlier (in UTC) of the occurrences is selected.
    # - `:last` - the later (in UTC) of the occurrences is selected.
    # - `:dst` - the occurrence observing daylight savings time is selected.
    # - `:standard` - the occurrence observing standard time is selected.
    #
    # If `:dst` or `:standard` is selected but does not resolve the ambiguity
    # (for example, if neither of the candidate periods observes daylight
    # savings time), an {AmbiguousTime} exception is raised.
    #
    # The result is of the same type as `time`: {TimeWithOffset} for a `Time`,
    # {DateTimeWithOffset} for a `DateTime` and {TimestampWithOffset} for a
    # {Timestamp}. The UTC offset and associated {TimezoneOffset} of the
    # result reflect the offset observed by this time zone at the resulting
    # instant. Any sub-second component of `time` is preserved.
    #
    # The system (local) time zone is never consulted. The UTC offset of
    # `time` is only used to establish the instant it represents; all other
    # calculations are performed using the offset data of this {Timezone}.
    #
    # @param time [Object] a `Time`, `DateTime` or {Timestamp} to be moved.
    # @param days [Integer] the number of local calendar days to move by (may
    #   be negative).
    # @param gap [Symbol] how to resolve a resulting local time that does not
    #   exist: `:raise`, `:forward` or `:backward`.
    # @param ambiguous [Symbol] how to resolve a resulting local time that
    #   occurs twice: `:raise`, `:first`, `:last`, `:dst` or `:standard`.
    # @return [Object] `time` moved by `days` local calendar days, as a
    #   {TimeWithOffset}, {DateTimeWithOffset} or {TimestampWithOffset}
    #   (matching the type of `time`).
    # @raise [ArgumentError] if `time` is `nil`.
    # @raise [ArgumentError] if `days` is not an `Integer`.
    # @raise [ArgumentError] if `gap` or `ambiguous` is not one of the
    #   supported values.
    # @raise [ArgumentError] if `time` is a {Timestamp} with an unspecified
    #   UTC offset.
    # @raise [PeriodNotFound] if the resulting local time does not exist and
    #   `gap` is `:raise`.
    # @raise [AmbiguousTime] if the resulting local time occurs twice and
    #   `ambiguous` did not resolve the ambiguity.
    def add_local_days(time, days, gap: :raise, ambiguous: :raise)
      raise ArgumentError, 'time must be specified' unless time
      raise ArgumentError, 'days must be an Integer' unless days.kind_of?(Integer)
      validate_strategy!(:gap, gap, LOCAL_GAP_STRATEGIES)
      validate_strategy!(:ambiguous, ambiguous, LOCAL_AMBIGUOUS_STRATEGIES)

      shift_local_calendar(time, days, 0, :clamp, gap, ambiguous)
    end

    # Moves a time forwards (or backwards) by a number of whole local calendar
    # months, keeping the local wall clock time-of-day unchanged, and returns
    # the result as a local time for this time zone.
    #
    # This is a calendar (local time) calculation, not an elapsed time
    # calculation: the day-of-month, time-of-day and sub-second components of
    # the local wall clock reading are carried over to the resulting month
    # (subject to the `month_overflow` handling described below). Use
    # {add_seconds} instead to move by an exact number of elapsed seconds.
    #
    # If the resulting month has fewer days than the day-of-month of the local
    # date of `time`, the `month_overflow` parameter selects how the overflow
    # is handled:
    #
    # - `:clamp` (the default) - the day-of-month is clamped to the last day
    #   of the resulting month (for example, 31 January plus 1 month becomes
    #   28 or 29 February).
    # - `:overflow` - the excess days are carried over into the following
    #   month (for example, 31 January plus 1 month becomes 2 or 3 March).
    # - `:raise` - an `ArgumentError` is raised.
    #
    # If the resulting local time does not exist (because of a forward offset
    # change), the `gap` parameter selects how it is resolved:
    #
    # - `:raise` (the default) - a {PeriodNotFound} exception is raised.
    # - `:forward` - the result is moved forward by the length of the gap.
    # - `:backward` - the result is moved backward by the length of the gap.
    #
    # If the resulting local time occurs twice (because of a backward offset
    # change), the `ambiguous` parameter selects how it is resolved:
    #
    # - `:raise` (the default) - an {AmbiguousTime} exception is raised.
    # - `:first` - the earlier (in UTC) of the occurrences is selected.
    # - `:last` - the later (in UTC) of the occurrences is selected.
    # - `:dst` - the occurrence observing daylight savings time is selected.
    # - `:standard` - the occurrence observing standard time is selected.
    #
    # If `:dst` or `:standard` is selected but does not resolve the ambiguity
    # (for example, if neither of the candidate periods observes daylight
    # savings time), an {AmbiguousTime} exception is raised.
    #
    # The result is of the same type as `time`: {TimeWithOffset} for a `Time`,
    # {DateTimeWithOffset} for a `DateTime` and {TimestampWithOffset} for a
    # {Timestamp}. The UTC offset and associated {TimezoneOffset} of the
    # result reflect the offset observed by this time zone at the resulting
    # instant. Any sub-second component of `time` is preserved.
    #
    # The system (local) time zone is never consulted. The UTC offset of
    # `time` is only used to establish the instant it represents; all other
    # calculations are performed using the offset data of this {Timezone}.
    #
    # @param time [Object] a `Time`, `DateTime` or {Timestamp} to be moved.
    # @param months [Integer] the number of local calendar months to move by
    #   (may be negative).
    # @param month_overflow [Symbol] how to handle a day-of-month that does
    #   not exist in the resulting month: `:clamp`, `:overflow` or `:raise`.
    # @param gap [Symbol] how to resolve a resulting local time that does not
    #   exist: `:raise`, `:forward` or `:backward`.
    # @param ambiguous [Symbol] how to resolve a resulting local time that
    #   occurs twice: `:raise`, `:first`, `:last`, `:dst` or `:standard`.
    # @return [Object] `time` moved by `months` local calendar months, as a
    #   {TimeWithOffset}, {DateTimeWithOffset} or {TimestampWithOffset}
    #   (matching the type of `time`).
    # @raise [ArgumentError] if `time` is `nil`.
    # @raise [ArgumentError] if `months` is not an `Integer`.
    # @raise [ArgumentError] if `month_overflow`, `gap` or `ambiguous` is not
    #   one of the supported values.
    # @raise [ArgumentError] if the day-of-month of the local date of `time`
    #   does not exist in the resulting month and `month_overflow` is
    #   `:raise`.
    # @raise [ArgumentError] if `time` is a {Timestamp} with an unspecified
    #   UTC offset.
    # @raise [PeriodNotFound] if the resulting local time does not exist and
    #   `gap` is `:raise`.
    # @raise [AmbiguousTime] if the resulting local time occurs twice and
    #   `ambiguous` did not resolve the ambiguity.
    def add_local_months(time, months, month_overflow: :clamp, gap: :raise, ambiguous: :raise)
      raise ArgumentError, 'time must be specified' unless time
      raise ArgumentError, 'months must be an Integer' unless months.kind_of?(Integer)
      validate_strategy!(:month_overflow, month_overflow, MONTH_OVERFLOW_STRATEGIES)
      validate_strategy!(:gap, gap, LOCAL_GAP_STRATEGIES)
      validate_strategy!(:ambiguous, ambiguous, LOCAL_AMBIGUOUS_STRATEGIES)

      shift_local_calendar(time, 0, months, month_overflow, gap, ambiguous)
    end

    private

    # Validates a strategy parameter.
    #
    # @param name [Symbol] the name of the parameter.
    # @param value [Symbol] the value of the parameter.
    # @param valid [Array<Symbol>] the valid values.
    # @raise [ArgumentError] if `value` is not included in `valid`.
    def validate_strategy!(name, value, valid)
      unless valid.include?(value)
        raise ArgumentError, "#{name} must be one of #{valid.map(&:inspect).join(', ')}"
      end
    end

    # Returns the number of days in a given month of a given (proleptic
    # Gregorian calendar) year.
    #
    # @param year [Integer] the year.
    # @param month [Integer] the month (1-12).
    # @return [Integer] the number of days in the month.
    def days_in_month(year, month)
      result = DAYS_IN_MONTH_NON_LEAP[month - 1]

      if month == 2 && (year % 4 == 0 && year % 100 != 0 || year % 400 == 0)
        result + 1
      else
        result
      end
    end

    # Moves a time by a number of local calendar days and/or months and
    # resolves the resulting local time to an instant observed by this time
    # zone.
    #
    # @param time [Object] a `Time`, `DateTime` or {Timestamp} to be moved.
    # @param days [Integer] the number of local calendar days to move by.
    # @param months [Integer] the number of local calendar months to move by.
    # @param month_overflow [Symbol] how to handle a day-of-month that does
    #   not exist in the resulting month.
    # @param gap [Symbol] how to resolve a resulting local time that does not
    #   exist.
    # @param ambiguous [Symbol] how to resolve a resulting local time that
    #   occurs twice.
    # @return [Object] the result, of the same type as `time`.
    def shift_local_calendar(time, days, months, month_overflow, gap, ambiguous)
      Timestamp.for(time) do |ts|
        raise ArgumentError, 'time must have a specified utc_offset' unless ts.utc_offset

        local = to_local(ts)
        day_number, time_of_day = (local.value + local.utc_offset).divmod(86400)
        extra_days = 0

        if months != 0
          datetime = local.to_datetime
          year = datetime.year
          month = datetime.month
          day = datetime.day

          total_months = year * 12 + month - 1 + months
          year = total_months.div(12)
          month = total_months % 12 + 1

          days_in_target_month = days_in_month(year, month)

          if day > days_in_target_month
            case month_overflow
            when :clamp
              day = days_in_target_month
            when :overflow
              extra_days = day - days_in_target_month
              day = days_in_target_month
            else # :raise
              raise ArgumentError, "day #{day} does not exist in the resulting month (#{year}-#{'%02d' % month})"
            end
          end

          day_number = Timestamp.create(year, month, day).value / 86400
        end

        resolve_local_candidate((day_number + days + extra_days) * 86400 + time_of_day, local.sub_second, gap, ambiguous)
      end
    end

    # Resolves a local wall clock time (expressed as a number of seconds since
    # 1970-01-01 00:00:00, ignoring UTC offsets) to a {TimestampWithOffset}
    # observed by this time zone, applying the chosen gap and ambiguity
    # strategies.
    #
    # @param local_value [Integer] the local wall clock time as a number of
    #   seconds since 1970-01-01 00:00:00, ignoring UTC offsets.
    # @param sub_second [Numeric] the fractional part of the second.
    # @param gap [Symbol] how to resolve a local time that does not exist.
    # @param ambiguous [Symbol] how to resolve a local time that occurs twice.
    # @return [TimestampWithOffset] the resolved local time.
    # @raise [PeriodNotFound] if the local time does not exist and `gap` is
    #   `:raise`.
    # @raise [AmbiguousTime] if the local time occurs twice and `ambiguous`
    #   did not resolve the ambiguity.
    def resolve_local_candidate(local_value, sub_second, gap, ambiguous)
      # Shifting past a gap will normally land on a valid local time. Loop
      # (with a bound) in case of pathological adjacent transitions.
      5.times do
        candidate = Timestamp.new(local_value, sub_second)
        periods = periods_for_local(candidate)

        if periods.empty?
          if gap == :raise
            raise PeriodNotFound, "#{candidate.strftime('%Y-%m-%d %H:%M:%S')} is an invalid local time."
          end

          local_value = shift_past_gap(local_value, gap)
        else
          period = periods.size == 1 ? periods.first : resolve_ambiguous_period(candidate, periods, ambiguous)
          utc_offset = period.observed_utc_offset
          return TimestampWithOffset.new(local_value - utc_offset, sub_second, utc_offset).set_timezone_offset(period.offset)
        end
      end

      raise PeriodNotFound, 'the resulting local time could not be resolved to a valid local time.'
    end

    # Moves a local wall clock time that falls into a gap (a local time that
    # does not exist because of a forward offset change) past the gap.
    #
    # @param local_value [Integer] the local wall clock time as a number of
    #   seconds since 1970-01-01 00:00:00, ignoring UTC offsets.
    # @param gap [Symbol] either `:forward` to move forward by the length of
    #   the gap or `:backward` to move backward by the length of the gap.
    # @return [Integer] the shifted local wall clock time.
    # @raise [PeriodNotFound] if no transition causing a gap containing
    #   `local_value` could be found.
    def shift_past_gap(local_value, gap)
      # The transition causing the gap occurs within +/- 24 hours of the local
      # wall clock value (UTC offsets are always less than 24 hours).
      from = Timestamp.utc(local_value - 86400)
      to = Timestamp.utc(local_value + 86400)

      transitions_up_to(to, from).each do |transition|
        previous = transition.previous_offset.observed_utc_offset
        current = transition.offset.observed_utc_offset

        # Only forward offset changes create gaps.
        next unless current > previous

        gap_start = transition.timestamp_value + previous
        gap_end = transition.timestamp_value + current

        if local_value >= gap_start && local_value < gap_end
          shift = current - previous
          return gap == :forward ? local_value + shift : local_value - shift
        end
      end

      raise PeriodNotFound, "#{Timestamp.new(local_value, 0).strftime('%Y-%m-%d %H:%M:%S')} is an invalid local time."
    end

    # Selects a single {TimezonePeriod} from multiple candidate periods for an
    # ambiguous local time.
    #
    # @param candidate [Timestamp] the ambiguous local time.
    # @param periods [Array<TimezonePeriod>] the candidate periods.
    # @param ambiguous [Symbol] how to resolve the ambiguity: `:raise`,
    #   `:first`, `:last`, `:dst` or `:standard`.
    # @return [TimezonePeriod] the selected period.
    # @raise [AmbiguousTime] if `ambiguous` is `:raise` or did not resolve the
    #   ambiguity.
    def resolve_ambiguous_period(candidate, periods, ambiguous)
      case ambiguous
      when :first
        # The earliest occurrence in UTC corresponds to the largest observed
        # UTC offset.
        periods.max_by {|period| period.observed_utc_offset }
      when :last
        periods.min_by {|period| period.observed_utc_offset }
      when :dst, :standard
        dst = ambiguous == :dst
        matches = periods.select {|period| period.dst? == dst }

        if matches.size == 1
          matches.first
        else
          raise AmbiguousTime, "#{candidate.strftime('%Y-%m-%d %H:%M:%S')} is an ambiguous local time."
        end
      else # :raise
        raise AmbiguousTime, "#{candidate.strftime('%Y-%m-%d %H:%M:%S')} is an ambiguous local time."
      end
    end
  end
end