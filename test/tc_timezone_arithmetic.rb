# encoding: UTF-8
# frozen_string_literal: true

require_relative 'test_utils'

class TCTimezoneArithmetic < Minitest::Test
  include TZInfo

  # A custom time zone whose clocks go back on 28 February each year (at
  # 02:00 DST local time to 01:00 standard local time, i.e. UTC 01:00). Used
  # to exercise the case where month-end overflow (31 January advanced by one
  # month clamps to 28 February) and a repeated local hour have to be
  # resolved together.
  class FebruaryEndFallBackTimezone < TZInfo::Timezone
    STANDARD = TZInfo::TimezoneOffset.new(0, 0, 'STD')
    DAYLIGHT = TZInfo::TimezoneOffset.new(0, 3600, 'DST')

    def identifier
      'Test/FebruaryEndFallBack'
    end

    def period_for(time)
      utc_value = TZInfo::Timestamp.for(time).value
      year = Time.at(utc_value).utc.year
      transition = transition_for_year(year)
      utc_value < transition.timestamp_value ? period_before(transition) : period_after(transition)
    end

    def periods_for_local(local_time)
      value = local_time.to_i
      time = Time.at(value).utc
      transition = transition_for_year(time.year)

      results = []

      if value >= transition.timestamp_value && value < transition.timestamp_value + 3600
        # The repeated hour (01:00..01:59:59 on 28 February) exists once with
        # the old DST offset ...
        results << period_before(transition)
      elsif value < transition.timestamp_value - 3600
        # DST is in force before the transition's local end.
        results << period_before(transition)
      end

      # ... and once with the new standard offset from the transition's local
      # start onwards.
      results << period_after(transition) if value >= transition.timestamp_value

      results
    end

    def transitions_up_to(to, from = nil)
      raise ArgumentError, 'to must be specified' unless to
      raise ArgumentError, 'to must be greater than from' if from && to.to_i <= from.to_i

      to_value = to.to_i
      from_value = from ? from.to_i : (to_value - 366 * 86400)

      first_year = Time.at(from_value).utc.year - 1
      last_year = Time.at(to_value).utc.year + 1

      (first_year..last_year).map {|year| transition_for_year(year)}.select do |transition|
        transition.timestamp_value >= from_value && transition.timestamp_value < to_value
      end
    end

    private

    def transition_for_year(year)
      TZInfo::TimezoneTransition.new(STANDARD, DAYLIGHT, TZInfo::Timestamp.create(year, 2, 28, 1, 0, 0, 0, :utc).value)
    end

    def period_before(transition)
      TZInfo::TransitionsTimezonePeriod.new(nil, transition)
    end

    def period_after(transition)
      TZInfo::TransitionsTimezonePeriod.new(transition, nil)
    end
  end

  def setup
    @melbourne = Timezone.get('Australia/Melbourne')
    @new_york = Timezone.get('America/New_York')
    @london = Timezone.get('Europe/London')
    @utc = Timezone.get('Etc/UTC')
  end

  # Returns the UTC instant represented by a Time, DateTime, Timestamp or
  # Time-like object as a whole number of seconds since the epoch.
  def to_utc_seconds(value)
    case value
      when Timestamp
        value.value
      when DateTime
        value.new_offset(0).to_time.to_i
      else
        value.to_i
    end
  end

  # Returns the sub-second part of a Time, DateTime, Timestamp or Time-like
  # object as a Rational.
  def sub_second_of(value)
    case value
      when Timestamp
        value.sub_second
      when DateTime
        value.sec_fraction
      else
        value.subsec.to_r
    end
  end

  # Returns the class used for offset-bearing results for a given input type.
  def offset_class(value)
    return DateTimeWithOffset if value.kind_of?(DateTime)
    return TimestampWithOffset if value.kind_of?(Timestamp)
    TimeWithOffset
  end

  # 1895-01-31 12:00 Melbourne: on 1895-02-01 at 00:00 local the zone changed
  # from LMT (+09:39:52) to AEST (+10:00), a non-integer-hour offset change
  # that put a 1208 second gap in the local timeline.
  def test_elapsed_seconds_versus_local_day_non_integer_hour_change
    time_types_test(:offset) do |h|
      start = h.time(1895, 1, 31, 12, 0, 0, Rational(1, 3), 34792)

      elapsed = @melbourne.add_elapsed_seconds(start, 86400)
      local = @melbourne.advance_local(start, 0, 1)

      # Exactly 86,400 seconds later: the wall clock shows the 1208 second
      # offset change on top of 24 hours.
      assert_equal_with_offset(h.output_time(1895, 2, 1, 12, 20, 8, Rational(1, 3), 36000), elapsed)
      assert_kind_of(offset_class(start), elapsed)
      assert_equal('AEST', elapsed.timezone_offset.abbreviation)

      # Same local time on the next local calendar day.
      assert_equal_with_offset(h.output_time(1895, 2, 1, 12, 0, 0, Rational(1, 3), 36000), local)
      assert_kind_of(offset_class(start), local)
      assert_equal('AEST', local.timezone_offset.abbreviation)

      elapsed_utc = to_utc_seconds(elapsed)
      local_utc = to_utc_seconds(local)
      assert_equal(1208, elapsed_utc - local_utc)
      assert_equal(Rational(1, 3), sub_second_of(elapsed))
      assert_equal(Rational(1, 3), sub_second_of(local))
    end
  end

  def test_add_elapsed_seconds_keeps_instant_across_integer_hour_transition
    start = @new_york.local_time(2004, 4, 3, 10, 0, 0)
    result = @new_york.add_elapsed_seconds(start, 24 * 3600)

    assert_equal(@new_york.local_time(2004, 4, 4, 11, 0, 0), result)
    assert_equal('EDT', result.timezone_offset.abbreviation)
    assert_equal(0, result.subsec)
  end

  def test_add_elapsed_seconds_preserves_sub_second
    start = @new_york.local_time(2004, 4, 3, 10, 0, 0, Rational(1, 7))
    result = @new_york.add_elapsed_seconds(start, Rational(2, 7))
    assert_equal(Rational(3, 7), result.subsec)
  end

  def test_add_elapsed_seconds_negative
    start = @london.local_time(2020, 11, 15, 10)
    result = @london.add_elapsed_seconds(start, -86400)
    assert_equal(@london.local_time(2020, 11, 14, 10), result)
  end

  def test_add_elapsed_seconds_float
    start = @utc.local_time(2020, 1, 1, 0, 0, 0)
    result = @utc.add_elapsed_seconds(start, 1.5)
    assert_equal(1, result.to_i - start.to_i)
    assert_in_delta(0.5, result.subsec.to_f, 1e-9)
  end

  def test_add_elapsed_seconds_zero_does_not_use_system_timezone
    # The suite runs with TZ=America/Los_Angeles (see ts_all.rb). The result
    # must be local to self, carrying self's offset rather than the system
    # zone offset.
    start = @london.local_time(2020, 1, 1, 12)
    result = @london.add_elapsed_seconds(start, 0)
    assert_equal(0, result.utc_offset)
    assert_kind_of(TimeWithOffset, result)
  end

  def test_add_elapsed_seconds_validates_arguments
    assert_raises(ArgumentError) { @utc.add_elapsed_seconds(nil, 1) }
    assert_raises(ArgumentError) { @utc.add_elapsed_seconds(@utc.now, '1') }
    assert_raises(ArgumentError) { @utc.add_elapsed_seconds(@utc.now, nil) }
  end

  def test_add_elapsed_seconds_preserves_type
    time = Time.utc(2004, 4, 3, 15, 0, 0)
    assert_kind_of(TimeWithOffset, @new_york.add_elapsed_seconds(time, 1))
    assert_kind_of(DateTimeWithOffset, @new_york.add_elapsed_seconds(time.to_datetime, 1))
    assert_kind_of(TimestampWithOffset, @new_york.add_elapsed_seconds(Timestamp.utc(time.to_i), 1))
  end

  def test_advance_local_day_keeps_wall_time_across_transition
    start = @new_york.local_time(2004, 4, 3, 10, 30, 15, Rational(1, 7))
    result = @new_york.advance_local(start, 0, 1)

    assert_equal(@new_york.local_time(2004, 4, 4, 10, 30, 15, Rational(1, 7)), result)
    assert_equal(Rational(1, 7), result.subsec)
    assert_equal('EDT', result.timezone_offset.abbreviation)
  end

  def test_advance_local_month
    start = @new_york.local_time(2004, 1, 15, 9)
    result = @new_york.advance_local(start, 1)
    assert_equal(@new_york.local_time(2004, 2, 15, 9), result)
  end

  def test_advance_local_negative_months_and_days
    start = @london.local_time(2020, 11, 15, 10)
    assert_equal(@london.local_time(2019, 9, 15, 10), @london.advance_local(start, -14))
    assert_equal(@london.local_time(2020, 10, 15, 10), @london.advance_local(start, 0, -31))
  end

  def test_advance_local_days_overflow_month_boundary
    start = @london.local_time(2020, 1, 31, 10)
    assert_equal(@london.local_time(2020, 2, 1, 10), @london.advance_local(start, 0, 1))

    start = @london.local_time(2020, 3, 1, 10)
    assert_equal(@london.local_time(2020, 2, 29, 10), @london.advance_local(start, 0, -1))
  end

  def test_advance_local_combined_months_and_days
    start = @london.local_time(2020, 1, 31, 10)
    # Month move clamps to 29 February, then a day is added.
    assert_equal(@london.local_time(2020, 3, 1, 10), @london.advance_local(start, 1, 1))
  end

  def test_advance_local_zero_is_identity_offset_preserved
    start = @new_york.local_time(2004, 7, 15, 9, 30, 45, Rational(2, 3))
    result = @new_york.advance_local(start)
    assert_equal(start, result)
    assert_equal(-14400, result.utc_offset)
    assert_equal(Rational(2, 3), result.subsec)
  end

  def test_methods_work_through_proxy
    proxy = Timezone.get_proxy('America/New_York')
    start = proxy.local_time(2004, 1, 15, 10)

    assert_equal(Timezone.get('America/New_York').local_time(2004, 1, 16, 10), proxy.advance_local(start, 0, 1))
    assert_kind_of(TimeWithOffset, proxy.add_elapsed_seconds(start, 86400))
  end

  def test_advance_local_month_end_clamps_by_default
    start = @new_york.local_time(2004, 1, 31, 12)
    assert_equal(@new_york.local_time(2004, 2, 29, 12), @new_york.advance_local(start, 1))
    assert_equal(@new_york.local_time(2004, 2, 29, 12), @new_york.advance_local(start, 1, 0, month_overflow: :clamp))

    start = @new_york.local_time(2003, 1, 31, 12)
    assert_equal(@new_york.local_time(2003, 2, 28, 12), @new_york.advance_local(start, 1))
  end

  def test_advance_local_month_end_raise
    start = @new_york.local_time(2003, 1, 31, 12)
    error = assert_raises(InvalidLocalDate) { @new_york.advance_local(start, 1, 0, month_overflow: :raise) }
    assert_equal('31 is not a valid day of 2003-2', error.message)
  end

  def test_advance_local_gap_raises_by_default
    # 2004-04-04 02:30 does not exist in New York.
    start = @new_york.local_time(2004, 4, 3, 2, 30)
    assert_raises(PeriodNotFound) { @new_york.advance_local(start, 0, 1) }
    assert_raises(PeriodNotFound) { @new_york.advance_local(start, 0, 1, gap: :raise) }
  end

  def test_advance_local_gap_before_and_after
    start = @new_york.local_time(2004, 4, 3, 2, 30)

    before = @new_york.advance_local(start, 0, 1, gap: :before)
    after = @new_york.advance_local(start, 0, 1, gap: :after)

    before_zone = before.timezone_offset.abbreviation
    after_zone = after.timezone_offset.abbreviation
    assert_equal(Time.utc(2004, 4, 4, 7, 30), before.utc)
    assert_equal(Time.utc(2004, 4, 4, 6, 30), after.utc)
    assert_equal('EDT', before_zone)
    assert_equal('EST', after_zone)
    assert_equal(3600, before.to_i - after.to_i)
  end

  def test_advance_local_ambiguous_raises_by_default
    # 2004-10-31 01:30 occurs twice in New York.
    start = @new_york.local_time(2004, 9, 30, 1, 30, 0, 0, false)
    assert_raises(AmbiguousTime) { @new_york.advance_local(start, 1, 1) }
    assert_raises(AmbiguousTime) { @new_york.advance_local(start, 1, 1, ambiguous: :raise) }
  end

  def test_advance_local_ambiguous_before_and_after
    start = @new_york.local_time(2004, 9, 30, 1, 30, 0, 0, false)

    before = @new_york.advance_local(start, 1, 1, ambiguous: :before)
    after = @new_york.advance_local(start, 1, 1, ambiguous: :after)

    before_zone = before.timezone_offset.abbreviation
    after_zone = after.timezone_offset.abbreviation
    assert_equal(Time.utc(2004, 10, 31, 5, 30), before.utc)
    assert_equal(Time.utc(2004, 10, 31, 6, 30), after.utc)
    assert_equal('EDT', before_zone)
    assert_equal('EST', after_zone)
    assert_equal(-3600, before.to_i - after.to_i)
  end

  def test_advance_local_non_integer_hour_gap
    # 1895-02-01 00:00:00..00:20:07 did not exist in Melbourne (a 1208 second
    # gap; the offset changed by a non-integer number of hours). Advancing
    # 1895-01-30 00:10:04 by two days targets 1895-02-01 00:10:04.
    start = @melbourne.local_time(1895, 1, 30, 0, 10, 4, 0, false)

    assert_raises(PeriodNotFound) { @melbourne.advance_local(start, 0, 2) }

    before = @melbourne.advance_local(start, 0, 2, gap: :before)
    after = @melbourne.advance_local(start, 0, 2, gap: :after)

    # This historical transition decreased the UTC offset (clocks were
    # relabeled forward on the wall). The :before interpretation (keep the old
    # offset) lands on the post-transition wall time; the :after interpretation
    # (use the new offset) lands on the pre-transition wall time.
    before_zone = before.timezone_offset.abbreviation
    after_zone = after.timezone_offset.abbreviation
    assert_equal(Time.utc(1895, 1, 31, 14, 30, 12), before.utc)
    assert_equal(Time.utc(1895, 1, 31, 14, 10, 4), after.utc)
    assert_equal('AEST', before_zone)
    assert_equal('LMT', after_zone)
  end

  def test_advance_local_preserves_type
    time = Time.utc(2004, 1, 15, 14, 0, 0)
    assert_kind_of(TimeWithOffset, @new_york.advance_local(time, 1))
    assert_kind_of(DateTimeWithOffset, @new_york.advance_local(time.to_datetime, 1))
    assert_kind_of(TimestampWithOffset, @new_york.advance_local(Timestamp.utc(time.to_i), 1))
  end

  def test_advance_local_does_not_use_system_timezone
    start = @london.local_time(2020, 1, 1, 12)
    result = @london.advance_local(start, 0, 1)
    assert_equal(0, result.utc_offset)
    assert_kind_of(TimeWithOffset, result)
  end

  def test_advance_local_validates_arguments
    time = @utc.now
    assert_raises(ArgumentError) { @utc.advance_local(nil) }
    assert_raises(ArgumentError) { @utc.advance_local(time, 1.5, 0) }
    assert_raises(ArgumentError) { @utc.advance_local(time, 0, 1.5) }
    assert_raises(ArgumentError) { @utc.advance_local(time, 0, 0, :clamp) }
    assert_raises(ArgumentError) { @utc.advance_local(time, 0, 0, month_overflow: :nope) }
    assert_raises(ArgumentError) { @utc.advance_local(time, 0, 0, gap: :nope) }
    assert_raises(ArgumentError) { @utc.advance_local(time, 0, 0, ambiguous: :nope) }
    assert_raises(ArgumentError) { @utc.advance_local(time, 0, 0, unknown: 1) }
  end

  def test_unspecified_offset_timestamp_rejected
    timestamp = Timestamp.create(2020, 1, 1)
    assert_raises(ArgumentError) { @utc.advance_local(timestamp, 1) }
    assert_raises(ArgumentError) { @utc.add_elapsed_seconds(timestamp, 1) }
  end

  # Moving from 31 January 01:30 DST in the custom zone by one month clamps
  # to 28 February 01:30, which is exactly the repeated hour caused by the
  # clocks going back. Month-end handling and ambiguity handling must be
  # applied in sequence, both according to the chosen options.
  def test_month_end_overflow_then_ambiguity_resolved_together
    tz = FebruaryEndFallBackTimezone.new

    start_2003 = tz.local_time(2003, 1, 31, 1, 30, 0, 0, true)
    target_2003 = Time.utc(2003, 2, 28, 1, 30)

    assert_raises(AmbiguousTime) { tz.advance_local(start_2003, 1) }

    before = tz.advance_local(start_2003, 1, 0, ambiguous: :before)
    after = tz.advance_local(start_2003, 1, 0, ambiguous: :after)

    before_zone = before.timezone_offset.abbreviation
    after_zone = after.timezone_offset.abbreviation
    assert_equal(target_2003 - 3600, before.utc)
    assert_equal(target_2003, after.utc)
    assert_equal('DST', before_zone)
    assert_equal('STD', after_zone)

    # In a leap year the same move clamps to 29 February, which is
    # unambiguous (the fall-back happened on the 28th).
    start_2004 = tz.local_time(2004, 1, 31, 1, 30, 0, 0, true)
    result = tz.advance_local(start_2004, 1)
    result_zone = result.timezone_offset.abbreviation
    assert_equal(Time.utc(2004, 2, 29, 1, 30), result.utc)
    assert_equal('STD', result_zone)

    # Choosing :raise for the month overflow short-circuits before the
    # ambiguity is considered.
    assert_raises(InvalidLocalDate) { tz.advance_local(start_2003, 1, 0, month_overflow: :raise) }
  end

  # Month-end clamping that lands well after a spring-forward gap just
  # resolves normally (31 March + 1 month in a 30-day April).
  def test_month_end_clamp_unambiguous_target
    start = @new_york.local_time(2004, 3, 31, 2, 30)
    result = @new_york.advance_local(start, 1)
    assert_equal(Time.utc(2004, 4, 30, 6, 30), result.utc)
  end
end
