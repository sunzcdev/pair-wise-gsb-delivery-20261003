# encoding: UTF-8
# frozen_string_literal: true

require_relative '../test_utils'

module DataSources
  class TCConstantOffsetDataTimezoneInfo < Minitest::Test
    include TZInfo
    include TZInfo::DataSources

    def test_initialize
      offset = TimezoneOffset.new(-17900, 0, 'TESTLMT')
      identifier = 'Test/Zone'.dup
      refute(identifier.frozen?)
      i = ConstantOffsetDataTimezoneInfo.new(identifier, offset)

      assert_same(identifier, i.identifier)
      assert_same(offset, i.constant_offset)
      assert(identifier.frozen?)
    end

    def test_initialize_nil_identifier
      o = TimezoneOffset.new(-17900, 0, 'TESTLMT')

      error = assert_raises(ArgumentError) { ConstantOffsetDataTimezoneInfo.new(nil, o) }
      assert_match(/\bidentifier\b/, error.message)
    end

    def test_initialize_nil_constant_offset
      error = assert_raises(ArgumentError) { ConstantOffsetDataTimezoneInfo.new('Test/Zone', nil) }
      assert_match(/\bconstant_offset\b/, error.message)
    end

    def test_period_for
      o = TimezoneOffset.new(-17900, 0, 'TESTLMT')
      i = ConstantOffsetDataTimezoneInfo.new('Test/Zone', o)

      assert_equal(OffsetTimezonePeriod.new(o), i.period_for(Timestamp.for(Time.utc(2017,1,1,0,0,0))))
      assert_equal(OffsetTimezonePeriod.new(o), i.period_for(Timestamp.for(Time.new(2017,1,1,0,0,0,0))))
      assert_equal(OffsetTimezonePeriod.new(o), i.period_for(Timestamp.for(Time.new(2017,1,1,1,0,0,3600))))
    end

    def test_periods_for_local
      o = TimezoneOffset.new(-17900, 0, 'TESTLMT')
      i = ConstantOffsetDataTimezoneInfo.new('Test/Zone', o)

      assert_equal([OffsetTimezonePeriod.new(o)], i.periods_for_local(Timestamp.for(Time.utc(2017,1,1,0,0,0), :ignore)))
    end

    def test_periods_for_local_batch
      o = TimezoneOffset.new(-17900, 0, 'TESTLMT')
      i = ConstantOffsetDataTimezoneInfo.new('Test/Zone', o)

      t1 = Timestamp.for(Time.utc(2017,1,1,0,0,0), :ignore)
      t2 = Timestamp.for(Time.utc(2018,6,1,12,30,0), :ignore)

      results = i.periods_for_local_batch([t1, t2, t1])

      assert_equal(3, results.length)
      expected = [OffsetTimezonePeriod.new(o)]
      assert_equal([expected, expected, expected], results)
    end

    def test_periods_for_local_batch_invalid_entries_isolated
      o = TimezoneOffset.new(-17900, 0, 'TESTLMT')
      i = ConstantOffsetDataTimezoneInfo.new('Test/Zone', o)

      t1 = Timestamp.for(Time.utc(2017,1,1,0,0,0), :ignore)
      with_offset = Timestamp.for(Time.utc(2017,1,1,0,0,0))

      results = i.periods_for_local_batch([nil, t1, with_offset])

      assert_equal(3, results.length)
      assert_kind_of(ArgumentError, results[0])
      assert_equal([OffsetTimezonePeriod.new(o)], results[1])
      assert_kind_of(ArgumentError, results[2])
    end

    def test_periods_for_local_batch_empty
      i = ConstantOffsetDataTimezoneInfo.new('Test/Zone', TimezoneOffset.new(-17900, 0, 'TESTLMT'))
      assert_equal([], i.periods_for_local_batch([]))
    end

    def test_periods_for_local_batch_nil
      i = ConstantOffsetDataTimezoneInfo.new('Test/Zone', TimezoneOffset.new(-17900, 0, 'TESTLMT'))
      error = assert_raises(ArgumentError) { i.periods_for_local_batch(nil) }
      assert_equal('local_timestamps must be specified', error.message)
    end

    def test_transitions_up_to
      i = ConstantOffsetDataTimezoneInfo.new('Test/Zone', TimezoneOffset.new(-17900, 0, 'TESTLMT'))

      assert_equal([], i.transitions_up_to(Timestamp.for(Time.utc(2017,1,1,0,0,0))))
    end

    def test_inspect
      i = ConstantOffsetDataTimezoneInfo.new('Test/Zone', TimezoneOffset.new(0, 0, 'TEST'))
      assert_equal('#<TZInfo::DataSources::ConstantOffsetDataTimezoneInfo: Test/Zone>', i.inspect)
    end
  end
end
