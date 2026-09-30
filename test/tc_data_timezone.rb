# encoding: UTF-8
# frozen_string_literal: true

require_relative 'test_utils'

class TCDataTimezone < Minitest::Test
  include TZInfo

  class TestTimezoneInfo < DataSources::TimezoneInfo
    attr_reader :timestamp
    attr_reader :local_timestamp
    attr_reader :to_timestamp
    attr_reader :from_timestamp
    attr_reader :batch_local_timestamps

    def initialize(identifier, period_for_result, periods_for_local_result, transitions_up_to_result)
      super(identifier)
      @period_for_result = period_for_result
      @periods_for_local_result = periods_for_local_result || []
      @transitions_up_to_result = transitions_up_to_result
      @periods_for_local_count = 0
      @periods_for_local_batch_count = 0
    end

    def periods_for_local_count
      @periods_for_local_count
    end

    def periods_for_local_batch_count
      @periods_for_local_batch_count
    end

    def period_for(timestamp)
      raise ArgumentError, 'timestamp must be specified' unless timestamp
      raise ArgumentError, 'timestamp must have a specified utc_offset' unless timestamp.utc_offset

      @timestamp = timestamp
      @period_for_result
    end

    def periods_for_local(local_timestamp)
      raise ArgumentError, 'local_timestamp must be specified' unless local_timestamp
      raise ArgumentError, 'local_timestamp must have an unspecified utc_offset' if local_timestamp.utc_offset

      @periods_for_local_count += 1
      @local_timestamp = local_timestamp
      @periods_for_local_result
    end

    def periods_for_local_batch(local_timestamps)
      @periods_for_local_batch_count += 1
      @batch_local_timestamps = local_timestamps
      local_timestamps.map { @periods_for_local_result }
    end

    def transitions_up_to(to_timestamp, from_timestamp = nil)
      raise ArgumentError, 'to_timestamp must be specified' unless to_timestamp
      raise ArgumentError, 'to_timestamp must have a specified utc_offset' unless to_timestamp.utc_offset

      if from_timestamp
        raise ArgumentError, 'from_timestamp must have a specified utc_offset' unless from_timestamp.utc_offset
        raise ArgumentError, 'to_timestamp must be greater than from_timestamp' if to_timestamp <= from_timestamp
      end

      @to_timestamp = to_timestamp
      @from_timestamp = from_timestamp
      @transitions_up_to_result
    end
  end

  def test_identifier
    tz = DataTimezone.new(TestTimezoneInfo.new('Test/Zone', nil, [], []))
    assert_equal('Test/Zone', tz.identifier)
  end

  def test_period_for
    time_types_test(:utc) do |h|
      # Don't need actual TimezonePeriods. DataTimezone isn't supposed to do
      # anything with them apart from return them.
      period = Object.new
      tti = TestTimezoneInfo.new('Test/Zone', period, [], [])
      tz = DataTimezone.new(tti)

      assert_same(period, tz.period_for(h.time(2006, 6, 27, 22, 50, 12, Rational(1,10), :utc)))
      assert_equal_with_offset(Timestamp.create(2006, 6, 27, 22, 50, 12, Rational(1,10), :utc), tti.timestamp)
    end
  end

  def test_period_for_zero_offset
    time_types_test do |h|
      # Don't need actual TimezonePeriods. DataTimezone isn't supposed to do
      # anything with them apart from return them.
      period = Object.new
      tti = TestTimezoneInfo.new('Test/Zone', period, [], [])
      tz = DataTimezone.new(tti)

      assert_same(period, tz.period_for(h.time(2006, 6, 27, 22, 50, 12, Rational(1,10), 0)))
      assert_equal_with_offset(Timestamp.create(2006, 6, 27, 22, 50, 12, Rational(1,10), h.expected_zero_offset), tti.timestamp)
    end
  end

  def test_period_for_offset
    time_types_test(:offset) do |h|
      # Don't need actual TimezonePeriods. DataTimezone isn't supposed to do
      # anything with them apart from return them.
      period = Object.new
      tti = TestTimezoneInfo.new('Test/Zone', period, [], [])
      tz = DataTimezone.new(tti)

      assert_same(period, tz.period_for(h.time(2006, 6, 27, 22, 50, 12, Rational(1,10), 3600)))
      assert_equal_with_offset(Timestamp.create(2006, 6, 27, 22, 50, 12, Rational(1,10), 3600), tti.timestamp)
    end
  end

  def test_period_for_unspecified
    time_types_test(:unspecified_offset) do |h|
      # Don't need actual TimezonePeriods. DataTimezone isn't supposed to do
      # anything with them apart from return them.
      tti = TestTimezoneInfo.new('Test/Zone', Object.new, [], [])
      tz = DataTimezone.new(tti)
      t = h.time(2006, 6, 27, 22, 50, 12, Rational(1,10))

      error = assert_raises(ArgumentError) { tz.period_for(t) }
      assert_match(/\btime\b/, error.message)
    end
  end

  def test_period_for_nil
    tti = TestTimezoneInfo.new('Test/Zone', Object.new, [], [])
    tz = DataTimezone.new(tti)

    error = assert_raises(ArgumentError) { tz.period_for(nil) }
    assert_match(/\btime\b/, error.message)
  end

  def test_period_for_unsupported_value
    tti = TestTimezoneInfo.new('Test/Zone', Object.new, [], [])
    tz = DataTimezone.new(tti)
    t = Time.utc(2006, 6, 27, 22, 50, 12).to_i

    error = assert_raises(ArgumentError) { tz.period_for(t) }
    assert_match(Regexp.new("\\b#{Regexp.escape(t.class.name)}\\b"), error.message)
  end

  def test_periods_for_local_unspecified
    time_types_test(:unspecified_offset) do |h|
      # Don't need actual TimezonePeriods. DataTimezone isn't supposed to do
      # anything with them apart from return them.
      periods = [Object.new, Object.new]
      tti = TestTimezoneInfo.new('Test/Zone', nil, periods, [])
      tz = DataTimezone.new(tti)

      assert_same(periods, tz.periods_for_local(h.time(2006, 6, 27, 22, 50, 12, Rational(1,10))))
      assert_equal_with_offset(Timestamp.create(2006, 6, 27, 22, 50, 12, Rational(1,10)), tti.local_timestamp)
    end
  end

  def test_periods_for_local_utc
    time_types_test(:utc) do |h|
      # Don't need actual TimezonePeriods. DataTimezone isn't supposed to do
      # anything with them apart from return them.
      periods = [Object.new, Object.new]
      tti = TestTimezoneInfo.new('Test/Zone', nil, periods, [])
      tz = DataTimezone.new(tti)

      assert_same(periods, tz.periods_for_local(h.time(2006, 6, 27, 22, 50, 12, Rational(1,10), :utc)))
      assert_equal_with_offset(Timestamp.create(2006, 6, 27, 22, 50, 12, Rational(1,10)), tti.local_timestamp)
    end
  end

  def test_periods_for_local_zero_offset
    time_types_test do |h|
      # Don't need actual TimezonePeriods. DataTimezone isn't supposed to do
      # anything with them apart from return them.
      periods = [Object.new, Object.new]
      tti = TestTimezoneInfo.new('Test/Zone', nil, periods, [])
      tz = DataTimezone.new(tti)

      assert_same(periods, tz.periods_for_local(h.time(2006, 6, 27, 22, 50, 12, Rational(1,10), 0)))
      assert_equal_with_offset(Timestamp.create(2006, 6, 27, 22, 50, 12, Rational(1,10)), tti.local_timestamp)
    end
  end

  def test_periods_for_local_with_offset
    time_types_test(:offset) do |h|
      # Don't need actual TimezonePeriods. DataTimezone isn't supposed to do
      # anything with them apart from return them.
      periods = [Object.new, Object.new]
      tti = TestTimezoneInfo.new('Test/Zone', nil, periods, [])
      tz = DataTimezone.new(tti)

      assert_same(periods, tz.periods_for_local(h.time(2006, 6, 27, 22, 50, 12, Rational(1,10), 3600)))
      assert_equal_with_offset(Timestamp.create(2006, 6, 27, 22, 50, 12, Rational(1,10)), tti.local_timestamp)
    end
  end

  def test_periods_for_local_not_found
    periods = []
    tti = TestTimezoneInfo.new('Test/Zone', nil, periods, [])
    tz = DataTimezone.new(tti)

    t = Time.utc(2006, 6, 27, 22, 50, 12 + Rational(1,10))
    assert_same(periods, tz.periods_for_local(t))
    assert_equal_with_offset(Timestamp.new(t.to_i, Rational(1,10)), tti.local_timestamp)
  end

  def test_periods_for_local_nil
    tti = TestTimezoneInfo.new('Test/Zone', Object.new, [], [])
    tz = DataTimezone.new(tti)

    error = assert_raises(ArgumentError) { tz.periods_for_local(nil) }
    assert_match(/\blocal_time\b/, error.message)
  end

  def test_periods_for_local_unsupported
    tti = TestTimezoneInfo.new('Test/Zone', Object.new, [], [])
    tz = DataTimezone.new(tti)
    t = Time.utc(2006, 6, 27, 22, 50, 12).to_i

    error = assert_raises(ArgumentError) { tz.periods_for_local(t) }
    assert_match(Regexp.new("\\b#{Regexp.escape(t.class.name)}\\b"), error.message)
  end

  def test_periods_for_local_batch_unique_ambiguous_and_none
    time_types_test(:unspecified_offset) do |h|
      unique_period = Object.new
      ambiguous_periods = [Object.new, Object.new]
      tti = TestTimezoneInfo.new('Test/Zone', nil, ambiguous_periods, [])

      # Return a unique period, the ambiguous periods or none based on the
      # local timestamp's value. Record the call like the real method does.
      def tti.periods_for_local_batch(local_timestamps)
        @periods_for_local_batch_count += 1
        @batch_local_timestamps = local_timestamps
        local_timestamps.map do |t|
          case t.value
            when Time.utc(2006, 6, 27, 22, 50, 12).to_i then [@unique_period]
            when Time.utc(2006, 4, 2, 2, 30, 0).to_i then []
            else @periods_for_local_result
          end
        end
      end
      tti.instance_variable_set(:@unique_period, unique_period)

      tz = DataTimezone.new(tti)

      unique = h.time(2006, 6, 27, 22, 50, 12)
      ambiguous = h.time(2006, 10, 29, 1, 30, 0)
      none = h.time(2006, 4, 2, 2, 30, 0)

      results = tz.periods_for_local_batch([ambiguous, unique, none])

      assert_equal(3, results.length)
      assert_kind_of(LocalPeriodsResult, results[0])
      assert(results[0].ambiguous?)
      refute(results[0].unique?)
      refute(results[0].none?)
      refute(results[0].invalid?)
      assert_same(ambiguous_periods, results[0].periods)
      assert_nil(results[0].period)

      assert(results[1].unique?)
      assert_same(unique_period, results[1].period)
      assert_equal([unique_period], results[1].periods)

      assert(results[2].none?)
      assert_equal([], results[2].periods)
      assert_nil(results[2].period)

      # The info's batch method is called once, not the single item method.
      assert_equal(1, tti.periods_for_local_batch_count)
      assert_equal(0, tti.periods_for_local_count)
    end
  end

  def test_periods_for_local_batch_preserves_order_and_duplicates
    period = Object.new
    tti = TestTimezoneInfo.new('Test/Zone', nil, [period], [])

    # Return a distinct result Array for each item, as the real info layer
    # does, so duplicate inputs remain independent positions.
    def tti.periods_for_local_batch(local_timestamps)
      @periods_for_local_batch_count += 1
      @batch_local_timestamps = local_timestamps
      local_timestamps.map { [Object.new] }
    end

    tz = DataTimezone.new(tti)

    times = [
      Time.utc(2006, 6, 27, 22, 50, 12),
      Time.utc(2000, 1, 1, 0, 0, 0),
      Time.utc(2006, 6, 27, 22, 50, 12),
      Time.utc(2030, 1, 1, 0, 0, 0),
      Time.utc(2006, 6, 27, 22, 50, 12)
    ]

    results = tz.periods_for_local_batch(times)

    assert_equal(5, results.length)
    assert(results.all?(&:unique?))
    # Duplicates remain separate positions with independent result Arrays.
    refute_same(results[0].periods, results[2].periods)
    refute_same(results[2].periods, results[4].periods)

    # Timestamps passed to the info in the original order.
    assert_equal(times.map {|t| Timestamp.for(t, :ignore)}, tti.batch_local_timestamps)
  end

  def test_periods_for_local_batch_invalid_items_do_not_discard_others
    period = Object.new
    tti = TestTimezoneInfo.new('Test/Zone', nil, [period], [])
    tz = DataTimezone.new(tti)

    valid = Time.utc(2006, 6, 27, 22, 50, 12)
    inputs = [valid, nil, 12345, valid]

    results = tz.periods_for_local_batch(inputs)

    assert_equal(4, results.length)
    assert(results[0].unique?)
    assert(results[1].invalid?)
    assert_equal([], results[1].periods)
    assert_kind_of(ArgumentError, results[1].error)
    assert_match(/\blocal_time\b/, results[1].error.message)
    assert(results[2].invalid?)
    assert(results[3].unique?)

    # Only the two valid timestamps are passed to the shared info batch.
    assert_equal([Timestamp.for(valid, :ignore), Timestamp.for(valid, :ignore)], tti.batch_local_timestamps)
    assert_equal(1, tti.periods_for_local_batch_count)
    assert_equal(0, tti.periods_for_local_count)
  end

  def test_periods_for_local_batch_empty
    tti = TestTimezoneInfo.new('Test/Zone', nil, [], [])
    tz = DataTimezone.new(tti)

    assert_equal([], tz.periods_for_local_batch([]))
    assert_equal([], tz.periods_for_local_batch([]))
  end

  def test_periods_for_local_batch_nil
    tti = TestTimezoneInfo.new('Test/Zone', nil, [], [])
    tz = DataTimezone.new(tti)

    error = assert_raises(ArgumentError) { tz.periods_for_local_batch(nil) }
    assert_match(/\blocal_times\b/, error.message)
  end

  def test_transitions_up_to_utc
    time_types_test(:utc) do |h|
      # Don't need actual TimezoneTransition instances. DataTimezone isn't
      # supposed to do anything with them apart from return them.
      transitions = [Object.new, Object.new]
      tti = TestTimezoneInfo.new('Test/Zone', nil, nil, transitions)
      tz = DataTimezone.new(tti)

      assert_same(transitions, tz.transitions_up_to(h.time(2013, 1, 1, 0, 0, 0, Rational(1,10), :utc), h.time(2012, 1, 1, 0, 0, 0, Rational(1,10), :utc)))
      assert_equal_with_offset(Timestamp.create(2013, 1, 1, 0, 0, 0, Rational(1,10), :utc), tti.to_timestamp)
      assert_equal_with_offset(Timestamp.create(2012, 1, 1, 0, 0, 0, Rational(1,10), :utc), tti.from_timestamp)
    end
  end

  def test_transitions_up_to_zero_offset
    time_types_test do |h|
      # Don't need actual TimezoneTransition instances. DataTimezone isn't
      # supposed to do anything with them apart from return them.
      transitions = [Object.new, Object.new]
      tti = TestTimezoneInfo.new('Test/Zone', nil, nil, transitions)
      tz = DataTimezone.new(tti)

      assert_same(transitions, tz.transitions_up_to(h.time(2013, 1, 1, 0, 0, 0, Rational(1,10), 0), h.time(2012, 1, 1, 0, 0, 0, Rational(1,10), 0)))
      assert_equal_with_offset(Timestamp.create(2013, 1, 1, 0, 0, 0, Rational(1,10), h.expected_zero_offset), tti.to_timestamp)
      assert_equal_with_offset(Timestamp.create(2012, 1, 1, 0, 0, 0, Rational(1,10), h.expected_zero_offset), tti.from_timestamp)
    end
  end

  def test_transitions_up_to_offset
    time_types_test(:offset) do |h|
      # Don't need actual TimezoneTransition instances. DataTimezone isn't
      # supposed to do anything with them apart from return them.
      transitions = [Object.new, Object.new]
      tti = TestTimezoneInfo.new('Test/Zone', nil, nil, transitions)
      tz = DataTimezone.new(tti)

      assert_same(transitions, tz.transitions_up_to(h.time(2013, 1, 1, 0, 0, 0, Rational(1,10), -3600), h.time(2012, 1, 1, 0, 0, 0, Rational(1,10), 3600)))
      assert_equal_with_offset(Timestamp.create(2013, 1, 1, 0, 0, 0, Rational(1,10), -3600), tti.to_timestamp)
      assert_equal_with_offset(Timestamp.create(2012, 1, 1, 0, 0, 0, Rational(1,10), 3600), tti.from_timestamp)
    end
  end

  def test_transitions_up_to_utc_to_not_greater_than_utc_from
    time_types_test do |h|
      tti = TestTimezoneInfo.new('Test/Zone', nil, nil, nil)
      tz = DataTimezone.new(tti)

      to = h.time(2013,1,1,0,0,0,0,0)
      from = h.time(2013,1,1,0,0,0,0,0)

      error = assert_raises(ArgumentError) { tz.transitions_up_to(to, from) }
      assert_equal('to must be greater than from', error.message)
    end
  end

  def test_transitions_up_to_unspecified_from
    time_types_test(:unspecified_offset) do |h|
      tti = TestTimezoneInfo.new('Test/Zone', nil, nil, nil)
      tz = DataTimezone.new(tti)

      to = h.time(2013, 1, 1, 0, 0, 0, Rational(1,10), 0)
      from = h.time(2012, 1, 1, 0, 0, 0, Rational(1,10))

      error = assert_raises(ArgumentError) { tz.transitions_up_to(to, from) }
      assert_equal('from must have a specified utc_offset', error.message)
    end
  end

  def test_transitions_up_to_unspecified_to
    time_types_test(:unspecified_offset) do |h|
      tti = TestTimezoneInfo.new('Test/Zone', nil, nil, nil)
      tz = DataTimezone.new(tti)

      to = h.time(2013, 1, 1, 0, 0, 0, Rational(1,10))

      error = assert_raises(ArgumentError) { tz.transitions_up_to(to) }
      assert_equal('to must have a specified utc_offset', error.message)
    end
  end

  def test_transitions_up_to_nil_from
    # Don't need actual TimezoneTransition instances. DataTimezone isn't
    # supposed to do anything with them apart from return them.
    transitions = [Object.new, Object.new]
    tti = TestTimezoneInfo.new('Test/Zone', nil, nil, transitions)
    tz = DataTimezone.new(tti)

    to = Time.utc(2013, 1, 1, 0, 0, Rational(1,10))
    assert_same(transitions, tz.transitions_up_to(to))
    assert_equal_with_offset(Timestamp.new(to.to_i, Rational(1,10), :utc), tti.to_timestamp)
    assert_nil(tti.from_timestamp)
  end

  def test_transitions_up_to_nil_to
    tti = TestTimezoneInfo.new('Test/Zone', nil, nil, nil)
    tz = DataTimezone.new(tti)

    error = assert_raises(ArgumentError) { tz.transitions_up_to(nil) }
    assert_equal('to must be specified', error.message)
  end

  def test_transitions_up_to_unsupported_to
    tti = TestTimezoneInfo.new('Test/Zone', nil, nil, nil)
    tz = DataTimezone.new(tti)
    to = Time.utc(2013, 1, 1, 0, 0, 0).to_i

    error = assert_raises(ArgumentError) { tz.transitions_up_to(to) }
    assert_match(Regexp.new("\\b#{Regexp.escape(to.class.name)}\\b"), error.message)
  end

  def test_transitions_up_to_unsupported_from
    tti = TestTimezoneInfo.new('Test/Zone', nil, nil, nil)
    tz = DataTimezone.new(tti)
    to = Time.utc(2013, 1, 1, 0, 0, 0)
    from = Time.utc(2012, 1, 1, 0, 0, 0).to_i

    error = assert_raises(ArgumentError) { tz.transitions_up_to(to, from) }
    assert_match(Regexp.new("\\b#{Regexp.escape(from.class.name)}\\b"), error.message)
  end

  def test_canonical_identifier
    tz = DataTimezone.new(TestTimezoneInfo.new('Test/Zone', nil, [], []))
    assert_equal('Test/Zone', tz.canonical_identifier)
  end

  def test_canonical_zone
    tz = DataTimezone.new(TestTimezoneInfo.new('Test/Zone', nil, [], []))
    assert_same(tz, tz.canonical_zone)
  end

  def test_inspect
    tz = DataTimezone.new(TestTimezoneInfo.new('Test/Zone', nil, [], []))
    assert_equal('#<TZInfo::DataTimezone: Test/Zone>', tz.inspect)
  end
end
