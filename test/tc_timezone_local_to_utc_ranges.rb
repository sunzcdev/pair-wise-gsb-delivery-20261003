# encoding: UTF-8
# frozen_string_literal: true

require_relative 'test_utils'

class TCTimezoneLocalToUtcRanges < Minitest::Test
  include TZInfo

  class IntervalTestTimezone < Timezone
    def initialize(identifier, transitions)
      super()
      @identifier = identifier
      @transitions = transitions
    end

    attr_reader :transitions

    def identifier
      @identifier
    end

    def period_for(time)
      timestamp = TZInfo::Timestamp.for(time)
      index = (0...@transitions.length).bsearch {|i| @transitions[i].at > timestamp } || @transitions.length

      start_transition = index > 0 ? @transitions[index - 1] : nil
      end_transition = @transitions[index]
      TZInfo::TransitionsTimezonePeriod.new(start_transition, end_transition)
    end

    def transitions_up_to(to, from = nil)
      @transitions.select do |transition|
        transition.at < to && (!from || transition.at >= from)
      end
    end
  end

  def local_second(second, sub_second = 0)
    Timestamp.new(second, sub_second, nil)
  end

  def utc_second(second, sub_second = 0)
    Timestamp.utc(second, sub_second)
  end

  def range(start_second, end_second, start_sub_second = 0, end_sub_second = start_sub_second)
    Range.new(utc_second(start_second, start_sub_second), utc_second(end_second, end_sub_second), true)
  end

  def offset(seconds)
    TimezoneOffset.new(seconds, 0, "O#{seconds}")
  end

  def exhaustive_timezone
    offsets = [
      offset(241),
      offset(161),
      offset(81),
      offset(1),
      offset(-79),
      offset(-159),
      offset(-239)
    ]

    transitions = (1..6).map do |index|
      TimezoneTransition.new(offsets[index], offsets[index - 1], index * 100)
    end

    IntervalTestTimezone.new('Test/Interval', transitions)
  end

  def expected_ranges(tz, local_start, local_end)
    start_value = Rational(local_start.value) + local_start.sub_second
    end_value = Rational(local_end.value) + local_end.sub_second

    transitions = [nil, *tz.transitions.map(&:timestamp_value), nil]
    offsets = [241, 161, 81, 1, -79, -159, -239]

    raw_ranges = offsets.each_with_index.map do |offset_seconds, index|
      period_start = transitions[index] && Rational(transitions[index]) + offset_seconds
      period_end = transitions[index + 1] && Rational(transitions[index + 1]) + offset_seconds
      intersection_start = period_start ? [start_value, period_start].max : start_value
      intersection_end = period_end ? [end_value, period_end].min : end_value

      next unless intersection_start < intersection_end

      Range.new(intersection_start - offset_seconds, intersection_end - offset_seconds, true)
    end.compact.sort_by(&:first)

    raw_ranges.each_with_object([]) do |candidate, merged|
      if merged.any? && merged.last.last >= candidate.first
        previous = merged.pop
        Range.new(previous.first, [previous.last, candidate.last].max, true)
      else
        candidate
      end.tap {|result| merged << result }
    end
  end

  def assert_ranges_equal(expected, actual)
    assert_equal(expected.length, actual.length)

    expected.zip(actual).each do |expected_range, actual_range|
      assert(actual_range.exclude_end?)
      assert_equal(expected_range.first, actual_range.first)
      assert_equal(expected_range.last, actual_range.last)
    end
  end

  def test_constant_offset
    offset = TimezoneOffset.new(19800, 0, 'TEST')
    transition = TimezoneTransition.new(offset, offset, 0)
    tz = IntervalTestTimezone.new('Test/Constant', [transition])

    result = tz.local_to_utc_ranges(local_second(100, Rational(1, 3)), local_second(110, Rational(2, 3)))

    assert_ranges_equal([range(100 - 19800, 110 - 19800, Rational(1, 3), Rational(2, 3))], result)
  end

  def test_supported_time_types_preserve_sub_seconds
    time_types_test(:output) do |helper|
      offset = TimezoneOffset.new(19800, 0, 'TEST')
      transition = TimezoneTransition.new(offset, offset, 0)
      tz = IntervalTestTimezone.new('Test/Constant', [transition])

      local_start = helper.time(1970, 1, 1, 2, 1, 40, Rational(1, 3), 0)
      local_end = helper.time(1970, 1, 1, 2, 1, 50, Rational(2, 3), 0)
      result = tz.local_to_utc_ranges(local_start, local_end).first

      assert_kind_of(local_start.class, result.first)
      assert_kind_of(local_start.class, result.last)
      assert_equal(Rational(1, 3), Timestamp.for(result.first).sub_second)
      assert_equal(Rational(2, 3), Timestamp.for(result.last).sub_second)
    end
  end

  def test_empty_half_open_interval
    boundary = local_second(100, Rational(1, 7))
    assert_equal([], exhaustive_timezone.local_to_utc_ranges(boundary, boundary))
  end

  def test_invalid_interval
    tz = exhaustive_timezone

    error = assert_raises(ArgumentError) do
      tz.local_to_utc_ranges(local_second(101), local_second(100))
    end

    assert_equal('local_end must be greater than or equal to local_start', error.message)
  end

  def test_skipped_local_time_has_no_utc_moments
    offsets = [offset(0), offset(3600)]
    transition = TimezoneTransition.new(offsets.last, offsets.first, 100)
    tz = IntervalTestTimezone.new('Test/SpringForward', [transition])

    assert_equal(
      [],
      tz.local_to_utc_ranges(local_second(100, Rational(1, 1000)), local_second(101, Rational(999, 1000)))
    )
  end

  def test_interval_crossing_skipped_local_time_merges_adjacent_utc_ranges
    offsets = [offset(0), offset(3600)]
    transition = TimezoneTransition.new(offsets.last, offsets.first, 100)
    tz = IntervalTestTimezone.new('Test/SpringForward', [transition])

    result = tz.local_to_utc_ranges(local_second(90, Rational(1, 1000)), local_second(3700, Rational(999, 1000)))

    assert_ranges_equal(
      [Range.new(utc_second(90, Rational(1, 1000)), utc_second(100, Rational(999, 1000)), true)],
      result
    )
  end

  def test_repeated_local_time_has_two_disjoint_utc_ranges
    offsets = [offset(3600), offset(0)]
    transition = TimezoneTransition.new(offsets.last, offsets.first, -3500)
    tz = IntervalTestTimezone.new('Test/FallBack', [transition])

    result = tz.local_to_utc_ranges(local_second(0, Rational(1, 1000)), local_second(50, Rational(999, 1000)))

    assert_ranges_equal(
      [
        range(-3600, -3550, Rational(1, 1000), Rational(999, 1000)),
        range(0, 50, Rational(1, 1000), Rational(999, 1000))
      ],
      result
    )
  end

  def test_interval_across_repeated_local_time_is_one_utc_range
    offsets = [offset(3600), offset(0)]
    transition = TimezoneTransition.new(offsets.last, offsets.first, -3500)
    tz = IntervalTestTimezone.new('Test/FallBack', [transition])

    result = tz.local_to_utc_ranges(local_second(-3500), local_second(110, Rational(999, 1000)))

    assert_ranges_equal([range(-7100, 110, 0, Rational(999, 1000))], result)
  end

  def test_non_integer_hour_transition
    offsets = [offset(19800), offset(16200)]
    transition = TimezoneTransition.new(offsets.last, offsets.first, -16100)
    tz = IntervalTestTimezone.new('Test/NonHour', [transition])

    result = tz.local_to_utc_ranges(local_second(100, Rational(1, 7)), local_second(105, Rational(2, 7)))

    assert_ranges_equal(
      [
        range(100 - 19800, 105 - 19800, Rational(1, 7), Rational(2, 7)),
        range(-16100, 105 - 16200, Rational(1, 7), Rational(2, 7))
      ],
      result
    )
  end

  def test_interval_crossing_non_integer_hour_repeated_local_time_merges_adjacent_ranges
    offsets = [offset(19800), offset(16200)]
    transition = TimezoneTransition.new(offsets.last, offsets.first, -16100)
    tz = IntervalTestTimezone.new('Test/NonHour', [transition])

    result = tz.local_to_utc_ranges(local_second(90, Rational(1, 7)), local_second(3700, Rational(2, 7)))

    assert_ranges_equal(
      [range(90 - 19800, 3700 - 16200, Rational(1, 7), Rational(2, 7))],
      result
    )
  end

  def test_range_argument
    tz = exhaustive_timezone
    local_range = Range.new(local_second(150, Rational(1, 3)), local_second(250, Rational(2, 3)), true)

    assert_equal(
      tz.local_to_utc_ranges(local_second(150, Rational(1, 3)), local_second(250, Rational(2, 3))),
      tz.local_to_utc_ranges(local_range)
    )
  end

  def test_closed_range_is_rejected
    error = assert_raises(ArgumentError) do
      exhaustive_timezone.local_to_utc_ranges(Range.new(local_second(1), local_second(2), false))
    end

    assert_equal('local range must be half-open (use ... rather than ..)', error.message)
  end

  def test_nil_arguments
    tz = exhaustive_timezone

    assert_equal('local_start must be specified', assert_raises(ArgumentError) { tz.local_to_utc_ranges(nil, local_second(1)) }.message)
    assert_equal('local_end must be specified', assert_raises(ArgumentError) { tz.local_to_utc_ranges(local_second(1), nil) }.message)
  end

  def test_returns_time_type_of_local_start
    tz = Timezone.get('Europe/London')
    local_start = Time.utc(2004, 10, 31, 0, 30)
    local_end = Time.utc(2004, 10, 31, 2, 30)

    result = tz.local_to_utc_ranges(local_start, local_end)

    assert(result.all? {|candidate| candidate.first.is_a?(Time) && candidate.last.is_a?(Time) && candidate.first.utc? && candidate.last.utc? })
  end

  def test_exhaustive_small_timezone_boundaries
    tz = exhaustive_timezone

    boundaries = [
      0,
      99,
      100,
      101,
      179,
      180,
      181,
      259,
      260,
      261,
      359,
      360,
      361,
      362,
      379,
      380,
      381,
      382,
      399,
      400,
      401,
      402,
      339,
      340,
      341,
      459,
      460,
      461,
      462,
      479,
      480,
      481,
      482,
      519,
      520,
      521,
      522,
      539,
      540,
      541,
      542,
      419,
      420,
      421,
      440,
      441,
      442,
      500,
      700
    ].flat_map do |second|
      [
        local_second(second),
        local_second(second, Rational(1, 1000)),
        local_second(second, Rational(999, 1000))
      ]
    end.uniq.sort

    five_candidate_start = local_second(340)
    five_candidate_end = local_second(340, Rational(1, 1000))
    assert_equal(5, tz.local_to_utc_ranges(five_candidate_start, five_candidate_end).length)

    boundaries.each do |start_boundary|
      boundaries.each do |end_boundary|
        next if end_boundary < start_boundary

        expected = expected_ranges(tz, start_boundary, end_boundary).map do |candidate|
          Range.new(utc_timestamp_for_rational(candidate.first), utc_timestamp_for_rational(candidate.last), true)
        end

        actual = tz.local_to_utc_ranges(start_boundary, end_boundary)
        assert_ranges_equal(expected, actual)
      end
    end
  end

  private

  def utc_timestamp_for_rational(seconds)
    value = seconds.floor
    sub_second = seconds - value
    Timestamp.utc(value, sub_second == 0 ? 0 : sub_second)
  end
end
