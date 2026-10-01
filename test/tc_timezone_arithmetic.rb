# encoding: UTF-8
# frozen_string_literal: true

require_relative 'test_utils'

class TCTimezoneArithmetic < Minitest::Test
  include TZInfo

  # A synthetic time zone observing a base offset of UTC+10:30 and a daylight
  # savings offset of UTC+11:00, modelled on Australia/Lord_Howe. The observed
  # offset changes by 30 minutes (a non-whole-hour change) at each transition:
  #
  # - 2016-04-30 02:00 +11:00 -> 2016-04-30 01:30 +10:30 (01:30-02:00 repeated)
  # - 2016-10-02 02:00 +10:30 -> 2016-10-02 02:30 +11:00 (02:00-02:30 skipped)
  # - 2017-04-30 02:00 +11:00 -> 2017-04-30 01:30 +10:30 (01:30-02:00 repeated)
  # - 2017-10-01 02:00 +10:30 -> 2017-10-01 02:30 +11:00 (02:00-02:30 skipped)
  #
  # The daylight savings transitions are placed on the last day of April so
  # that month-end clamping can land directly on an ambiguous local time.
  def half_hour_zone
    @half_hour_zone ||= begin
      std = TimezoneOffset.new(37800, 0, 'LHST')
      dst = TimezoneOffset.new(37800, 1800, 'LHDT')

      transitions = [
        [2016,  4, 29, 15,  0, dst, std],
        [2016, 10,  1, 15, 30, std, dst],
        [2017,  4, 29, 15,  0, dst, std],
        [2017,  9, 30, 15, 30, std, dst]
      ].map do |year, month, day, hour, minute, from, to|
        timestamp_value = Timestamp.create(year, month, day, hour, minute, 0, 0, :utc).value
        TimezoneTransition.new(to, from, timestamp_value)
      end

      DataSources::TransitionsDataTimezoneInfo.new('Test/Half_Hour_Zone', transitions).create_timezone
    end
  end

  # "24 hours later" (elapsed seconds) and "the same local time tomorrow"
  # (calendar days) differ across the 30 minute forward transition.
  def test_add_seconds_and_add_local_days_differ_across_non_whole_hour_transition
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 10, 1, 12, 0, 0, 0)

    elapsed = zone.add_seconds(start, 86400)
    calendar = zone.add_local_days(start, 1)

    # 24 elapsed hours crosses the 30 minute forward shift, landing 30 minutes
    # later on the wall clock.
    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 10, 2, 12, 30, 0, 0), elapsed)
    assert_equal(39600, elapsed.utc_offset)

    # 1 local calendar day keeps the 12:00 wall clock reading.
    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 10, 2, 12, 0, 0, 0), calendar)
    assert_equal(39600, calendar.utc_offset)

    # The two results are 30 minutes (the size of the non-whole-hour offset
    # change) apart.
    assert_equal(1800, elapsed.value - calendar.value)
  end

  def test_add_seconds_negative_across_transition
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 10, 2, 3, 0, 0, 0)

    # 1 elapsed hour earlier crosses the 30 minute forward shift.
    result = zone.add_seconds(start, -3600)

    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 10, 2, 1, 30, 0, 0), result)
    assert_equal(37800, result.utc_offset)
  end

  def test_add_seconds_zero
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 10, 1, 12, 0, 0, Rational(1, 2))

    result = zone.add_seconds(start, 0)

    assert_equal_with_offset_and_timezone_offset(start, result)
  end

  def test_add_local_days_negative
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 10, 3, 12, 0, 0, 0)

    result = zone.add_local_days(start, -1)

    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 10, 2, 12, 0, 0, 0), result)
  end

  def test_add_local_days_gap_raise
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 10, 1, 2, 15, 0, 0)

    # 2016-10-02 02:15 does not exist (02:00-02:30 is skipped).
    error = assert_raises(PeriodNotFound) { zone.add_local_days(start, 1) }
    assert_equal('2016-10-02 02:15:00 is an invalid local time.', error.message)

    error = assert_raises(PeriodNotFound) { zone.add_local_days(start, 1, gap: :raise) }
    assert_equal('2016-10-02 02:15:00 is an invalid local time.', error.message)
  end

  def test_add_local_days_gap_forward
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 10, 1, 2, 15, 0, 0)

    # Moved forward by the 30 minute length of the gap.
    result = zone.add_local_days(start, 1, gap: :forward)

    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 10, 2, 2, 45, 0, 0), result)
    assert_equal(39600, result.utc_offset)
  end

  def test_add_local_days_gap_backward
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 10, 1, 2, 15, 0, 0)

    # Moved backward by the 30 minute length of the gap.
    result = zone.add_local_days(start, 1, gap: :backward)

    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 10, 2, 1, 45, 0, 0), result)
    assert_equal(37800, result.utc_offset)
  end

  def test_add_local_days_ambiguous_raise
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 4, 29, 1, 45, 0, 0)

    # 2016-04-30 01:45 occurs twice (01:30-02:00 is repeated).
    error = assert_raises(AmbiguousTime) { zone.add_local_days(start, 1) }
    assert_equal('2016-04-30 01:45:00 is an ambiguous local time.', error.message)

    error = assert_raises(AmbiguousTime) { zone.add_local_days(start, 1, ambiguous: :raise) }
    assert_equal('2016-04-30 01:45:00 is an ambiguous local time.', error.message)
  end

  def test_add_local_days_ambiguous_first
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 4, 29, 1, 45, 0, 0)

    # The earlier (in UTC) occurrence observes +11:00 (daylight savings).
    result = zone.add_local_days(start, 1, ambiguous: :first)

    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 4, 30, 1, 45, 0, 0, true), result)
    assert_equal(39600, result.utc_offset)
  end

  def test_add_local_days_ambiguous_last
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 4, 29, 1, 45, 0, 0)

    # The later (in UTC) occurrence observes +10:30 (standard time).
    result = zone.add_local_days(start, 1, ambiguous: :last)

    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 4, 30, 1, 45, 0, 0, false), result)
    assert_equal(37800, result.utc_offset)
  end

  def test_add_local_days_ambiguous_dst_and_standard
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 4, 29, 1, 45, 0, 0)

    dst = zone.add_local_days(start, 1, ambiguous: :dst)
    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 4, 30, 1, 45, 0, 0, true), dst)

    standard = zone.add_local_days(start, 1, ambiguous: :standard)
    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 4, 30, 1, 45, 0, 0, false), standard)
  end

  def test_add_local_months_month_end_clamp
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 1, 31, 10, 0, 0, 0)

    # 2016 is a leap year; the default is to clamp to the last day of the
    # resulting month.
    result = zone.add_local_months(start, 1)
    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 2, 29, 10, 0, 0, 0), result)

    result = zone.add_local_months(start, 1, month_overflow: :clamp)
    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 2, 29, 10, 0, 0, 0), result)
  end

  def test_add_local_months_month_end_clamp_non_leap_year
    zone = half_hour_zone
    start = zone.local_timestamp(2017, 1, 31, 10, 0, 0, 0)

    result = zone.add_local_months(start, 1)

    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2017, 2, 28, 10, 0, 0, 0), result)
  end

  def test_add_local_months_month_end_overflow
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 1, 31, 10, 0, 0, 0)

    # 31 - 29 = 2 excess days are carried into March.
    result = zone.add_local_months(start, 1, month_overflow: :overflow)

    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 3, 2, 10, 0, 0, 0), result)
  end

  def test_add_local_months_month_end_raise
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 1, 31, 10, 0, 0, 0)

    error = assert_raises(ArgumentError) { zone.add_local_months(start, 1, month_overflow: :raise) }
    assert_equal('day 31 does not exist in the resulting month (2016-02)', error.message)
  end

  def test_add_local_months_no_overflow
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 12, 15, 8, 30, 0, 0)

    result = zone.add_local_months(start, 2)

    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2017, 2, 15, 8, 30, 0, 0), result)
  end

  def test_add_local_months_negative_crosses_year_boundary
    zone = half_hour_zone
    start = zone.local_timestamp(2017, 3, 15, 8, 0, 0, 0)

    result = zone.add_local_months(start, -5)

    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 10, 15, 8, 0, 0, 0), result)
  end

  # Month-end clamping lands on 2016-04-30, where 01:45 is ambiguous because
  # of the 30 minute backward transition. The month_overflow and ambiguous
  # strategies must both be honoured.
  def test_add_local_months_month_end_clamp_combined_with_ambiguity
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 3, 31, 1, 45, 0, Rational(1, 2))

    first = zone.add_local_months(start, 1, month_overflow: :clamp, ambiguous: :first)
    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 4, 30, 1, 45, 0, Rational(1, 2), true), first)
    assert_equal(39600, first.utc_offset)

    last = zone.add_local_months(start, 1, month_overflow: :clamp, ambiguous: :last)
    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 4, 30, 1, 45, 0, Rational(1, 2), false), last)
    assert_equal(37800, last.utc_offset)

    error = assert_raises(AmbiguousTime) { zone.add_local_months(start, 1, month_overflow: :clamp, ambiguous: :raise) }
    assert_equal('2016-04-30 01:45:00 is an ambiguous local time.', error.message)

    # Overflowing into May avoids the ambiguous local time entirely.
    overflow = zone.add_local_months(start, 1, month_overflow: :overflow, ambiguous: :raise)
    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 5, 1, 1, 45, 0, Rational(1, 2)), overflow)
    assert_equal(37800, overflow.utc_offset)
  end

  def test_results_preserve_sub_second_and_timezone_offset
    zone = half_hour_zone
    sub_second = Rational(123456789, 1000000000)
    start = zone.local_timestamp(2016, 10, 1, 12, 0, 0, sub_second)

    result = zone.add_local_days(start, 1)
    assert_kind_of(TimestampWithOffset, result)
    assert_equal(sub_second, result.sub_second)
    assert_equal(zone.local_timestamp(2016, 10, 2, 12, 0, 0).timezone_offset, result.timezone_offset)

    result = zone.add_seconds(start, 3600)
    assert_kind_of(TimestampWithOffset, result)
    assert_equal(sub_second, result.sub_second)
    assert_equal(zone.local_timestamp(2016, 10, 1, 13, 0, 0).timezone_offset, result.timezone_offset)

    result = zone.add_local_months(start, 1)
    assert_kind_of(TimestampWithOffset, result)
    assert_equal(sub_second, result.sub_second)
    assert_equal(zone.local_timestamp(2016, 11, 1, 12, 0, 0).timezone_offset, result.timezone_offset)
  end

  def test_result_type_matches_input_type
    zone = half_hour_zone

    time_types_test(:offset) do |h|
      time = h.time(2016, 10, 1, 12, 0, 0, 0, 37800)

      [zone.add_seconds(time, 3600), zone.add_local_days(time, 1), zone.add_local_months(time, 1)].each do |result|
        if h.type == :datetime
          assert_kind_of(DateTimeWithOffset, result)
        elsif h.type == :timestamp
          assert_kind_of(TimestampWithOffset, result)
        else
          assert_kind_of(TimeWithOffset, result)
        end
      end
    end
  end

  def test_results_do_not_depend_on_system_timezone
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 10, 1, 2, 15, 0, 0)
    old_tz = ENV['TZ']

    begin
      results = ['UTC', 'America/New_York', 'Australia/Lord_Howe'].map do |tz|
        ENV['TZ'] = tz
        zone.add_local_days(start, 1, gap: :forward)
      end
    ensure
      ENV['TZ'] = old_tz
    end

    assert(results.all? {|r| r == results.first })
    assert(results.all? {|r| r.utc_offset == 39600 })
    assert(results.all? {|r| r.timezone_offset == results.first.timezone_offset })
  end

  def test_add_seconds_with_real_zone
    zone = Timezone.get('Europe/London')
    start = zone.local_timestamp(2016, 3, 27, 0, 30, 0, 0)

    # The clocks go forward 1 hour at 01:00 GMT on 2016-03-27, so 2 elapsed
    # hours from 00:30 GMT lands at 03:30 BST.
    result = zone.add_seconds(start, 7200)

    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 3, 27, 3, 30, 0, 0), result)
  end

  def test_add_local_days_with_real_zone
    zone = Timezone.get('Europe/London')
    start = zone.local_timestamp(2016, 3, 26, 12, 0, 0, 0)

    result = zone.add_local_days(start, 1)

    assert_equal_with_offset_and_timezone_offset(zone.local_timestamp(2016, 3, 27, 12, 0, 0, 0), result)
  end

  def test_nil_time
    zone = half_hour_zone

    error = assert_raises(ArgumentError) { zone.add_seconds(nil, 1) }
    assert_equal('time must be specified', error.message)

    error = assert_raises(ArgumentError) { zone.add_local_days(nil, 1) }
    assert_equal('time must be specified', error.message)

    error = assert_raises(ArgumentError) { zone.add_local_months(nil, 1) }
    assert_equal('time must be specified', error.message)
  end

  def test_non_integer_amounts
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 10, 1, 12, 0, 0, 0)

    error = assert_raises(ArgumentError) { zone.add_seconds(start, 1.5) }
    assert_equal('seconds must be an Integer', error.message)

    error = assert_raises(ArgumentError) { zone.add_local_days(start, 1.5) }
    assert_equal('days must be an Integer', error.message)

    error = assert_raises(ArgumentError) { zone.add_local_months(start, 1.5) }
    assert_equal('months must be an Integer', error.message)
  end

  def test_invalid_strategies
    zone = half_hour_zone
    start = zone.local_timestamp(2016, 10, 1, 12, 0, 0, 0)

    error = assert_raises(ArgumentError) { zone.add_local_days(start, 1, gap: :invalid) }
    assert_equal('gap must be one of :raise, :forward, :backward', error.message)

    error = assert_raises(ArgumentError) { zone.add_local_days(start, 1, ambiguous: :invalid) }
    assert_equal('ambiguous must be one of :raise, :first, :last, :dst, :standard', error.message)

    error = assert_raises(ArgumentError) { zone.add_local_months(start, 1, month_overflow: :invalid) }
    assert_equal('month_overflow must be one of :clamp, :overflow, :raise', error.message)

    error = assert_raises(ArgumentError) { zone.add_local_months(start, 1, gap: :invalid) }
    assert_equal('gap must be one of :raise, :forward, :backward', error.message)

    error = assert_raises(ArgumentError) { zone.add_local_months(start, 1, ambiguous: :invalid) }
    assert_equal('ambiguous must be one of :raise, :first, :last, :dst, :standard', error.message)
  end

  def test_time_with_unspecified_offset
    zone = half_hour_zone
    timestamp = Timestamp.create(2016, 10, 1, 12, 0, 0)

    error = assert_raises(ArgumentError) { zone.add_seconds(timestamp, 1) }
    assert_equal('time must have a specified utc_offset', error.message)

    error = assert_raises(ArgumentError) { zone.add_local_days(timestamp, 1) }
    assert_equal('time must have a specified utc_offset', error.message)

    error = assert_raises(ArgumentError) { zone.add_local_months(timestamp, 1) }
    assert_equal('time must have a specified utc_offset', error.message)
  end
end