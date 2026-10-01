# encoding: UTF-8
# frozen_string_literal: true

require_relative '../test_utils'
require 'tempfile'

module DataSources
  class TCZoneinfoWriter < Minitest::Test
    include TZInfo
    include TZInfo::DataSources

    # A fixed offset time zone (UTC+5:45, abbreviation XST) with no
    # transitions and no POSIX-style TZ string, written as a TZif version 2
    # file.
    FIXED_OFFSET_V2_BYTES = [
      '545a696632000000000000000000000000000000000000000000000000000000' \
      '000000000000000100000004000050dc000058535400545a6966320000000000' \
      '0000000000000000000000000000000000000000000000000000000000010000' \
      '0004000050dc0000585354000a0a'
    ].join.freeze

    # A time zone observing GMT/BST with explicit transitions on
    # 2037-03-29, 2037-10-25, 2040-03-25 and 2040-10-28 (the latter two
    # being outside of the range of the 32-bit section) and a POSIX-style TZ
    # string of GMT0BST,M3.5.0/1,M10.5.0, written as a TZif version 3 file.
    DST_TRANSITIONS_V3_BYTES = [
      '545a696633000000000000000000000000000000000000000000000000000000' \
      '0000000200000002000000087e79a4907f8e7f90010000000000000000000e10' \
      '0104474d540042535400545a6966330000000000000000000000000000000000' \
      '00000000000000000000000000040000000200000008000000007e79a4900000' \
      '00007f8e7f900000000084194a90000000008537601001000100000000000000' \
      '00000e100104474d5400425354000a474d54304253542c4d332e352e302f312c' \
      '4d31302e352e300a'
    ].join.freeze

    def setup
      @string_deduper = StringDeduper.new
      @writer = ZoneinfoWriter.new(PosixTimeZoneParser.new(@string_deduper))
    end

    def write(offsets, transitions, tz_string = nil, version = 2)
      Tempfile.open('tzinfo-test-zone') do |file|
        path = file.path
        file.close
        @writer.write(path, offsets, transitions, tz_string, version)
        File.binread(path)
      end
    end

    def read(bytes)
      Tempfile.open('tzinfo-test-zone') do |file|
        path = file.path
        file.close
        File.binwrite(path, bytes)
        reader = ZoneinfoReader.new(PosixTimeZoneParser.new(@string_deduper), @string_deduper)
        reader.read(path)
      end
    end

    def fixed_offset
      TimezoneOffset.new(20700, 0, 'XST')
    end

    def gmt_offset
      TimezoneOffset.new(0, 0, 'GMT')
    end

    def bst_offset
      TimezoneOffset.new(0, 3600, 'BST')
    end

    def dst_transitions
      gmt = gmt_offset
      bst = bst_offset
      [
        TimezoneTransition.new(bst, gmt, Time.utc(2037, 3, 29, 1).to_i),
        TimezoneTransition.new(gmt, bst, Time.utc(2037, 10, 25, 1).to_i),
        TimezoneTransition.new(bst, gmt, Time.utc(2040, 3, 25, 1).to_i),
        TimezoneTransition.new(gmt, bst, Time.utc(2040, 10, 28, 1).to_i)
      ]
    end

    def test_write_fixed_offset_no_transitions_matches_expected_bytes
      bytes = write([fixed_offset], [])
      assert_equal([FIXED_OFFSET_V2_BYTES].pack('H*'), bytes)
    end

    def test_write_fixed_offset_no_transitions_reads_back
      bytes = write([fixed_offset], [])
      assert_equal(fixed_offset, read(bytes))
    end

    def test_write_dst_transitions_beyond_2038_matches_expected_bytes
      bytes = write([gmt_offset, bst_offset], dst_transitions, 'GMT0BST,M3.5.0/1,M10.5.0', 3)
      assert_equal([DST_TRANSITIONS_V3_BYTES].pack('H*'), bytes)
    end

    def test_write_dst_transitions_beyond_2038_reads_back
      transitions = dst_transitions
      bytes = write([gmt_offset, bst_offset], transitions, 'GMT0BST,M3.5.0/1,M10.5.0', 3)
      read_back = read(bytes)

      # The reader generates additional transitions from the POSIX-style TZ
      # string. The explicitly written transitions must all be present.
      assert(read_back.length > transitions.length)
      assert_equal(transitions, read_back[0, transitions.length])
    end

    def test_write_dst_transitions_without_tz_string_reads_back
      transitions = dst_transitions
      bytes = write([gmt_offset, bst_offset], transitions)
      assert_equal(transitions, read(bytes))
    end

    def test_write_transition_before_1901_reads_back
      lmt = TimezoneOffset.new(-500, 0, 'LMT')
      gmt = gmt_offset
      transitions = [TimezoneTransition.new(gmt, lmt, Time.utc(1850, 1, 1).to_i)]

      bytes = write([lmt, gmt], transitions)
      assert_equal(transitions, read(bytes))

      # The transition is outside of the range of the 32-bit section, so the
      # 32-bit section must not contain any transitions.
      timecnt = bytes[32, 4].unpack('N')[0]
      assert_equal(0, timecnt)
    end

    def test_write_fixed_offset_with_constant_tz_string_reads_back
      bytes = write([fixed_offset], [], 'XST-5:45')
      assert_equal(fixed_offset, read(bytes))
    end

    def test_write_no_transitions_with_dst_tz_string
      bytes = write([gmt_offset, bst_offset], [], 'GMT0BST,M3.5.0/1,M10.5.0')
      read_back = read(bytes)

      # The reader generates transitions from the POSIX-style TZ string.
      assert_kind_of(Array, read_back)
      assert(read_back.length > 0)
      assert_equal(gmt_offset, read_back[0].previous_offset)
    end

    def test_write_version_2_and_3
      [2, 3].each do |version|
        bytes = write([fixed_offset], [], nil, version)
        assert_equal("TZif#{version}", bytes[0, 5])
        assert_equal("TZif#{version}", bytes[44 + 6 + 4, 5])
      end
    end

    def test_write_invalid_version
      [0, 1, 4].each do |version|
        assert_raises(ArgumentError) { write([fixed_offset], [], nil, version) }
      end
    end

    def test_write_no_offsets
      assert_raises(ArgumentError) { write([], []) }
    end

    def test_write_too_many_offsets
      offsets = (1..257).map {|i| TimezoneOffset.new(i, 0, "X#{i}") }
      assert_raises(ArgumentError) { write(offsets, []) }
    end

    def test_write_offset_outside_representable_range
      offset = TimezoneOffset.new(2147483648, 0, 'XST')
      assert_raises(ArgumentError) { write([offset], []) }
    end

    def test_write_abbreviation_containing_null
      offset = TimezoneOffset.new(0, 0, "XS\0T")
      assert_raises(ArgumentError) { write([offset], []) }
    end

    def test_write_unordered_transitions
      gmt = gmt_offset
      bst = bst_offset
      transitions = [
        TimezoneTransition.new(gmt, bst, Time.utc(2037, 10, 25, 1).to_i),
        TimezoneTransition.new(bst, gmt, Time.utc(2037, 3, 29, 1).to_i)
      ]

      assert_raises(ArgumentError) { write([gmt, bst], transitions) }
    end

    def test_write_transition_time_outside_representable_range
      gmt = gmt_offset
      bst = bst_offset
      transitions = [TimezoneTransition.new(bst, gmt, 0x8000000000000000)]

      assert_raises(ArgumentError) { write([gmt, bst], transitions) }
    end

    def test_write_transition_offset_not_included_in_offsets
      gmt = gmt_offset
      bst = bst_offset
      transitions = [TimezoneTransition.new(bst, gmt, Time.utc(2037, 3, 29, 1).to_i)]

      assert_raises(ArgumentError) { write([gmt], transitions) }
    end

    def test_write_initial_offset_not_included_in_offsets
      gmt = gmt_offset
      bst = bst_offset
      transitions = [TimezoneTransition.new(bst, gmt, Time.utc(2037, 3, 29, 1).to_i)]

      assert_raises(ArgumentError) { write([bst], transitions) }
    end

    def test_write_inconsistent_previous_offsets
      gmt = gmt_offset
      bst = bst_offset
      transitions = [
        TimezoneTransition.new(bst, gmt, Time.utc(2037, 3, 29, 1).to_i),
        TimezoneTransition.new(gmt, gmt, Time.utc(2037, 10, 25, 1).to_i)
      ]

      assert_raises(ArgumentError) { write([gmt, bst], transitions) }
    end

    def test_write_invalid_tz_string
      assert_raises(ArgumentError) { write([fixed_offset], [], 'not a tz string') }
    end

    def test_write_tz_string_containing_newline
      assert_raises(ArgumentError) { write([fixed_offset], [], "XST-5:45\nEST5") }
    end

    def test_write_constant_tz_string_contradicting_offsets
      assert_raises(ArgumentError) { write([fixed_offset], [], 'EST5') }
    end

    def test_write_tz_string_contradicting_last_transition
      transitions = dst_transitions

      # The last transition is to GMT, but the TZ string says a constant BST
      # is observed.
      assert_raises(ArgumentError) do
        write([gmt_offset, bst_offset], transitions, 'BST-1')
      end
    end

    def test_write_tz_string_contradicting_last_transition_abbreviation
      transitions = dst_transitions

      # The last transition is to GMT, but the TZ string ends up in EDT.
      assert_raises(ArgumentError) do
        write([gmt_offset, bst_offset], transitions, 'EST5EDT,M3.2.0/2,M11.1.0/2')
      end
    end

    def test_write_version_3_tz_string_using_extended_hours
      transitions = dst_transitions

      # Version 2 permits transition times outside of the POSIX 0 to 24 hour
      # range. Version 3 does not.
      write([gmt_offset, bst_offset], transitions, 'GMT0BST,M3.5.0/26,M10.5.0', 2)

      assert_raises(ArgumentError) do
        write([gmt_offset, bst_offset], transitions, 'GMT0BST,M3.5.0/26,M10.5.0', 3)
      end
    end
  end
end
