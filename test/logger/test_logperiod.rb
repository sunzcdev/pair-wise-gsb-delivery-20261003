# coding: US-ASCII
# frozen_string_literal: false
require "logger"
require "time"

class TestLogPeriod < Test::Unit::TestCase
  def test_next_rotate_time
    time = Time.parse("2019-07-18 13:52:02")

    assert_next_rotate_time_words(time, "2019-07-19 00:00:00", ["daily", :daily])
    assert_next_rotate_time_words(time, "2019-07-21 00:00:00", ["weekly", :weekly])
    assert_next_rotate_time_words(time, "2019-08-01 00:00:00", ["monthly", :monthly])

    assert_raise(ArgumentError) { Logger::Period.next_rotate_time(time, "invalid") }
  end

  def test_next_rotate_time_extreme_cases
    # First day of Month and Saturday
    time = Time.parse("2018-07-01 00:00:00")

    assert_next_rotate_time_words(time, "2018-07-02 00:00:00", ["daily", :daily])
    assert_next_rotate_time_words(time, "2018-07-08 00:00:00", ["weekly", :weekly])
    assert_next_rotate_time_words(time, "2018-08-01 00:00:00", ["monthly", :monthly])

    assert_raise(ArgumentError) { Logger::Period.next_rotate_time(time, "invalid") }
  end

  def test_previous_period_end
    time = Time.parse("2019-07-18 13:52:02")

    assert_previous_period_end_words(time, "2019-07-17 23:59:59", ["daily", :daily])
    assert_previous_period_end_words(time, "2019-07-13 23:59:59", ["weekly", :weekly])
    assert_previous_period_end_words(time, "2019-06-30 23:59:59", ["monthly", :monthly])

    assert_raise(ArgumentError) { Logger::Period.previous_period_end(time, "invalid") }
  end

  def test_previous_period_end_extreme_cases
    # First day of Month and Saturday
    time = Time.parse("2018-07-01 00:00:00")
    previous_date = "2018-06-30 23:59:59"

    assert_previous_period_end_words(time, previous_date, ["daily", :daily])
    assert_previous_period_end_words(time, previous_date, ["weekly", :weekly])
    assert_previous_period_end_words(time, previous_date, ["monthly", :monthly])

    assert_raise(ArgumentError) { Logger::Period.previous_period_end(time, "invalid") }
  end

  def test_next_rotate_time_with_zone
    # 2019-07-19 00:30 in +09:00; boundaries must follow the explicit
    # zone, not the host's local time zone.
    time = Time.utc(2019, 7, 18, 15, 30, 0)

    assert_next_rotate_time_words_with_zone(time, "+09:00", Time.utc(2019, 7, 19, 15, 0, 0), ["daily", :daily])
    assert_next_rotate_time_words_with_zone(time, "+09:00", Time.utc(2019, 7, 20, 15, 0, 0), ["weekly", :weekly])
    assert_next_rotate_time_words_with_zone(time, "+09:00", Time.utc(2019, 7, 31, 15, 0, 0), ["monthly", :monthly])

    # Integer offset (seconds) is accepted as well.
    assert_equal(Time.utc(2019, 7, 19, 15, 0, 0),
                 Logger::Period.next_rotate_time(time, "daily", 9 * 3600))

    # 2019-07-18 10:30 in -05:00.
    assert_next_rotate_time_words_with_zone(time, "-05:00", Time.utc(2019, 7, 19, 5, 0, 0), ["daily", :daily])
    assert_next_rotate_time_words_with_zone(time, "-05:00", Time.utc(2019, 7, 21, 5, 0, 0), ["weekly", :weekly])

    assert_next_rotate_time_words_with_zone(time, "UTC", Time.utc(2019, 7, 19, 0, 0, 0), ["daily", :daily])
    assert_next_rotate_time_words_with_zone(time, "UTC", Time.utc(2019, 8, 1, 0, 0, 0), ["monthly", :monthly])

    assert_raise(ArgumentError) { Logger::Period.next_rotate_time(time, "invalid", "+09:00") }
  end

  def test_next_rotate_time_with_zone_boundary
    # Exactly midnight in the zone: the next boundary is a full period away.
    time = Time.utc(2019, 7, 19, 15, 0, 0) # 2019-07-20 00:00:00 in +09:00

    assert_next_rotate_time_words_with_zone(time, "+09:00", Time.utc(2019, 7, 20, 15, 0, 0), ["daily", :daily])
  end

  def test_previous_period_end_with_zone
    # 2019-07-19 00:30 in +09:00.
    time = Time.utc(2019, 7, 18, 15, 30, 0)

    assert_previous_period_end_words_with_zone(time, "+09:00", Time.utc(2019, 7, 18, 14, 59, 59), ["daily", :daily])
    assert_previous_period_end_words_with_zone(time, "+09:00", Time.utc(2019, 7, 13, 14, 59, 59), ["weekly", :weekly])
    assert_previous_period_end_words_with_zone(time, "+09:00", Time.utc(2019, 6, 30, 14, 59, 59), ["monthly", :monthly])

    # 2019-07-18 10:30 in -05:00.
    assert_previous_period_end_words_with_zone(time, "-05:00", Time.utc(2019, 7, 18, 4, 59, 59), ["daily", :daily])
    assert_previous_period_end_words_with_zone(time, "-05:00", Time.utc(2019, 7, 14, 4, 59, 59), ["weekly", :weekly])

    assert_previous_period_end_words_with_zone(time, "UTC", Time.utc(2019, 7, 17, 23, 59, 59), ["daily", :daily])
    assert_previous_period_end_words_with_zone(time, "UTC", Time.utc(2019, 6, 30, 23, 59, 59), ["monthly", :monthly])

    assert_raise(ArgumentError) { Logger::Period.previous_period_end(time, "invalid", "+09:00") }
  end

  def test_period_end_suffix_uses_zone_calendar
    # The returned time carries the zone offset, so a filename suffix
    # computed from it follows the same calendar as the boundary.
    time = Time.utc(2019, 7, 18, 15, 30, 0) # 2019-07-19 00:30 in +09:00

    period_end = Logger::Period.previous_period_end(time, "daily", "+09:00")
    assert_equal(9 * 3600, period_end.utc_offset)
    assert_equal("20190718", period_end.strftime("%Y%m%d"))

    next_time = Logger::Period.next_rotate_time(time, "daily", "+09:00")
    assert_equal(9 * 3600, next_time.utc_offset)
    assert_equal("20190720", next_time.strftime("%Y%m%d"))
  end

  private

  def assert_next_rotate_time_words(time, next_date, words)
    assert_time_words(:next_rotate_time, time, next_date, words)
  end

  def assert_previous_period_end_words(time, previous_date, words)
    assert_time_words(:previous_period_end, time, previous_date, words)
  end

  def assert_next_rotate_time_words_with_zone(time, zone, next_time, words)
    assert_time_words_with_zone(:next_rotate_time, time, zone, next_time, words)
  end

  def assert_previous_period_end_words_with_zone(time, zone, previous_time, words)
    assert_time_words_with_zone(:previous_period_end, time, zone, previous_time, words)
  end

  def assert_time_words_with_zone(method, time, zone, expected, words)
    words.each do |word|
      result = Logger::Period.public_send(method, time, word, zone)
      assert_equal(expected, result, "#{method}(#{time.inspect}, #{word.inspect}, #{zone.inspect})")
    end
  end

  def assert_time_words(method, time, date, words)
    words.each do |word|
      daily_result = Logger::Period.public_send(method, time, word)
      expected_result = Time.parse(date)
      assert_equal(expected_result, daily_result)
    end
  end
end
