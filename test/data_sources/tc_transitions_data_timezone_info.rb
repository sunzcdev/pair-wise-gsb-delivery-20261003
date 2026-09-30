# encoding: UTF-8
# frozen_string_literal: true

require 'delegate'
require_relative '../test_utils'

module DataSources
  class TCTransitionsDataTimezoneInfo < Minitest::Test
    include TZInfo
    include TZInfo::DataSources

    def test_initialize_transitions
      o1 = TimezoneOffset.new(-17900,    0, 'TESTLMT')
      o2 = TimezoneOffset.new(-18000, 3600, 'TESTD')
      o3 = TimezoneOffset.new(-18000,    0, 'TESTS')

      t1 = TimezoneTransition.new(o2, o1, Time.utc(2000, 4,1,1,0,0).to_i)
      t2 = TimezoneTransition.new(o3, o2, Time.utc(2000,10,1,1,0,0).to_i)
      t3 = TimezoneTransition.new(o2, o3, Time.utc(2001, 3,1,1,0,0).to_i)

      transitions = [t1, t2, t3]
      identifier = 'Test/Zone'.dup
      refute(identifier.frozen?)

      i = TransitionsDataTimezoneInfo.new(identifier, transitions)

      assert_same(identifier, i.identifier)
      assert_same(transitions, i.transitions)
      assert_equal(true, i.identifier.frozen?)
      assert_equal(true, i.transitions.frozen?)
    end

    def test_initialize_empty_transitions
      error = assert_raises(ArgumentError) { TransitionsDataTimezoneInfo.new('Test/Zone', []) }
      assert_match(/\btransitions\b/, error.message)
    end

    def test_initialize_nil_identifier
      o1 = TimezoneOffset.new(-17900,    0, 'TESTLMT')
      o2 = TimezoneOffset.new(-18000,    0, 'TESTS')
      t1 = TimezoneTransition.new(o2, o1, Time.utc(2000,10,1,1,0,0).to_i)
      transitions = [t1]

      error = assert_raises(ArgumentError) { TransitionsDataTimezoneInfo.new(nil, transitions) }
      assert_match(/\bidentifier\b/, error.message)
    end

    def test_initialize_nil_transitions
      error = assert_raises(ArgumentError) { TransitionsDataTimezoneInfo.new('Test/Zone', nil) }
      assert_match(/\btransitions\b/, error.message)
    end

    def test_period_for
      o1 = TimezoneOffset.new(-17900, 0,    'TESTLMT')
      o2 = TimezoneOffset.new(-18000, 3600, 'TESTD')
      o3 = TimezoneOffset.new(-18000, 0,    'TESTS')
      o4 = TimezoneOffset.new(-21600, 3600, 'TESTD')

      t1 = TimezoneTransition.new(o2, o1, Time.utc(2000, 4,1,1,0,0).to_i)
      t2 = TimezoneTransition.new(o3, o2, Time.utc(2000,10,1,1,0,0).to_i)
      t3 = TimezoneTransition.new(o2, o3, Time.utc(2001, 3,1,1,0,0).to_i)
      t4 = TimezoneTransition.new(o4, o2, Time.utc(2001, 4,1,1,0,0).to_i)
      t5 = TimezoneTransition.new(o3, o4, Time.utc(2001,10,1,1,0,0).to_i)
      t6 = TimezoneTransition.new(o3, o3, Time.utc(2002,10,1,1,0,0).to_i)
      t7 = TimezoneTransition.new(o2, o3, Time.utc(2003, 2,1,1,0,0).to_i)
      t8 = TimezoneTransition.new(o3, o2, Time.utc(2003, 3,1,1,0,0).to_i)

      i = TransitionsDataTimezoneInfo.new('Test/Zone', [t1,t2,t3,t4,t5,t6,t7,t8])

      assert_equal(TransitionsTimezonePeriod.new(nil, t1), i.period_for(Timestamp.for(Time.utc(1960, 1,1,1, 0, 0))))
      assert_equal(TransitionsTimezonePeriod.new(nil, t1), i.period_for(Timestamp.for(Time.utc(1999,12,1,0, 0, 0))))
      assert_equal(TransitionsTimezonePeriod.new(nil, t1), i.period_for(Timestamp.for(Time.utc(2000, 4,1,0,59,59))))
      assert_equal(TransitionsTimezonePeriod.new(t1, t2),  i.period_for(Timestamp.for(Time.utc(2000, 4,1,1, 0, 0))))
      assert_equal(TransitionsTimezonePeriod.new(t1, t2),  i.period_for(Timestamp.for(Time.utc(2000,10,1,0,59,59))))
      assert_equal(TransitionsTimezonePeriod.new(t2, t3),  i.period_for(Timestamp.for(Time.utc(2000,10,1,1, 0, 0))))
      assert_equal(TransitionsTimezonePeriod.new(t2, t3),  i.period_for(Timestamp.for(Time.utc(2001, 3,1,0,59,59))))
      assert_equal(TransitionsTimezonePeriod.new(t3, t4),  i.period_for(Timestamp.for(Time.utc(2001, 3,1,1, 0, 0))))
      assert_equal(TransitionsTimezonePeriod.new(t3, t4),  i.period_for(Timestamp.for(Time.utc(2001, 4,1,0,59,59))))
      assert_equal(TransitionsTimezonePeriod.new(t4, t5),  i.period_for(Timestamp.for(Time.utc(2001, 4,1,1, 0, 0))))
      assert_equal(TransitionsTimezonePeriod.new(t4, t5),  i.period_for(Timestamp.for(Time.utc(2001,10,1,0,59,59))))
      assert_equal(TransitionsTimezonePeriod.new(t5, t6),  i.period_for(Timestamp.for(Time.utc(2001,10,1,1, 0, 0))))
      assert_equal(TransitionsTimezonePeriod.new(t5, t6),  i.period_for(Timestamp.for(Time.utc(2002, 2,1,1, 0, 0))))
      assert_equal(TransitionsTimezonePeriod.new(t5, t6),  i.period_for(Timestamp.for(Time.utc(2002,10,1,0,59,59))))
      assert_equal(TransitionsTimezonePeriod.new(t6, t7),  i.period_for(Timestamp.for(Time.utc(2002,10,1,1, 0, 0))))
      assert_equal(TransitionsTimezonePeriod.new(t6, t7),  i.period_for(Timestamp.for(Time.utc(2003, 2,1,0,59,59))))
      assert_equal(TransitionsTimezonePeriod.new(t7, t8),  i.period_for(Timestamp.for(Time.utc(2003, 2,1,1, 0, 0))))
      assert_equal(TransitionsTimezonePeriod.new(t7, t8),  i.period_for(Timestamp.for(Time.utc(2003, 3,1,0,59,59))))
      assert_equal(TransitionsTimezonePeriod.new(t8, nil), i.period_for(Timestamp.for(Time.utc(2003, 3,1,1, 0, 0))))
      assert_equal(TransitionsTimezonePeriod.new(t8, nil), i.period_for(Timestamp.for(Time.utc(2004, 1,1,1, 0, 0))))
      assert_equal(TransitionsTimezonePeriod.new(t8, nil), i.period_for(Timestamp.for(Time.utc(2050, 1,1,1, 0, 0))))
    end

    def test_period_for_timestamp_with_zero_utc_offset
      o1 = TimezoneOffset.new(-17900, 0, 'TESTLMT')
      o2 = TimezoneOffset.new(-18000, 0, 'TEST')

      t1 = TimezoneTransition.new(o2, o1, Time.utc(2000,7,1,0,0,0).to_i)

      i = TransitionsDataTimezoneInfo.new('Test/Zone', [t1])

      assert_equal(TransitionsTimezonePeriod.new(t1, nil), i.period_for(Timestamp.for(Time.new(2000,7,1,0,0,0,0))))
    end

    def test_period_for_timestamp_with_non_zero_utc_offset
      o1 = TimezoneOffset.new(-17900, 0, 'TESTLMT')
      o2 = TimezoneOffset.new(-18000, 0, 'TEST')

      t1 = TimezoneTransition.new(o2, o1, Time.utc(2000,7,1,0,0,0).to_i)

      i = TransitionsDataTimezoneInfo.new('Test/Zone', [t1])

      assert_equal(TransitionsTimezonePeriod.new(t1, nil), i.period_for(Timestamp.for(Time.new(2000,6,30,23, 0, 0,-3600))))
      assert_equal(TransitionsTimezonePeriod.new(nil, t1), i.period_for(Timestamp.for(Time.new(2000,7, 1, 0,59,59, 3600))))
    end

    def create_basic_info
      o1 = TimezoneOffset.new(-17900, 0, 'TESTLMT')
      o2 = TimezoneOffset.new(-18000, 0, 'TEST')

      t1 = TimezoneTransition.new(o2, o1, Time.utc(2000,7,1,0,0,0).to_i)

      TransitionsDataTimezoneInfo.new('Test/Zone', [t1])
    end

    def test_period_for_timestamp_with_unspecified_offset
      i = create_basic_info
      error = assert_raises(ArgumentError) { i.period_for(Timestamp.for(Time.utc(2005,1,1,0,0,0), :ignore)) }
      assert_equal('timestamp must have a specified utc_offset', error.message)
    end

    def test_period_for_nil
      i = create_basic_info
      error = assert_raises(ArgumentError) { i.period_for(nil) }
      assert_equal('timestamp must be specified', error.message)
    end

    def test_periods_for_local
      o1 = TimezoneOffset.new(-17900,    0, 'TESTLMT')
      o2 = TimezoneOffset.new(-18000, 3600, 'TESTD')
      o3 = TimezoneOffset.new(-18000,    0, 'TESTS')
      o4 = TimezoneOffset.new(-21600, 3600, 'TESTD')

      t1 = TimezoneTransition.new(o2, o1, Time.utc(2000, 4,2,1,0,0).to_i)
      t2 = TimezoneTransition.new(o3, o2, Time.utc(2000,10,2,1,0,0).to_i)
      t3 = TimezoneTransition.new(o2, o3, Time.utc(2001, 3,2,1,0,0).to_i)
      t4 = TimezoneTransition.new(o4, o2, Time.utc(2001, 4,2,1,0,0).to_i)
      t5 = TimezoneTransition.new(o3, o4, Time.utc(2001,10,2,1,0,0).to_i)
      t6 = TimezoneTransition.new(o3, o3, Time.utc(2002,10,2,1,0,0).to_i)
      t7 = TimezoneTransition.new(o2, o3, Time.utc(2003, 2,2,1,0,0).to_i)

      i = TransitionsDataTimezoneInfo.new('Test/Zone', [t1,t2,t3,t4,t5,t6,t7])

      assert_equal([TransitionsTimezonePeriod.new(nil, t1)], i.periods_for_local(Timestamp.for(Time.utc(1960, 1, 1, 1, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(nil, t1)], i.periods_for_local(Timestamp.for(Time.utc(1999,12, 1, 0, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(nil, t1)], i.periods_for_local(Timestamp.for(Time.utc(2000, 1, 1,10, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(nil, t1)], i.periods_for_local(Timestamp.for(Time.utc(2000, 4, 1,20, 1,39), :ignore)))
      assert_equal([],                            i.periods_for_local(Timestamp.for(Time.utc(2000, 4, 1,20, 1,40), :ignore)))
      assert_equal([],                            i.periods_for_local(Timestamp.for(Time.utc(2000, 4, 1,20,59,59), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t1,  t2)], i.periods_for_local(Timestamp.for(Time.utc(2000, 4, 1,21, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t1,  t2)], i.periods_for_local(Timestamp.for(Time.utc(2000,10, 1,19,59,59), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t1,  t2),
                    TransitionsTimezonePeriod.new(t2,  t3)], i.periods_for_local(Timestamp.for(Time.utc(2000,10, 1,20, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t1,  t2),
                    TransitionsTimezonePeriod.new(t2,  t3)], i.periods_for_local(Timestamp.for(Time.utc(2000,10, 1,20,59,59), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t2,  t3)], i.periods_for_local(Timestamp.for(Time.utc(2000,10, 1,21, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t2,  t3)], i.periods_for_local(Timestamp.for(Time.utc(2001, 3, 1,19,59,59), :ignore)))
      assert_equal([],                            i.periods_for_local(Timestamp.for(Time.utc(2001, 3, 1,20, 0, 0), :ignore)))
      assert_equal([],                            i.periods_for_local(Timestamp.for(Time.utc(2001, 3, 1,20,30, 0), :ignore)))
      assert_equal([],                            i.periods_for_local(Timestamp.for(Time.utc(2001, 3, 1,20,59,59), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t3,  t4)], i.periods_for_local(Timestamp.for(Time.utc(2001, 3, 1,21, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t3,  t4)], i.periods_for_local(Timestamp.for(Time.utc(2001, 4, 1,19,59,59), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t3,  t4),
                    TransitionsTimezonePeriod.new(t4,  t5)], i.periods_for_local(Timestamp.for(Time.utc(2001, 4, 1,20, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t3,  t4),
                    TransitionsTimezonePeriod.new(t4,  t5)], i.periods_for_local(Timestamp.for(Time.utc(2001, 4, 1,20,59,59), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t4,  t5)], i.periods_for_local(Timestamp.for(Time.utc(2001, 4, 1,21, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t4,  t5)], i.periods_for_local(Timestamp.for(Time.utc(2001,10, 1,19,59,59), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t5,  t6)], i.periods_for_local(Timestamp.for(Time.utc(2001,10, 1,20, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t5,  t6)], i.periods_for_local(Timestamp.for(Time.utc(2002, 2, 1,20, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t5,  t6)], i.periods_for_local(Timestamp.for(Time.utc(2002,10, 1,19,59,59), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t6,  t7)], i.periods_for_local(Timestamp.for(Time.utc(2002,10, 1,20, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t6,  t7)], i.periods_for_local(Timestamp.for(Time.utc(2003, 2, 1,19,59,59), :ignore)))
      assert_equal([],                            i.periods_for_local(Timestamp.for(Time.utc(2003, 2, 1,20, 0, 0), :ignore)))
      assert_equal([],                            i.periods_for_local(Timestamp.for(Time.utc(2003, 2, 1,20,59,59), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t7, nil)], i.periods_for_local(Timestamp.for(Time.utc(2003, 2, 1,21, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t7, nil)], i.periods_for_local(Timestamp.for(Time.utc(2004, 2, 1,20, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t7, nil)], i.periods_for_local(Timestamp.for(Time.utc(2040, 2, 1,20, 0, 0), :ignore)))
    end

    def test_periods_for_local_warsaw
      o1 = TimezoneOffset.new(5040,    0, 'LMT')
      o2 = TimezoneOffset.new(5040,    0, 'WMT')
      o3 = TimezoneOffset.new(3600,    0, 'CET')
      o4 = TimezoneOffset.new(3600, 3600, 'CEST')

      t1 = TimezoneTransition.new(o2, o1, Time.utc(1879,12,31,22,36,0).to_i)
      t2 = TimezoneTransition.new(o3, o2, Time.utc(1915, 8, 4,22,36,0).to_i)
      t3 = TimezoneTransition.new(o4, o3, Time.utc(1916, 4,30,22, 0,0).to_i)

      i = TransitionsDataTimezoneInfo.new('Test/Europe/Warsaw', [t1,t2,t3])

      assert_equal([TransitionsTimezonePeriod.new(t1, t2),
                    TransitionsTimezonePeriod.new(t2, t3)], i.periods_for_local(Timestamp.for(Time.utc(1915,8,4,23,40,0), :ignore)))
    end

    def test_periods_for_local_single_transition
      o1 = TimezoneOffset.new(-3600, 0, 'TESTD')
      o2 = TimezoneOffset.new(-3600, 0, 'TESTS')

      t1 = TimezoneTransition.new(o2, o1, Time.utc(2000,7,1,0,0,0).to_i)

      i = TransitionsDataTimezoneInfo.new('Test/Zone', [t1])

      assert_equal([TransitionsTimezonePeriod.new(nil, t1)], i.periods_for_local(Timestamp.for(Time.utc(2000,6,30,22,59,59), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t1, nil)], i.periods_for_local(Timestamp.for(Time.utc(2000,6,30,23, 0, 0), :ignore)))
      assert_equal([TransitionsTimezonePeriod.new(t1, nil)], i.periods_for_local(Timestamp.for(Time.utc(2000,7, 1, 0, 0, 0), :ignore)))
    end

    def test_periods_for_local_timestamp_with_specified_offset
      i = create_basic_info
      error = assert_raises(ArgumentError) { i.periods_for_local(Timestamp.for(Time.utc(2005,1,1,0,0,0))) }
      assert_equal('local_timestamp must have an unspecified utc_offset', error.message)
    end

    def test_periods_for_local_nil
      i = create_basic_info
      error = assert_raises(ArgumentError) { i.periods_for_local(nil) }
      assert_equal('local_timestamp must be specified', error.message)
    end

    # Wraps an Array recording how many times its elements are accessed via
    # integer indexing and how many elements bsearch_index yields.
    class AccessCountingArray < SimpleDelegator
      attr_reader :index_accesses
      attr_reader :bsearch_accesses

      def initialize(array)
        super
        @index_accesses = 0
        @bsearch_accesses = 0
      end

      def [](index)
        @index_accesses += 1 if index.kind_of?(Integer)
        __getobj__[index]
      end

      def bsearch_index
        __getobj__.bsearch_index do |element|
          @bsearch_accesses += 1
          yield element
        end
      end

      def freeze
        # Ignore freezing so the counters keep working.
        self
      end
    end

    # A subclass of TransitionsDataTimezoneInfo that records how many times the
    # single item periods_for_local method is called and how many individual
    # transitions are accessed while resolving local timestamps.
    class CountingTransitionsDataTimezoneInfo < TransitionsDataTimezoneInfo
      attr_reader :counting_transitions

      def initialize(identifier, transitions)
        @periods_for_local_count = 0
        @counting_transitions = AccessCountingArray.new(transitions)
        super(identifier, @counting_transitions)
      end

      def periods_for_local_count
        @periods_for_local_count
      end

      def index_accesses
        @counting_transitions.index_accesses
      end

      def bsearch_accesses
        @counting_transitions.bsearch_accesses
      end

      def periods_for_local(local_timestamp)
        @periods_for_local_count += 1
        super
      end
    end

    def local_timestamp(year, month, day, hour, minute, second, sub_second = 0)
      Timestamp.for(Time.utc(year, month, day, hour, minute, second + sub_second), :ignore)
    end

    def test_periods_for_local_batch
      o1 = TimezoneOffset.new(-17900,    0, 'TESTLMT')
      o2 = TimezoneOffset.new(-18000, 3600, 'TESTD')
      o3 = TimezoneOffset.new(-18000,    0, 'TESTS')

      t1 = TimezoneTransition.new(o2, o1, Time.utc(2000, 4,2,1,0,0).to_i)
      t2 = TimezoneTransition.new(o3, o2, Time.utc(2000,10,2,1,0,0).to_i)
      t3 = TimezoneTransition.new(o2, o3, Time.utc(2001, 3,2,1,0,0).to_i)
      t4 = TimezoneTransition.new(o3, o2, Time.utc(2001,10,2,1,0,0).to_i)
      t5 = TimezoneTransition.new(o2, o3, Time.utc(2002, 3,2,1,0,0).to_i)
      t6 = TimezoneTransition.new(o3, o2, Time.utc(2002,10,2,1,0,0).to_i)
      t7 = TimezoneTransition.new(o2, o3, Time.utc(2003, 3,2,1,0,0).to_i)

      i = TransitionsDataTimezoneInfo.new('Test/Zone', [t1,t2,t3,t4,t5,t6,t7])

      unique     = local_timestamp(2000, 1, 1, 10, 0, 0)
      gap        = local_timestamp(2000, 4, 1, 20, 30, 0)
      ambiguous  = local_timestamp(2000,10, 1, 20, 30, 0)
      before_all = local_timestamp(1960, 1, 1,  1,  0, 0)
      after_all  = local_timestamp(2040, 2, 1, 20,  0, 0)

      # Input in arbitrary order with a duplicate (ambiguous appears twice).
      input = [gap, unique, ambiguous, after_all, ambiguous, before_all]
      result = i.periods_for_local_batch(input)

      expected = [
        [],
        [TransitionsTimezonePeriod.new(nil, t1)],
        [TransitionsTimezonePeriod.new(t1, t2), TransitionsTimezonePeriod.new(t2, t3)],
        [TransitionsTimezonePeriod.new(t7, nil)],
        [TransitionsTimezonePeriod.new(t1, t2), TransitionsTimezonePeriod.new(t2, t3)],
        [TransitionsTimezonePeriod.new(nil, t1)]
      ]

      assert_equal(expected, result)
    end

    def test_periods_for_local_batch_preserves_input_order_and_duplicates
      o1 = TimezoneOffset.new(-17900,    0, 'TESTLMT')
      o2 = TimezoneOffset.new(-18000, 3600, 'TESTD')
      o3 = TimezoneOffset.new(-18000,    0, 'TESTS')

      t1 = TimezoneTransition.new(o2, o1, Time.utc(2000, 4,2,1,0,0).to_i)
      t2 = TimezoneTransition.new(o3, o2, Time.utc(2000,10,2,1,0,0).to_i)

      i = TransitionsDataTimezoneInfo.new('Test/Zone', [t1, t2])

      t = local_timestamp(2000,10,1,20,30,0)
      after_fallback = local_timestamp(2000,10,1,21,30,0)
      ambiguous = [TransitionsTimezonePeriod.new(t1, t2), TransitionsTimezonePeriod.new(t2, nil)]

      # Same value repeated, supplied in reverse chronological order. The
      # duplicates must remain separate positions and map to identical results.
      input = [t, local_timestamp(2000,1,1,0,0,0), t, local_timestamp(1999,1,1,0,0,0), t, after_fallback]
      result = i.periods_for_local_batch(input)

      assert_equal(6, result.length)
      assert_equal(ambiguous, result[0])
      assert_equal([TransitionsTimezonePeriod.new(nil, t1)], result[1])
      assert_equal(ambiguous, result[2])
      assert_equal([TransitionsTimezonePeriod.new(nil, t1)], result[3])
      assert_equal(ambiguous, result[4])
      assert_equal([TransitionsTimezonePeriod.new(t2, nil)], result[5])
    end

    def test_periods_for_local_batch_subsecond_boundaries
      o1 = TimezoneOffset.new(-18000,    0, 'TESTS')
      o2 = TimezoneOffset.new(-14400, 3600, 'TESTD')
      o3 = TimezoneOffset.new(-18000,    0, 'TESTS')

      # Transition at 2000-04-02 01:00:00 UTC.
      t1 = TimezoneTransition.new(o2, o1, Time.utc(2000, 4,2,1,0,0).to_i)
      # Transition at 2000-10-29 06:00:00 UTC.
      t2 = TimezoneTransition.new(o3, o2, Time.utc(2000,10,29,6,0,0).to_i)

      i = TransitionsDataTimezoneInfo.new('Test/Zone', [t1, t2])

      # Local times at the sub-second boundaries of the gap and repeated hour.
      input = [
        local_timestamp(2000, 3, 4, 1, 59, 59, Rational(999,1000)),
        local_timestamp(2000, 4, 2, 2,  0,  0, Rational(1,1000000)),
        local_timestamp(2000,10,29, 1, 59, 59, Rational(999,1000)),
        local_timestamp(2000,10,29, 1,  0,  0, Rational(1,1000000))
      ].shuffle

      result = i.periods_for_local_batch(input)

      input.each_with_index do |local_timestamp, index|
        assert_equal(i.periods_for_local(local_timestamp), result[index],
          "batch result differs from periods_for_local at #{local_timestamp}")
      end
    end

    def test_periods_for_local_batch_matches_periods_for_local
      o1 = TimezoneOffset.new(-17900,    0, 'TESTLMT')
      o2 = TimezoneOffset.new(-18000, 3600, 'TESTD')
      o3 = TimezoneOffset.new(-18000,    0, 'TESTS')

      transitions = []
      year = 2000
      8.times do
        transitions << TimezoneTransition.new(o2, transitions.last ? transitions.last.offset : o1, Time.utc(year, 4,2,1,0,0).to_i)
        transitions << TimezoneTransition.new(o3, o2, Time.utc(year,10,2,1,0,0).to_i)
        year += 1
      end

      i = TransitionsDataTimezoneInfo.new('Test/Zone', transitions)

      # Inputs spanning multiple offset changes, in arbitrary order, including
      # sub-seconds and duplicates.
      input = []
      (2000..2007).each do |y|
        [1, 4, 10].each do |month|
          [1, 2].each do |day|
            [19, 20, 21].each do |hour|
              [0, 30, 59].each do |minute|
                [0, Rational(1,3), Rational(999999,1000000)].each do |sub|
                  input << local_timestamp(y, month, day, hour, minute, sub == 0 ? 0 : 59, sub)
                end
              end
            end
          end
        end
      end
      input << local_timestamp(2000,10,1,20,30,0)
      input << local_timestamp(2000,10,1,20,30,0)
      input.shuffle!

      result = i.periods_for_local_batch(input)

      assert_equal(input.length, result.length)
      input.each_with_index do |local_timestamp, index|
        assert_equal(i.periods_for_local(local_timestamp), result[index],
          "batch result differs from periods_for_local at #{local_timestamp}")
      end
    end

    def test_periods_for_local_batch_empty
      i = create_basic_info
      assert_equal([], i.periods_for_local_batch([]))
    end

    def test_periods_for_local_batch_shares_rule_traversal
      o1 = TimezoneOffset.new(-17900,    0, 'TESTLMT')
      o2 = TimezoneOffset.new(-18000, 3600, 'TESTD')
      o3 = TimezoneOffset.new(-18000,    0, 'TESTS')

      transitions = []
      year = 2000
      12.times do
        transitions << TimezoneTransition.new(o2, transitions.last ? transitions.last.offset : o1, Time.utc(year, 4,2,1,0,0).to_i)
        transitions << TimezoneTransition.new(o3, o2, Time.utc(year,10,2,1,0,0).to_i)
        year += 1
      end

      # Many local times spanning multiple offset changes, including repeated
      # values.
      input = []
      (2000..2011).each do |y|
        [1, 4, 7, 10].each do |month|
          input << local_timestamp(y, month, 1, 20, 30, 0)
          input << local_timestamp(y, month, 1, 20, 30, 0)
        end
      end
      input.shuffle!

      # Count transitions accessed when looping the single item interface.
      loop_info = CountingTransitionsDataTimezoneInfo.new('Test/Zone', transitions)
      input.each {|local_timestamp| loop_info.periods_for_local(local_timestamp) }

      # Count transitions accessed with a single batch call.
      batch_info = CountingTransitionsDataTimezoneInfo.new('Test/Zone', transitions)
      batch_info.periods_for_local_batch(input)

      # The batch path must not simply loop the single item interface: the
      # single item periods_for_local method is never called, and no binary
      # searches are performed.
      assert_equal(input.length, loop_info.periods_for_local_count)
      assert_equal(0, batch_info.periods_for_local_count)
      assert_equal(0, batch_info.bsearch_accesses)
      assert_operator(loop_info.bsearch_accesses, :>, 0)

      # Sharing the traversal must access fewer transitions in total than
      # repeatedly binary searching for each item.
      loop_accesses = loop_info.bsearch_accesses + loop_info.index_accesses
      batch_accesses = batch_info.bsearch_accesses + batch_info.index_accesses
      assert_operator(batch_accesses, :<, loop_accesses,
        "expected batch (#{batch_accesses}) to access fewer transitions than looping (#{loop_accesses})")
    end

    def test_periods_for_local_batch_nil
      i = create_basic_info
      error = assert_raises(ArgumentError) { i.periods_for_local_batch([nil]) }
      assert_equal('local_timestamp must be specified', error.message)
    end

    def test_periods_for_local_batch_nil_amongst_others
      i = create_basic_info
      input = [local_timestamp(2000,1,1,0,0,0), nil, local_timestamp(2000,1,1,0,0,0)]
      error = assert_raises(ArgumentError) { i.periods_for_local_batch(input) }
      assert_equal('local_timestamp must be specified', error.message)
    end

    def test_periods_for_local_batch_with_specified_offset
      i = create_basic_info
      error = assert_raises(ArgumentError) { i.periods_for_local_batch([Timestamp.for(Time.utc(2005,1,1,0,0,0))]) }
      assert_equal('local_timestamp must have an unspecified utc_offset', error.message)
    end

    def test_transitions_up_to
      o1 = TimezoneOffset.new(-17900,    0, 'TESTLMT')
      o2 = TimezoneOffset.new(-18000, 3600, 'TESTD')
      o3 = TimezoneOffset.new(-18000,    0, 'TESTS')
      o4 = TimezoneOffset.new(-21600, 3600, 'TESTD')

      t1 = TimezoneTransition.new(o2, o1, Time.utc(2010, 4,1,1,0,0).to_i)
      t2 = TimezoneTransition.new(o3, o2, Time.utc(2010,10,1,1,0,0).to_i)
      t3 = TimezoneTransition.new(o2, o3, Time.utc(2011, 3,1,1,0,0).to_i)
      t4 = TimezoneTransition.new(o4, o2, Time.utc(2011, 4,1,1,0,0).to_i)
      t5 = TimezoneTransition.new(o3, o4, Time.utc(2011,10,1,1,0,0).to_i)

      i = TransitionsDataTimezoneInfo.new('Test/Zone', [t1,t2,t3,t4,t5])

      assert_equal([],               i.transitions_up_to(Timestamp.for(Time.utc(2010, 4,1,1,0,0))))
      assert_equal([],               i.transitions_up_to(Timestamp.for(Time.utc(2010, 4,1,1,0,0)), Timestamp.for(Time.utc(2000, 1,1,0,0,0))))
      assert_equal([t1],             i.transitions_up_to(Timestamp.for(Time.utc(2010, 4,1,1,0,1))))
      assert_equal([t1],             i.transitions_up_to(Timestamp.for(Time.utc(2010, 4,1,1,0,1)), Timestamp.for(Time.utc(2000, 1,1,0,0,0))))
      assert_equal([t2,t3,t4],       i.transitions_up_to(Timestamp.for(Time.utc(2011, 4,1,1,0,1)), Timestamp.for(Time.utc(2010,10,1,1,0,0))))
      assert_equal([t2,t3,t4],       i.transitions_up_to(Timestamp.for(Time.utc(2011,10,1,1,0,0)), Timestamp.for(Time.utc(2010, 4,1,1,0,1))))
      assert_equal([t3],             i.transitions_up_to(Timestamp.for(Time.utc(2011, 4,1,1,0,0)), Timestamp.for(Time.utc(2010,10,1,1,0,1))))
      assert_equal([],               i.transitions_up_to(Timestamp.for(Time.utc(2011, 3,1,1,0,0)), Timestamp.for(Time.utc(2010,10,1,1,0,1))))
      assert_equal([t1,t2,t3,t4],    i.transitions_up_to(Timestamp.for(Time.utc(2011,10,1,1,0,0))))
      assert_equal([t1,t2,t3,t4,t5], i.transitions_up_to(Timestamp.for(Time.utc(2011,10,1,1,0,1))))
      assert_equal([t1,t2,t3,t4,t5], i.transitions_up_to(Timestamp.for(Time.utc(2011,10,1,1,0,0,1))))
      assert_equal([t1,t2,t3,t4,t5], i.transitions_up_to(Timestamp.for(Time.utc(2011,10,1,1,0,1)), Timestamp.for(Time.utc(2010, 4,1,1,0,0))))
      assert_equal([t2,t3,t4,t5],    i.transitions_up_to(Timestamp.for(Time.utc(2011,10,1,1,0,1)), Timestamp.for(Time.utc(2010, 4,1,1,0,1))))
      assert_equal([t2,t3,t4,t5],    i.transitions_up_to(Timestamp.for(Time.utc(2011,10,1,1,0,1)), Timestamp.for(Time.utc(2010, 4,1,1,0,0,1))))
      assert_equal([t5],             i.transitions_up_to(Timestamp.for(Time.utc(2015, 1,1,0,0,0)), Timestamp.for(Time.utc(2011,10,1,1,0,0))))
      assert_equal([],               i.transitions_up_to(Timestamp.for(Time.utc(2015, 1,1,0,0,0)), Timestamp.for(Time.utc(2011,10,1,1,0,1))))
    end

    def test_transitions_up_to_timestamp_with_zero_utc_offset
      o1 = TimezoneOffset.new(-17900,    0, 'TESTLMT')
      o2 = TimezoneOffset.new(-18000,    0, 'TESTS')
      o3 = TimezoneOffset.new(-18000, 3600, 'TESTD')

      t1 = TimezoneTransition.new(o2, o1, Time.utc(2009,12,31,23,59,59).to_i)
      t2 = TimezoneTransition.new(o3, o2, Time.utc(2010, 7, 1, 0, 0, 0).to_i)

      i = TransitionsDataTimezoneInfo.new('Test/Zone', [t1,t2])

      assert_equal([t1,t2], i.transitions_up_to(Timestamp.for(Time.new(2010,7,1,0,0,1,0))))
      assert_equal([t1,t2], i.transitions_up_to(Timestamp.for(Time.new(2011,1,1,0,0,0,0)), Timestamp.for(Time.new(2009,12,31,23,59,59,0))))
    end

    def test_transitions_up_to_timestamp_with_non_zero_utc_offset
      o1 = TimezoneOffset.new(-17900,    0, 'TESTLMT')
      o2 = TimezoneOffset.new(-18000,    0, 'TESTS')
      o3 = TimezoneOffset.new(-18000, 3600, 'TESTD')

      t1 = TimezoneTransition.new(o2, o1, Time.utc(2009,12,31,23,59,59).to_i)
      t2 = TimezoneTransition.new(o3, o2, Time.utc(2010, 7, 1, 0, 0, 0).to_i)

      i = TransitionsDataTimezoneInfo.new('Test/Zone', [t1,t2])

      assert_equal([t1,t2], i.transitions_up_to(Timestamp.for(Time.new(2010,6,30,23,0,1,-3600))))
      assert_equal([t1],    i.transitions_up_to(Timestamp.for(Time.new(2010,7, 1, 1,0,0, 3600))))
      assert_equal([t1,t2], i.transitions_up_to(Timestamp.for(Time.new(2011,1,1,0,0,0,0)), Timestamp.for(Time.new(2010, 1, 1, 0,59,59, 3600))))
      assert_equal([t2],    i.transitions_up_to(Timestamp.for(Time.new(2011,1,1,0,0,0,0)), Timestamp.for(Time.new(2009,12,31,23, 0, 0,-3600))))
    end

    def test_transitions_up_to_to_not_greater_than_from
      i = create_basic_info

      to = Timestamp.for(Time.utc(2012,8,1,0,0,0))
      from = Timestamp.for(Time.utc(2013,8,1,0,0,0))

      error = assert_raises(ArgumentError) { i.transitions_up_to(to, from) }
      assert_equal('to_timestamp must be greater than from_timestamp', error.message)
    end

    def test_transitions_up_to_to_not_greater_than_from_subsecond
      i = create_basic_info

      to = Timestamp.for(Time.utc(2012,8,1,0,0,0))
      from = Timestamp.for(Time.utc(2012,8,1,0,0,0,1))

      error = assert_raises(ArgumentError) { i.transitions_up_to(to, from) }
      assert_equal('to_timestamp must be greater than from_timestamp', error.message)
    end

    def test_transitions_up_to_to_timestamp_with_unspecified_offset
      i = create_basic_info

      to = Timestamp.for(Time.utc(2015,1,1,0,0,0), :ignore)

      error = assert_raises(ArgumentError) { i.transitions_up_to(to) }
      assert_equal('to_timestamp must have a specified utc_offset', error.message)
    end

    def test_transitions_up_to_from_timestamp_with_unspecified_offset
      i = create_basic_info

      to = Timestamp.for(Time.utc(2015,1,1,0,0,0))
      from = Timestamp.for(Time.utc(2014,1,1,0,0,0), :ignore)

      error = assert_raises(ArgumentError) { i.transitions_up_to(to, from) }
      assert_equal('from_timestamp must have a specified utc_offset', error.message)
    end

    def test_transitions_up_to_to_timestamp_nil
      i = create_basic_info
      error = assert_raises(ArgumentError) { i.transitions_up_to(nil) }
      assert_equal('to_timestamp must be specified', error.message)
    end

    def test_inspect
      i = create_basic_info
      assert_equal('#<TZInfo::DataSources::TransitionsDataTimezoneInfo: Test/Zone>', i.inspect)
    end
  end
end
