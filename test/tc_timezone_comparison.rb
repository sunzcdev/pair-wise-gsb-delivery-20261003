# encoding: UTF-8

require_relative 'test_utils'

class TCTimezoneComparison < Minitest::Test
  include TZInfo

  class MemoryDataSource < DataSource
    def initialize(zones, links = {})
      super()
      @zones = zones
      @links = links
    end

    def data_timezone_identifiers
      @zones.keys.sort.freeze
    end

    def linked_timezone_identifiers
      @links.keys.sort.freeze
    end

    protected

      def load_timezone_info(identifier)
      if @zones.key?(identifier)
        TZInfo::DataSources::TransitionsDataTimezoneInfo.new(identifier, @zones[identifier])
      elsif @links.key?(identifier)
        TZInfo::DataSources::LinkedTimezoneInfo.new(identifier, @links[identifier])
      else
        raise InvalidTimezoneIdentifier, "Invalid identifier: #{identifier}"
      end
    end
  end

  class BlockingMemoryDataSource < MemoryDataSource
    def initialize(zones, links, ready_queue, continue_queue)
      super(zones, links)
      @ready_queue = ready_queue
      @continue_queue = continue_queue
    end

    protected

    def load_timezone_info(identifier)
      if identifier == 'Test/Zone'
        @ready_queue << object_id
        @continue_queue.pop
      end

      super
    end
  end

  def setup
    @orig_data_source = DataSource.get
  end

  def teardown
    DataSource.set(@orig_data_source)
  end

  def test_compares_offsets_dst_and_abbreviations_and_merges_equal_intervals
    first = MemoryDataSource.new(
      'Test/Zone' => transitions(
        offset(0, 0, 'STD'), utc(2020, 1, 1),
        [
          [utc(2020, 2, 1), offset(0, 3600, 'DST')],
          [utc(2020, 3, 1), offset(0, 0, 'STD')],
          [utc(2020, 4, 1), offset(0, 3600, 'DST')]
        ]
      )
    )

    second = MemoryDataSource.new(
      'Test/Zone' => transitions(
        offset(0, 0, 'STD'), utc(2020, 1, 1),
        [
          [utc(2020, 2, 15), offset(0, 3600, 'DST')],
          [utc(2020, 3, 15), offset(0, 3600, 'DST')],
          [utc(2020, 4, 1), offset(0, 3600, 'DST')]
        ]
      )
    )

    differences = TimezoneComparison.compare(
      'Test/Zone', first, second, utc(2020, 1, 1), utc(2020, 5, 1)
    )

    assert_equal [
      [utc(2020, 2, 1), utc(2020, 2, 15), 3600, 0, true, false, 'DST', 'STD'],
      [utc(2020, 3, 1), utc(2020, 4, 1), 0, 3600, false, true, 'STD', 'DST']
    ], differences.map {|difference|
      [
        difference.from, difference.to,
        difference.first_utc_offset, difference.second_utc_offset,
        difference.first_dst, difference.second_dst,
        difference.first_abbreviation, difference.second_abbreviation
      ]
    }
    refute(differences.first.link_target_changed?)
  end

  def test_returns_empty_result_when_identical
    transitions = transitions(
      offset(0, 0, 'STD'), utc(2020, 1, 1),
      [[utc(2020, 2, 1), offset(0, 3600, 'DST')]]
    )
    first = MemoryDataSource.new('Test/Zone' => transitions)
    second = MemoryDataSource.new('Test/Zone' => transitions)

    assert_equal([], TimezoneComparison.compare(
      'Test/Zone', first, second, utc(2020, 1, 1), utc(2020, 3, 1)
    ))
  end

  def test_timezone_class_provides_explicit_data_source_entry
    first = MemoryDataSource.new(
      'Test/Zone' => constant_transitions(offset(0, 0, 'OLD'))
    )
    second = MemoryDataSource.new(
      'Test/Zone' => constant_transitions(offset(3600, 0, 'NEW'))
    )

    differences = Timezone.compare_data_sources(
      'Test/Zone', first, second, utc(2020, 1, 1), utc(2020, 2, 1)
    )

    assert_equal TimezoneComparison.compare(
      'Test/Zone', first, second, utc(2020, 1, 1), utc(2020, 2, 1)
    ), differences
  end

  def test_reports_link_target_change_with_same_observance
    first = MemoryDataSource.new(
      {
        'Test/TargetA' => constant_transitions(offset(0, 0, 'STD')),
        'Test/TargetB' => constant_transitions(offset(0, 0, 'STD'))
      },
      'Test/Link' => 'Test/TargetA'
    )
    second = MemoryDataSource.new(
      {
        'Test/TargetA' => constant_transitions(offset(0, 0, 'STD')),
        'Test/TargetB' => constant_transitions(offset(0, 0, 'STD'))
      },
      'Test/Link' => 'Test/TargetB'
    )

    differences = TimezoneComparison.compare(
      'Test/Link', first, second, utc(2020, 1, 1), utc(2020, 2, 1)
    )

    assert_equal 1, differences.length
    difference = differences.first
    assert_equal utc(2020, 1, 1), difference.from
    assert_equal utc(2020, 2, 1), difference.to
    assert_equal 0, difference.first_utc_offset
    assert_equal 0, difference.second_utc_offset
    assert_equal 'Test/TargetA', difference.first_link_target
    assert_equal 'Test/TargetB', difference.second_link_target
    assert difference.link_target_changed?
  end

  def test_reports_canonical_link_target_change_when_direct_link_unchanged
    zones = {
      'Test/TargetA' => constant_transitions(offset(0, 0, 'STD')),
      'Test/TargetB' => constant_transitions(offset(0, 0, 'STD'))
    }
    first = MemoryDataSource.new(zones, 'Test/Link' => 'Test/TargetA')
    second = MemoryDataSource.new(
      { 'Test/TargetB' => constant_transitions(offset(0, 0, 'STD')) },
      'Test/TargetA' => 'Test/TargetB',
      'Test/Link' => 'Test/TargetA'
    )

    difference = TimezoneComparison.compare(
      'Test/Link', first, second, utc(2020, 1, 1), utc(2020, 2, 1)
    ).first

    assert_equal 'Test/TargetA', difference.first_link_target
    assert_equal 'Test/TargetA', difference.second_link_target
    assert_equal 'Test/TargetA', difference.first_canonical_link_target
    assert_equal 'Test/TargetB', difference.second_canonical_link_target
    assert difference.link_target_changed?
  end

  def test_explicit_sources_do_not_change_default_during_interleaved_queries
    default_source = MemoryDataSource.new(
      'Test/Zone' => constant_transitions(offset(0, 0, 'DEFAULT'))
    )
    ready_queue = Queue.new
    continue_queue = Queue.new
    first = BlockingMemoryDataSource.new(
      { 'Test/Zone' => constant_transitions(offset(0, 3600, 'FIRST')) },
      {}, ready_queue, continue_queue
    )
    second = BlockingMemoryDataSource.new(
      { 'Test/Zone' => constant_transitions(offset(3600, 3600, 'SECOND')) },
      {}, ready_queue, continue_queue
    )

    DataSource.set(default_source)
    queue = Queue.new

    thread = Thread.new do
      begin
        differences = TimezoneComparison.compare(
          'Test/Zone', first, second, utc(2020, 1, 1), utc(2020, 2, 1)
        )
        queue << [:ok, differences]
      rescue => error
        queue << [:error, error]
      end
    end

    2.times do
      ready_queue.pop
      assert_equal 'DEFAULT', Timezone.get('Test/Zone').abbreviation(utc(2020, 1, 15))
      assert_same default_source, DataSource.get
      continue_queue << true
    end

    thread.join
    status, differences = queue.pop

    assert_equal :ok, status
    assert_same default_source, DataSource.get
    assert_equal 1, differences.length
    assert_equal 3600, differences.first.first_utc_offset
    assert_equal 7200, differences.first.second_utc_offset
    assert_equal 'FIRST', differences.first.first_abbreviation
    assert_equal 'SECOND', differences.first.second_abbreviation
  end

  def test_supports_custom_info_with_zero_argument_create_timezone
    info_class = Class.new(TZInfo::DataSources::DataTimezoneInfo) do
      def create_timezone
        TZInfo::DataTimezone.new(self)
      end

      def period_for(timestamp)
        TZInfo::OffsetTimezonePeriod.new(TZInfo::TimezoneOffset.new(0, 0, 'CUSTOM'))
      end

      def transitions_up_to(to_timestamp, from_timestamp = nil)
        []
      end
    end

    custom_info = info_class.new('Test/Zone')
    source = MemoryDataSource.new({})
    source.define_singleton_method(:load_timezone_info) do |identifier|
      custom_info if identifier == 'Test/Zone'
    end

    differences = TimezoneComparison.compare(
      'Test/Zone',
      source,
      MemoryDataSource.new('Test/Zone' => constant_transitions(offset(0, 0, 'STD'))),
      utc(2020, 1, 1), utc(2020, 2, 1)
    )

    assert_equal 'CUSTOM', differences.first.first_abbreviation
    assert_equal 'STD', differences.first.second_abbreviation
  end

  def test_validates_data_sources
    source = MemoryDataSource.new(
      'Test/Zone' => constant_transitions(offset(0, 0, 'STD'))
    )

    assert_raises(ArgumentError) do
      TimezoneComparison.compare('Test/Zone', source, nil, utc(2020, 1, 1), utc(2020, 2, 1))
    end
  end

  private

  def offset(base_utc_offset, std_offset, abbreviation)
    TimezoneOffset.new(base_utc_offset, std_offset, abbreviation)
  end

  def transition(offset, at, previous_offset = nil)
    TimezoneTransition.new(offset, previous_offset, at.value)
  end

  def transitions(initial_offset, initial_at, changes)
    result = [transition(initial_offset, initial_at)]
    previous = initial_offset

    changes.each do |at, zone_offset|
      result << transition(zone_offset, at, previous)
      previous = zone_offset
    end

    result
  end

  def constant_transitions(zone_offset)
    [transition(zone_offset, Timestamp.utc(-2**62))]
  end

  def utc(year, month = 1, day = 1, hour = 0, minute = 0, second = 0)
    Timestamp.create(year, month, day, hour, minute, second, 0, :utc)
  end
end
