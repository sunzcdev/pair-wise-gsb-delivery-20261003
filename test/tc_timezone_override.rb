# encoding: UTF-8
# frozen_string_literal: true

require_relative 'test_utils'

class TCTimezoneOverride < Minitest::Test
  include TZInfo

  def test_initialize
    offset = TimezoneOffset.new(-18000, 3600, 'XDT')
    override = TimezoneOverride.new(Time.utc(2024, 3, 1), Time.utc(2024, 12, 15), offset)

    assert_equal(Time.utc(2024, 3, 1).to_i, override.start_value)
    assert_equal(Time.utc(2024, 12, 15).to_i, override.end_value)
    assert_same(offset, override.offset)
  end

  def test_initialize_accepts_timestamp_and_datetime
    override = TimezoneOffset.new(0, 0, 'ZZZ')
    o = TimezoneOverride.new(Timestamp.utc(100), DateTime.new(2024, 1, 1, 0, 0, 0), override)
    assert_equal(100, o.start_value)
    assert_equal(Time.utc(2024).to_i, o.end_value)
  end

  def test_initialize_takes_offset_of_local_time_into_account
    offset = TimezoneOffset.new(0, 0, 'ZZZ')
    o = TimezoneOverride.new(Time.new(2024, 1, 1, 5, 0, 0, '+05:00'), Time.utc(2024, 2, 1), offset)
    assert_equal(Time.utc(2024).to_i, o.start_value)
  end

  def test_initialize_rejects_nil_arguments
    offset = TimezoneOffset.new(0, 0, 'ZZZ')

    error = assert_raises(ArgumentError) { TimezoneOverride.new(nil, Time.utc(2024), offset) }
    assert_match(/\bstart_at\b/, error.message)

    error = assert_raises(ArgumentError) { TimezoneOverride.new(Time.utc(2024), nil, offset) }
    assert_match(/\bend_at\b/, error.message)

    error = assert_raises(ArgumentError) { TimezoneOverride.new(Time.utc(2024), Time.utc(2024, 2), nil) }
    assert_match(/\boffset\b/, error.message)
  end

  def test_initialize_rejects_non_offset
    error = assert_raises(ArgumentError) do
      TimezoneOverride.new(Time.utc(2024), Time.utc(2024, 2), Object.new)
    end
    assert_match(/\bTimezoneOffset\b/, error.message)
  end

  def test_initialize_rejects_sub_second_boundaries
    offset = TimezoneOffset.new(0, 0, 'ZZZ')

    assert_raises(ArgumentError) do
      TimezoneOverride.new(Time.at(Time.utc(2024).to_i, Rational(1, 2), :utc), Time.utc(2024, 2), offset)
    end

    assert_raises(ArgumentError) do
      TimezoneOverride.new(Time.utc(2024), Time.at(Time.utc(2024, 2).to_i, Rational(1, 2), :utc), offset)
    end
  end

  def test_initialize_rejects_empty_window
    offset = TimezoneOffset.new(0, 0, 'ZZZ')
    error = assert_raises(ArgumentError) do
      TimezoneOverride.new(Time.utc(2024, 3, 2), Time.utc(2024, 3, 2), offset)
    end
    assert_match(/\blater than\b/, error.message)
  end

  def test_initialize_rejects_inverted_window
    offset = TimezoneOffset.new(0, 0, 'ZZZ')
    assert_raises(ArgumentError) do
      TimezoneOverride.new(Time.utc(2024, 4), Time.utc(2024, 3), offset)
    end
  end

  def test_equality
    offset = TimezoneOffset.new(-18000, 3600, 'XDT')
    o1 = TimezoneOverride.new(Time.utc(2024, 3), Time.utc(2024, 4), offset)
    o2 = TimezoneOverride.new(Time.utc(2024, 3), Time.utc(2024, 4), TimezoneOffset.new(-18000, 3600, 'XDT'))
    o3 = TimezoneOverride.new(Time.utc(2024, 3), Time.utc(2024, 5), offset)

    assert_equal(o1, o2)
    assert(o1.eql?(o2))
    assert_equal(o1.hash, o2.hash)
    refute_equal(o1, o3)
    refute_equal(o1, Object.new)
  end
end
