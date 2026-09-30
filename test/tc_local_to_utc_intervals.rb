# encoding: UTF-8
# frozen_string_literal: true

require_relative 'test_utils'

class TCLocalToUtcIntervals < Minitest::Test
  include TZInfo

  def local_timestamp(value)
    Timestamp.new(value.floor, value - value.floor)
  end

  def utc_timestamp(value)
    Timestamp.utc(value.floor, value - value.floor)
  end

  def utc_range(start_value, end_value)
    Range.new(utc_timestamp(start_value), utc_timestamp(end_value), true)
  end

  def assert_intervals_equal(expected, actual)
    assert_equal(expected.length, actual.length)

    expected.zip(actual) do |expected_interval, actual_interval|
      assert_equal(true, actual_interval.exclude_end?)
      assert_equal(expected_interval.begin, actual_interval.begin)
      assert_equal(expected_interval.end, actual_interval.end)
    end
  end

  def spring_fall_info
    standard_offset = TimezoneOffset.new(0, 0, 'TESTS')
    daylight_offset = TimezoneOffset.new(0, 3600, 'TESTD')

    spring = TimezoneTransition.new(daylight_offset, standard_offset, 10000)
    fall = TimezoneTransition.new(standard_offset, daylight_offset, 15600)

    DataSources::TransitionsDataTimezoneInfo.new('Test/Zone', [spring, fall])
  end

  def non_integer_info
    first_offset = TimezoneOffset.new(19800, 0, 'TEST1')
    second_offset = TimezoneOffset.new(20700, 0, 'TEST2')
    third_offset = TimezoneOffset.new(20100, 0, 'TEST3')

    forward = TimezoneTransition.new(second_offset, first_offset, 20000)
    backward = TimezoneTransition.new(third_offset, second_offset, 22000)

    DataSources::TransitionsDataTimezoneInfo.new('Test/NonInteger', [forward, backward])
  end

  def triple_overlap_info
    initial_offset = TimezoneOffset.new(0, 0, 'TEST0')
    first_offset = TimezoneOffset.new(-1000, 0, 'TEST1')
    second_offset = TimezoneOffset.new(-3000, 0, 'TEST2')
    third_offset = TimezoneOffset.new(-5000, 0, 'TEST3')

    first = TimezoneTransition.new(first_offset, initial_offset, 30000)
    second = TimezoneTransition.new(second_offset, first_offset, 32000)
    third = TimezoneTransition.new(third_offset, second_offset, 34000)

    DataSources::TransitionsDataTimezoneInfo.new('Test/Triple', [first, second, third])
  end

  def expected_intervals(local_start_value, local_end_value, periods)
    intervals = periods.collect do |offset, period_start_value, period_end_value|
      start_value = period_start_value ? [local_start_value, period_start_value].max : local_start_value
      end_value = period_end_value ? [local_end_value, period_end_value].min : local_end_value

      next if start_value >= end_value

      utc_range(start_value - offset, end_value - offset)
    end.compact

    intervals.sort_by!(&:begin)

    intervals.each_with_object([]) do |interval, result|
      if result.empty? || interval.begin > result.last.end
        result << interval
      else
        previous = result.last
        maximum_end = [previous.end, interval.end].max
        result[-1] = utc_range(previous.begin.value + previous.begin.sub_second, maximum_end.value + maximum_end.sub_second)
      end
    end
  end

  def test_spring_and_fall_boundaries_exhaustive
    info = spring_fall_info
    periods = [
      [0, nil, 10000],
      [3600, 13600, 19200],
      [0, 15600, nil]
    ]

    boundaries = [
      Rational(26998, 3),
      9000,
      Rational(29999, 3),
      10000,
      Rational(30001, 3),
      Rational(40799, 3),
      13600,
      Rational(40801, 3),
      Rational(46799, 3),
      15600,
      Rational(46801, 3),
      Rational(57599, 3),
      19200,
      Rational(57601, 3),
      20000,
      Rational(60001, 3)
    ]

    boundaries.each do |local_start_value|
      boundaries.each do |local_end_value|
        next unless local_end_value > local_start_value

        expected = expected_intervals(local_start_value, local_end_value, periods)
        actual = info.local_to_utc_intervals(local_timestamp(local_start_value), local_timestamp(local_end_value))
        assert_intervals_equal(expected, actual)
      end
    end
  end

  def test_empty_interval
    info = spring_fall_info
    timestamp = local_timestamp(10000)

    assert_equal([], info.local_to_utc_intervals(timestamp, timestamp))
  end

  def test_interval_entirely_within_forward_gap
    info = spring_fall_info

    assert_equal([], info.local_to_utc_intervals(local_timestamp(10000), local_timestamp(13600)))
  end

  def test_interval_crossing_forward_gap_is_merged
    info = spring_fall_info

    assert_intervals_equal(
      [utc_range(Rational(27001, 3), Rational(31202, 3))],
      info.local_to_utc_intervals(local_timestamp(Rational(27001, 3)), local_timestamp(Rational(42002, 3))))
  end

  def test_interval_within_backward_overlap
    info = spring_fall_info

    assert_intervals_equal(
      [utc_range(12400, 13400), utc_range(16000, 17000)],
      info.local_to_utc_intervals(local_timestamp(16000), local_timestamp(17000)))
  end

  def test_interval_across_backward_overlap
    info = spring_fall_info

    assert_intervals_equal(
      [utc_range(11400, 13400), utc_range(15600, 17000)],
      info.local_to_utc_intervals(local_timestamp(15000), local_timestamp(17000)))
  end

  def test_non_integer_forward_gap
    info = non_integer_info

    assert_equal(
      [],
      info.local_to_utc_intervals(local_timestamp(39800), local_timestamp(40700)))
  end

  def test_non_integer_forward_gap_boundaries_with_sub_seconds
    info = non_integer_info

    assert_intervals_equal(
      [utc_range(Rational(57601, 3), Rational(60902, 3))],
      info.local_to_utc_intervals(local_timestamp(Rational(117001, 3)), local_timestamp(Rational(123002, 3))))
  end

  def test_non_integer_backward_overlap
    info = non_integer_info

    assert_intervals_equal(
      [utc_range(Rational(43601, 2), Rational(43801, 2)), utc_range(Rational(44801, 2), Rational(45001, 2))],
      info.local_to_utc_intervals(local_timestamp(Rational(85001, 2)), local_timestamp(Rational(85201, 2))))
  end

  def test_three_periods_can_match_one_local_time
    info = triple_overlap_info

    assert_equal(3, info.periods_for_local(local_timestamp(30000)).length)
  end

  def test_triple_overlap_boundaries_exhaustive
    info = triple_overlap_info
    periods = [
      [0, nil, 30000],
      [-1000, 29000, 31000],
      [-3000, 29000, 31000],
      [-5000, 29000, 31000],
      [-5000, 29000, nil]
    ]

    boundaries = [
      Rational(86998, 3),
      29000,
      Rational(87001, 3),
      Rational(89999, 3),
      30000,
      Rational(90001, 3),
      Rational(92999, 3),
      31000,
      Rational(93001, 3),
      Rational(95999, 3),
      32000,
      Rational(96001, 3)
    ]

    boundaries.each do |local_start_value|
      boundaries.each do |local_end_value|
        next unless local_end_value > local_start_value

        expected = expected_intervals(local_start_value, local_end_value, periods)
        actual = info.local_to_utc_intervals(local_timestamp(local_start_value), local_timestamp(local_end_value))
        assert_intervals_equal(expected, actual)
      end
    end
  end

  def test_triple_overlap_returns_three_intervals
    info = triple_overlap_info

    assert_intervals_equal(
      [utc_range(31000, 31100), utc_range(33000, 33100), utc_range(35000, 35100)],
      info.local_to_utc_intervals(local_timestamp(30000), local_timestamp(30100)))
  end

  def test_constant_offset
    offset = TimezoneOffset.new(19800, 0, 'TEST')
    info = DataSources::ConstantOffsetDataTimezoneInfo.new('Test/Constant', offset)

    assert_intervals_equal(
      [utc_range(Rational(1, 3), Rational(902, 3))],
      info.local_to_utc_intervals(local_timestamp(Rational(59401, 3)), local_timestamp(Rational(60302, 3))))

    assert_intervals_equal(
      [utc_range(Rational(2, 3), Rational(5, 3))],
      info.local_to_utc_intervals(local_timestamp(Rational(59402, 3)), local_timestamp(Rational(59405, 3))))
  end

  def test_constant_zero_offset_returns_single_interval
    timezone = Timezone.get('UTC')

    result = timezone.local_to_utc_intervals(
      Time.utc(2000, 1, 1, 0, 0, 1),
      Time.utc(2000, 1, 1, 0, 0, 2))

    assert_equal([Time.utc(2000, 1, 1, 0, 0, 1)...Time.utc(2000, 1, 1, 0, 0, 2)], result)
  end

  def test_linked_timezone_and_proxy
    linked_timezone = Timezone.get('America/New_York')
    proxy = Timezone.get_proxy('America/New_York')

    start_time = Time.utc(2004, 10, 31, 1, 30, 0)
    end_time = Time.utc(2004, 10, 31, 1, 30, 1)

    assert_equal(
      linked_timezone.local_to_utc_intervals(start_time, end_time),
      proxy.local_to_utc_intervals(start_time, end_time))
  end

  def test_data_timezone_returns_timestamp_intervals
    info = spring_fall_info
    timezone = DataTimezone.new(info)

    result = timezone.local_to_utc_intervals(
      Timestamp.new(16000),
      Timestamp.new(17000))

    assert_intervals_equal(
      [utc_range(12400, 13400), utc_range(16000, 17000)],
      result)
    assert(result.all? {|interval| interval.begin.kind_of?(Timestamp) && interval.end.kind_of?(Timestamp)})
  end

  def test_data_timezone_preserves_time_type
    info = spring_fall_info
    timezone = DataTimezone.new(info)

    result = timezone.local_to_utc_intervals(
      Time.utc(1970, 1, 1, 4, 26, 40),
      Time.utc(1970, 1, 1, 4, 43, 20))

    assert_equal([
      Time.utc(1970, 1, 1, 3, 26, 40)...Time.utc(1970, 1, 1, 3, 43, 20),
      Time.utc(1970, 1, 1, 4, 26, 40)...Time.utc(1970, 1, 1, 4, 43, 20)
    ], result)
  end

  def test_data_timezone_preserves_datetime_type
    info = spring_fall_info
    timezone = DataTimezone.new(info)

    result = timezone.local_to_utc_intervals(
      DateTime.new(1970, 1, 1, 4, 26, 40),
      DateTime.new(1970, 1, 1, 4, 43, 20))

    assert(result.all? {|interval| interval.begin.kind_of?(DateTime) && interval.end.kind_of?(DateTime)})
    assert_equal([
      DateTime.new(1970, 1, 1, 3, 26, 40)...DateTime.new(1970, 1, 1, 3, 43, 20),
      DateTime.new(1970, 1, 1, 4, 26, 40)...DateTime.new(1970, 1, 1, 4, 43, 20)
    ], result)
  end

  def test_nil_start_and_end
    info = spring_fall_info

    error = assert_raises(ArgumentError) { info.local_to_utc_intervals(nil, local_timestamp(10000)) }
    assert_equal('local_start_timestamp must be specified', error.message)

    error = assert_raises(ArgumentError) { info.local_to_utc_intervals(local_timestamp(10000), nil) }
    assert_equal('local_end_timestamp must be specified', error.message)
  end

  def test_end_before_start
    info = spring_fall_info

    error = assert_raises(ArgumentError) do
      info.local_to_utc_intervals(local_timestamp(10000), local_timestamp(Rational(29999, 3)))
    end

    assert_equal('local_end_timestamp must be greater than or equal to local_start_timestamp', error.message)
  end

  def test_specified_offsets_are_ignored_at_data_timezone_boundary
    info = spring_fall_info
    timezone = DataTimezone.new(info)

    result = timezone.local_to_utc_intervals(
      Timestamp.utc(16000),
      Timestamp.new(13400, 0, 3600))

    assert_intervals_equal([utc_range(12400, 13400), utc_range(16000, 17000)], result)
  end
end
