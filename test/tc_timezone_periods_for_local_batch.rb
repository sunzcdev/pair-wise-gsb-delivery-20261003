# encoding: UTF-8
# frozen_string_literal: true

require_relative 'test_utils'

class TCLocalPeriodsResult < Minitest::Test
  include TZInfo

  def test_unique
    period = Object.new
    result = LocalPeriodsResult.new([period])

    assert(result.unique?)
    refute(result.ambiguous?)
    refute(result.none?)
    refute(result.invalid?)
    assert_same(period, result.period)
    assert_equal([period], result.periods)
    assert_nil(result.error)
  end

  def test_ambiguous
    periods = [Object.new, Object.new]
    result = LocalPeriodsResult.new(periods)

    assert(result.ambiguous?)
    refute(result.unique?)
    refute(result.none?)
    refute(result.invalid?)
    assert_nil(result.period)
    assert_same(periods, result.periods)
    assert_nil(result.error)
  end

  def test_none
    result = LocalPeriodsResult.new([])

    assert(result.none?)
    refute(result.unique?)
    refute(result.ambiguous?)
    refute(result.invalid?)
    assert_nil(result.period)
    assert_equal([], result.periods)
    assert_nil(result.error)
  end

  def test_invalid
    error = ArgumentError.new('bad')
    result = LocalPeriodsResult.new([], error)

    assert(result.invalid?)
    refute(result.unique?)
    refute(result.ambiguous?)
    refute(result.none?)
    assert_nil(result.period)
    assert_equal([], result.periods)
    assert_same(error, result.error)
  end
end

class TCTimezonePeriodsForLocalBatch < Minitest::Test
  include TZInfo

  # A minimal Timezone subclass that only implements the single item
  # periods_for_local method. The base class batch implementation must loop
  # this, counting calls.
  class BaseFallbackTimezone < Timezone
    attr_reader :periods_for_local_calls

    def initialize(identifier, results_by_value)
      super()
      @identifier = identifier
      @results_by_value = results_by_value
      @periods_for_local_calls = []
    end

    def identifier
      @identifier
    end

    def periods_for_local(local_time)
      raise ArgumentError, 'local_time must be specified' unless local_time
      @periods_for_local_calls << local_time
      @results_by_value[local_time.to_i] || []
    end
  end

  def test_base_class_fallback_loops_single_item
    p1 = Object.new
    p2a = Object.new
    p2b = Object.new
    t1 = Time.utc(2006, 1, 1, 0, 0, 0)
    t2 = Time.utc(2006, 6, 1, 0, 0, 0)
    t3 = Time.utc(2006, 4, 2, 2, 30, 0)

    tz = BaseFallbackTimezone.new('Test/Zone', t1.to_i => [p1], t2.to_i => [p2a, p2b])

    results = tz.periods_for_local_batch([t2, t1, t3])

    assert_equal(3, results.length)
    assert(results[0].ambiguous?)
    assert_equal([p2a, p2b], results[0].periods)
    assert(results[1].unique?)
    assert_same(p1, results[1].period)
    assert(results[2].none?)

    # The base implementation falls back to the single item method once per
    # position, preserving order.
    assert_equal([t2, t1, t3], tz.periods_for_local_calls)
  end

  def test_base_class_invalid_item_is_captured
    tz = BaseFallbackTimezone.new('Test/Zone', {})

    results = tz.periods_for_local_batch([nil])

    assert_equal(1, results.length)
    assert(results[0].invalid?)
    assert_kind_of(ArgumentError, results[0].error)
  end

  def test_nil_local_times
    tz = Timezone.get('America/New_York')
    error = assert_raises(ArgumentError) { tz.periods_for_local_batch(nil) }
    assert_match(/\blocal_times\b/, error.message)
  end

  def test_empty
    tz = Timezone.get('America/New_York')
    assert_equal([], tz.periods_for_local_batch([]))
  end

  def test_real_zone_unique_ambiguous_none_and_invalid_ordering
    tz = Timezone.get('America/New_York')

    unique_edt   = Time.utc(2004, 7, 1, 12, 0, 0)
    unique_est   = Time.utc(2004, 12, 1, 12, 0, 0)
    ambiguous    = Time.utc(2004, 10, 31, 1, 30, 0)
    gap          = Time.utc(2004, 4, 4, 2, 30, 0)
    invalid_nil  = nil
    invalid_int  = 12345
    duplicate    = unique_edt

    input = [gap, invalid_nil, ambiguous, duplicate, invalid_int, unique_est, unique_edt]
    results = tz.periods_for_local_batch(input)

    assert_equal(7, results.length)

    assert(results[0].none?)
    assert(results[1].invalid?)
    assert(results[2].ambiguous?)
    assert_equal([-14400, -18000], results[2].periods.map(&:observed_utc_offset))
    assert(results[3].unique?)
    assert_equal(-14400, results[3].period.observed_utc_offset)
    assert(results[4].invalid?)
    assert_kind_of(ArgumentError, results[4].error)
    assert(results[5].unique?)
    assert_equal(-18000, results[5].period.observed_utc_offset)
    assert(results[6].unique?)
    assert_equal(-14400, results[6].period.observed_utc_offset)

    # Duplicate positions have equal content.
    assert_equal(results[3].periods, results[6].periods)
  end

  def test_batch_matches_periods_for_local_across_many_offset_changes
    tz = Timezone.get('America/New_York')

    inputs = []

    # Cover spring-forward gaps and fall-back repeats across several years,
    # including sub-second values right at the boundaries.
    (1995..2020).each do |year|
      [3, 4, 10, 11].each do |month|
        (1..8).each do |day|
          [0, 1, 2, 3, 4, 5].each do |hour|
            [0, 59].each do |minute|
              [0, 30, 59].each do |second|
                [0, Rational(1,2), Rational(999999999,1000000000)].each do |sub|
                  inputs << Time.utc(year, month, day, hour, minute, second + sub)
                end
              end
            end
          end
        end
      end
    end

    # A constant-offset zone value, the earliest and latest years, and
    # duplicates.
    inputs.concat([
      Time.utc(1900, 1, 1, 0, 0, 0, Rational(7,13)),
      Time.utc(2100, 1, 1, 0, 0, 0),
      Time.utc(2004, 10, 31, 1, 30, 0, Rational(1,1000000)),
      Time.utc(2004, 10, 31, 1, 30, 0, Rational(1,1000000))
    ])

    shuffled = inputs.shuffle

    results = tz.periods_for_local_batch(shuffled)

    assert_equal(inputs.length, results.length)

    failures = 0
    shuffled.each_with_index do |local_time, index|
      expected = tz.periods_for_local(local_time)
      actual = results[index].periods
      if expected != actual
        failures += 1
        flunk("mismatch at #{local_time.utc.iso8601(12)}: expected #{expected.inspect}, got #{actual.inspect}") if failures == 1
      end
      refute(results[index].invalid?, "unexpected invalid result at #{local_time}")
    end

    assert_equal(0, failures)
  end

  def test_batch_matches_periods_for_local_for_constant_offset_zone
    tz = Timezone.get('Etc/UTC')

    inputs = [
      Time.utc(1900, 1, 1, 0, 0, 0, Rational(3,4)),
      Time.utc(2000, 6, 1, 12, 0, 0),
      Time.utc(2000, 6, 1, 12, 0, 0),
      Time.utc(2100, 1, 1, 0, 0, 0)
    ].shuffle

    results = tz.periods_for_local_batch(inputs)

    inputs.each_with_index do |local_time, index|
      assert_equal(tz.periods_for_local(local_time), results[index].periods)
      assert(results[index].unique?)
    end
  end

  def test_proxy_delegates
    proxy = Timezone.get_proxy('America/New_York')
    results = proxy.periods_for_local_batch([Time.utc(2004, 10, 31, 1, 30, 0), Time.utc(2004, 7, 1, 12, 0, 0)])

    assert(results[0].ambiguous?)
    assert(results[1].unique?)
  end

  def test_accepts_datetime_and_timestamp_inputs
    tz = Timezone.get('America/New_York')

    inputs = [
      DateTime.new(2004, 10, 31, 1, 30, 0),
      Timestamp.create(2004, 7, 1, 12, 0, 0),
      Time.utc(2004, 4, 4, 2, 30, 0)
    ]

    results = tz.periods_for_local_batch(inputs)

    assert(results[0].ambiguous?)
    assert(results[1].unique?)
    assert(results[2].none?)
  end
end
