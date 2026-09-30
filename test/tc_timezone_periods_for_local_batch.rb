# encoding: UTF-8
# frozen_string_literal: true

require_relative 'test_utils'

# Integration tests for the batch local time resolution entry point
# (Timezone#periods_for_local_batch), exercising the shared rule traversal
# implemented by TransitionsDataTimezoneInfo and comparing the results with the
# single item Timezone#periods_for_local method.
class TCTimezonePeriodsForLocalBatch < Minitest::Test
  include TZInfo

  # Returns the local wall-clock Times around every offset change between 1990
  # and 2026 for the given zone, including sub-second boundaries, plus some
  # ordinary times. The inputs are returned in an arbitrary order and include
  # duplicates and invalid entries.
  def batch_inputs(tz)
    to = Timestamp.utc(Time.utc(2026, 1, 1).to_i)
    from = Timestamp.utc(Time.utc(1990, 1, 1).to_i)
    transitions = tz.transitions_up_to(to, from)

    inputs = []

    sub_seconds = [0, Rational(1, 2), Rational(1, 1000), Rational(999, 1000)]

    transitions.each do |transition|
      [-1, 0, 1].each do |delta_second|
        [transition.previous_offset, transition.offset].each do |offset|
          sub_seconds.each do |sub_second|
            value = transition.timestamp_value + offset.observed_utc_offset + delta_second
            inputs << Timestamp.new(value, sub_second, nil)
          end
        end
      end
    end

    (1990..2025).each do |year|
      (1..12).each do |month|
        inputs << Timestamp.for(Time.utc(year, month, 15, 12, 0, 0), :ignore)
      end
      end

    shuffled = inputs.shuffle(random: Random.new(1234))

    mixed = []
    shuffled.each_with_index do |input, index|
      mixed << input
      mixed << input if (index % 41).zero?    # duplicate entry
      mixed << nil if (index % 67).zero?      # invalid entry
    end

    mixed
  end

  def assert_batch_matches_single(tz, inputs)
    results = tz.periods_for_local_batch(inputs)

    assert_equal(inputs.length, results.length)

    results.each_with_index do |result, index|
      input = inputs[index]
      if input.nil?
        assert_kind_of(ArgumentError, result)
      else
        assert_kind_of(Array, result)
        assert_equal(tz.periods_for_local(input), result)
      end
    end

    results
  end

  ['America/New_York', 'Europe/London', 'Australia/Melbourne',
   'America/Argentina/Buenos_Aires', 'Europe/Prague'].each do |identifier|
    define_method("test_batch_matches_single_#{identifier.gsub(/[^a-z0-9]+/i, '_')}") do
      tz = Timezone.get(identifier)
      assert_batch_matches_single(tz, batch_inputs(tz))
    end
  end

  def test_results_classified_as_none_unique_and_multiple
    tz = Timezone.get('America/New_York')

    none      = Time.utc(2004, 4, 4, 2,30, 0)    # spring forward gap
    unique    = Time.utc(2004, 7, 1,12, 0, 0)    # ordinary summer time
    ambiguous = Time.utc(2004,10,31, 1,30, 0)    # fall back repeated hour

    results = tz.periods_for_local_batch([none, unique, ambiguous])

    assert_equal(0, results[0].length)
    assert_equal(1, results[1].length)
    assert_equal(2, results[2].length)
    assert_equal(['EDT', 'EST'], results[2].map(&:zone_identifier))
  end

  def test_arbitrary_order_preserved
    tz = Timezone.get('America/New_York')

    none      = Time.utc(2004, 4, 4, 2,30, 0)
    unique    = Time.utc(2004, 7, 1,12, 0, 0)
    ambiguous = Time.utc(2004,10,31, 1,30, 0)

    inputs = [ambiguous, none, unique, ambiguous, none]
    results = tz.periods_for_local_batch(inputs)

    assert_equal(2, results[0].length)
    assert_equal(0, results[1].length)
    assert_equal(1, results[2].length)
    assert_equal(results[0], results[3])
    assert_equal(results[1], results[4])
    refute_same(results[0], results[3])
    refute_same(results[1], results[4])
  end

  def test_invalid_entry_does_not_discard_other_results
    tz = Timezone.get('America/New_York')
    unique = Time.utc(2004, 7, 1,12, 0, 0)

    results = tz.periods_for_local_batch([nil, unique, 42, unique])

    assert_equal(4, results.length)

    assert_kind_of(ArgumentError, results[0])
    assert_match(/\blocal_time\b/, results[0].message)

    assert_equal(tz.periods_for_local(unique), results[1])

    assert_kind_of(ArgumentError, results[2])
    assert_match(/\bInteger\b/, results[2].message)

    assert_equal(tz.periods_for_local(unique), results[3])
  end

  def test_empty_batch
    tz = Timezone.get('America/New_York')
    assert_equal([], tz.periods_for_local_batch([]))
  end

  def test_nil_batch
    tz = Timezone.get('America/New_York')
    error = assert_raises(ArgumentError) { tz.periods_for_local_batch(nil) }
    assert_equal('local_times must be specified', error.message)
  end

  def test_sub_second_boundary_candidates_match_single
    tz = Timezone.get('America/New_York')

    # The 2004 fall back transition occurs at 06:00 UTC. The local wall clock
    # repeats the 01:00 hour, so local 01:59:59.x is ambiguous and local
    # 02:00:00.x is unambiguous (EST). Test immediately either side of the
    # boundary with sub-second precision.
    inputs = [
      Time.utc(2004,10,31, 1,59,59, Rational(999,1000)),
      Time.utc(2004,10,31, 2, 0, 0, Rational(1,1000)),
      Time.utc(2004,10,31, 2, 0, 0, Rational(1,2)),
      Time.utc(2004, 4, 4, 1,59,59, Rational(999,1000)),
      Time.utc(2004, 4, 4, 3, 0, 0, Rational(0,1))
    ]

    results = tz.periods_for_local_batch(inputs)
    inputs.each_with_index do |input, index|
      assert_equal(tz.periods_for_local(input), results[index])
    end

    assert_equal(2, results[0].length)
    assert_equal(1, results[1].length)
    assert_equal(1, results[2].length)
  end

  def test_accepts_time_datetime_and_timestamp
    tz = Timezone.get('America/New_York')

    time      = Time.utc(2004, 7, 1, 12, 0, 0)
    datetime  = DateTime.new(2004, 7, 1, 12, 0, 0)
    timestamp = Timestamp.for(time, :ignore)

    results = tz.periods_for_local_batch([time, datetime, timestamp])

    expected = tz.periods_for_local(time)
    assert_equal([expected, expected, expected], results)
  end

  def test_linked_timezone_batch
    # Link zones are only distinguished from data zones by the Ruby data
    # source. The Zoneinfo data source returns all zones as DataTimezone.
    info = DataSource.get.get_timezone_info('UTC')
    skip unless info.kind_of?(DataSources::LinkedTimezoneInfo)

    # 'UTC' is a link to 'Etc/UTC' with the Ruby data source.
    linked = Timezone.get('UTC')
    canonical = linked.canonical_zone

    assert_kind_of(LinkedTimezone, linked)

    inputs = [
      Time.utc(2004, 7, 1, 12, 0, 0),
      Time.utc(2000, 3, 1, 2, 0, 0),
      Time.utc(2010, 1, 1, 0, 0, 0)
    ]

    results = linked.periods_for_local_batch(inputs)
    assert_equal(canonical.periods_for_local_batch(inputs), results)
    inputs.each_with_index do |input, index|
      assert_equal(canonical.periods_for_local(input), results[index])
    end
  end

  def test_proxy_batch
    proxy = Timezone.get_proxy('America/New_York')
    tz = Timezone.get('America/New_York')

    inputs = [
      Time.utc(2004, 4, 4, 2,30, 0),
      Time.utc(2004,10,31, 1,30, 0),
      Time.utc(2004, 7, 1,12, 0, 0)
    ]

    assert_equal(tz.periods_for_local_batch(inputs), proxy.periods_for_local_batch(inputs))
  end
end
