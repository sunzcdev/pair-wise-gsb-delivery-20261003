# encoding: UTF-8
# frozen_string_literal: true

require_relative 'test_utils'

class TCDataSourceComparer < Minitest::Test
  include TZInfo

  # An in-memory DataSource that serves a fixed set of TimezoneInfo instances.
  class InMemoryDataSource < DataSource
    include TZInfo

    def initialize(timezone_infos)
      super()
      @timezone_infos = {}
      timezone_infos.each {|info| @timezone_infos[info.identifier] = info }
    end

    def data_timezone_identifiers
      @timezone_infos.values.select {|i| i.kind_of?(DataSources::DataTimezoneInfo) }.map(&:identifier).sort.freeze
    end

    def linked_timezone_identifiers
      @timezone_infos.values.select {|i| i.kind_of?(DataSources::LinkedTimezoneInfo) }.map(&:identifier).sort.freeze
    end

    protected

    def load_timezone_info(identifier)
      info = @timezone_infos[identifier]
      raise InvalidTimezoneIdentifier, "Invalid identifier: #{identifier}" unless info
      info
    end
  end

  STANDARD_OFFSET = TimezoneOffset.new(-18000, 0, 'TSTD').freeze
  DAYLIGHT_OFFSET = TimezoneOffset.new(-18000, 3600, 'TDST').freeze

  def setup
    @orig_data_source = DataSource.get

    # Two data sources that define the same identifiers, but with different
    # transition rules (as could be found in different tzdata releases).
    @first_data_source = InMemoryDataSource.new([
      transitions_info('Test/Zone', [STANDARD_OFFSET, DAYLIGHT_OFFSET, STANDARD_OFFSET],
        [Time.utc(2021, 3, 14, 7).to_i, Time.utc(2021, 11, 7, 6).to_i])
    ])

    @second_data_source = InMemoryDataSource.new([
      transitions_info('Test/Zone', [STANDARD_OFFSET, DAYLIGHT_OFFSET, STANDARD_OFFSET],
        [Time.utc(2021, 4, 4, 7).to_i, Time.utc(2021, 10, 31, 6).to_i])
    ])

    @comparer = DataSourceComparer.new(@first_data_source, @second_data_source)
  end

  def teardown
    DataSource.set(@orig_data_source)
  end

  def test_initialize_with_invalid_data_sources
    assert_raises(ArgumentError) { DataSourceComparer.new(nil, @second_data_source) }
    assert_raises(ArgumentError) { DataSourceComparer.new(@first_data_source, nil) }
    assert_raises(ArgumentError) { DataSourceComparer.new(:ruby, @second_data_source) }
  end

  def test_data_source_readers
    assert_same(@first_data_source, @comparer.first_data_source)
    assert_same(@second_data_source, @comparer.second_data_source)
  end

  def test_compare_returns_differing_intervals
    comparison = @comparer.compare('Test/Zone', Time.utc(2021, 1, 1), Time.utc(2022, 1, 1))

    assert_equal('Test/Zone', comparison.identifier)
    assert_equal(Timestamp.for(Time.utc(2021, 1, 1)), comparison.from)
    assert_equal(Timestamp.for(Time.utc(2022, 1, 1)), comparison.to)
    refute(comparison.empty?)
    refute(comparison.link_target_changed?)
    assert_nil(comparison.first_link_target)
    assert_nil(comparison.second_link_target)

    assert_equal([
      TimezoneDifference.new(
        Timestamp.for(Time.utc(2021, 3, 14, 7)), Timestamp.for(Time.utc(2021, 4, 4, 7)),
        TimezoneObservance.new(-14400, true, 'TDST'),
        TimezoneObservance.new(-18000, false, 'TSTD')),
      TimezoneDifference.new(
        Timestamp.for(Time.utc(2021, 10, 31, 6)), Timestamp.for(Time.utc(2021, 11, 7, 6)),
        TimezoneObservance.new(-14400, true, 'TDST'),
        TimezoneObservance.new(-18000, false, 'TSTD'))
    ], comparison.differences)
  end

  def test_compare_restricts_results_to_the_window
    comparison = @comparer.compare('Test/Zone', Time.utc(2021, 6, 1), Time.utc(2022, 1, 1))

    assert_equal([
      TimezoneDifference.new(
        Timestamp.for(Time.utc(2021, 10, 31, 6)), Timestamp.for(Time.utc(2021, 11, 7, 6)),
        TimezoneObservance.new(-14400, true, 'TDST'),
        TimezoneObservance.new(-18000, false, 'TSTD'))
    ], comparison.differences)
  end

  def test_compare_merges_adjacent_intervals_with_identical_differences
    # The second data source has an additional transition in the middle of a
    # differing interval that doesn't change the observance. The two differing
    # segments either side of that transition should be merged.
    second_data_source = InMemoryDataSource.new([
      transitions_info('Test/Zone',
        [STANDARD_OFFSET, DAYLIGHT_OFFSET, TimezoneOffset.new(-18000, 3600, 'TDST'), STANDARD_OFFSET],
        [Time.utc(2021, 4, 4, 7).to_i, Time.utc(2021, 4, 5, 7).to_i, Time.utc(2021, 10, 31, 6).to_i])
    ])

    comparison = DataSourceComparer.new(@first_data_source, second_data_source).
      compare('Test/Zone', Time.utc(2021, 1, 1), Time.utc(2022, 1, 1))

    assert_equal([
      TimezoneDifference.new(
        Timestamp.for(Time.utc(2021, 3, 14, 7)), Timestamp.for(Time.utc(2021, 4, 4, 7)),
        TimezoneObservance.new(-14400, true, 'TDST'),
        TimezoneObservance.new(-18000, false, 'TSTD')),
      TimezoneDifference.new(
        Timestamp.for(Time.utc(2021, 10, 31, 6)), Timestamp.for(Time.utc(2021, 11, 7, 6)),
        TimezoneObservance.new(-14400, true, 'TDST'),
        TimezoneObservance.new(-18000, false, 'TSTD'))
    ], comparison.differences)
  end

  def test_compare_with_no_differences_returns_empty_result
    comparer = DataSourceComparer.new(@first_data_source, @first_data_source)
    comparison = comparer.compare('Test/Zone', Time.utc(2021, 1, 1), Time.utc(2022, 1, 1))

    assert(comparison.empty?)
    assert_equal([], comparison.differences)
    assert_equal([], comparison.to_a)
  end

  def test_compare_window_within_a_single_second
    comparison = @comparer.compare('Test/Zone',
      Timestamp.create(2021, 3, 14, 7, 0, 0, Rational(1, 2), :utc),
      Timestamp.create(2021, 3, 14, 7, 0, 0, Rational(3, 4), :utc))

    assert_equal(1, comparison.differences.length)
    difference = comparison.differences.first
    assert_equal(TimezoneObservance.new(-14400, true, 'TDST'), difference.first)
    assert_equal(TimezoneObservance.new(-18000, false, 'TSTD'), difference.second)
  end

  def test_compare_constant_offset_with_transitions
    constant_data_source = InMemoryDataSource.new([
      DataSources::ConstantOffsetDataTimezoneInfo.new('Test/Zone', STANDARD_OFFSET)
    ])

    comparison = DataSourceComparer.new(constant_data_source, @first_data_source).
      compare('Test/Zone', Time.utc(2021, 1, 1), Time.utc(2022, 1, 1))

    assert_equal([
      TimezoneDifference.new(
        Timestamp.for(Time.utc(2021, 3, 14, 7)), Timestamp.for(Time.utc(2021, 11, 7, 6)),
        TimezoneObservance.new(-18000, false, 'TSTD'),
        TimezoneObservance.new(-14400, true, 'TDST'))
    ], comparison.differences)
  end

  def test_compare_reports_changed_link_target
    first_data_source = InMemoryDataSource.new([
      DataSources::LinkedTimezoneInfo.new('Test/Linked', 'Test/ZoneA'),
      transitions_info('Test/ZoneA', [STANDARD_OFFSET, DAYLIGHT_OFFSET], [Time.utc(2021, 3, 14, 7).to_i])
    ])
    second_data_source = InMemoryDataSource.new([
      DataSources::LinkedTimezoneInfo.new('Test/Linked', 'Test/ZoneB'),
      transitions_info('Test/ZoneB', [STANDARD_OFFSET, DAYLIGHT_OFFSET], [Time.utc(2021, 4, 4, 7).to_i])
    ])

    comparison = DataSourceComparer.new(first_data_source, second_data_source).
      compare('Test/Linked', Time.utc(2021, 1, 1), Time.utc(2022, 1, 1))

    assert(comparison.link_target_changed?)
    assert_equal('Test/ZoneA', comparison.first_link_target)
    assert_equal('Test/ZoneB', comparison.second_link_target)

    # The differences are resolved through the links within each data source.
    assert_equal([
      TimezoneDifference.new(
        Timestamp.for(Time.utc(2021, 3, 14, 7)), Timestamp.for(Time.utc(2021, 4, 4, 7)),
        TimezoneObservance.new(-14400, true, 'TDST'),
        TimezoneObservance.new(-18000, false, 'TSTD'))
    ], comparison.differences)
  end

  def test_compare_reports_unchanged_link_target
    target = transitions_info('Test/Zone', [STANDARD_OFFSET, DAYLIGHT_OFFSET], [Time.utc(2021, 3, 14, 7).to_i])
    first_data_source = InMemoryDataSource.new([DataSources::LinkedTimezoneInfo.new('Test/Linked', 'Test/Zone'), target])
    second_data_source = InMemoryDataSource.new([DataSources::LinkedTimezoneInfo.new('Test/Linked', 'Test/Zone'), target])

    comparison = DataSourceComparer.new(first_data_source, second_data_source).
      compare('Test/Linked', Time.utc(2021, 1, 1), Time.utc(2022, 1, 1))

    refute(comparison.link_target_changed?)
    assert_equal('Test/Zone', comparison.first_link_target)
    assert_equal('Test/Zone', comparison.second_link_target)
    assert(comparison.empty?)
  end

  def test_compare_reports_link_changed_to_data_zone
    first_data_source = InMemoryDataSource.new([
      DataSources::LinkedTimezoneInfo.new('Test/Zone', 'Test/Target'),
      transitions_info('Test/Target', [STANDARD_OFFSET, DAYLIGHT_OFFSET, STANDARD_OFFSET],
        [Time.utc(2021, 3, 14, 7).to_i, Time.utc(2021, 11, 7, 6).to_i])
    ])

    comparison = DataSourceComparer.new(first_data_source, @first_data_source).
      compare('Test/Zone', Time.utc(2021, 1, 1), Time.utc(2022, 1, 1))

    assert(comparison.link_target_changed?)
    assert_equal('Test/Target', comparison.first_link_target)
    assert_nil(comparison.second_link_target)
    assert(comparison.empty?)
  end

  def test_compare_raises_for_invalid_identifier
    error = assert_raises(InvalidTimezoneIdentifier) do
      @comparer.compare('Test/Unknown', Time.utc(2021, 1, 1), Time.utc(2022, 1, 1))
    end
    assert_match(/\bTest\/Unknown\b/, error.message)
  end

  def test_compare_raises_for_invalid_arguments
    assert_raises(ArgumentError) { @comparer.compare(nil, Time.utc(2021, 1, 1), Time.utc(2022, 1, 1)) }
    assert_raises(ArgumentError) { @comparer.compare('Test/Zone', nil, Time.utc(2022, 1, 1)) }
    assert_raises(ArgumentError) { @comparer.compare('Test/Zone', Time.utc(2021, 1, 1), nil) }
    assert_raises(ArgumentError) { @comparer.compare('Test/Zone', Time.utc(2022, 1, 1), Time.utc(2021, 1, 1)) }
    assert_raises(ArgumentError) { @comparer.compare('Test/Zone', Time.utc(2021, 1, 1), Time.utc(2021, 1, 1)) }
    assert_raises(ArgumentError) { @comparer.compare('Test/Zone', Timestamp.create(2021, 1, 1), Time.utc(2022, 1, 1)) }
    assert_raises(ArgumentError) { @comparer.compare('Test/Zone', Time.utc(2021, 1, 1), Timestamp.create(2022, 1, 1)) }
  end

  def test_compare_does_not_use_the_default_data_source
    # Replace the default data source with one that raises if used.
    DataSource.set(DataSource.new)

    comparison = @comparer.compare('Test/Zone', Time.utc(2021, 1, 1), Time.utc(2022, 1, 1))

    refute(comparison.empty?)
  end

  def test_interleaved_queries_do_not_pollute_the_default_data_source
    default_data_source = DataSource.get

    first_comparison = @comparer.compare('Test/Zone', Time.utc(2021, 1, 1), Time.utc(2022, 1, 1))
    refute(first_comparison.empty?)

    # Interleave queries against the default data source.
    london = Timezone.get('Europe/London')
    summer = london.period_for_utc(Time.utc(2021, 7, 1))
    assert_equal(3600, summer.observed_utc_offset)
    assert_equal(true, summer.dst?)
    assert_equal('BST', summer.abbreviation)

    second_comparison = @comparer.compare('Test/Zone', Time.utc(2021, 1, 1), Time.utc(2022, 1, 1))

    winter = london.period_for_utc(Time.utc(2021, 1, 1))
    assert_equal(0, winter.observed_utc_offset)
    assert_equal(false, winter.dst?)
    assert_equal('GMT', winter.abbreviation)

    # The comparisons are unaffected by the interleaved queries and the
    # default data source is unchanged and unaware of the test zones.
    assert_equal(first_comparison.differences, second_comparison.differences)
    assert_same(default_data_source, DataSource.get)
    assert_raises(InvalidTimezoneIdentifier) { Timezone.get('Test/Zone') }
  end

  private

  # Builds a TransitionsDataTimezoneInfo that observes offsets[0] initially
  # and transitions to offsets[i + 1] at transition_values[i].
  def transitions_info(identifier, offsets, transition_values)
    transitions = transition_values.each_with_index.map do |value, index|
      TimezoneTransition.new(offsets[index + 1], offsets[index], value)
    end
    DataSources::TransitionsDataTimezoneInfo.new(identifier, transitions)
  end
end
