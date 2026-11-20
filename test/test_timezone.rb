#
# Fluentd
#
#    Licensed under the Apache License, Version 2.0 (the "License");
#    you may not use this file except in compliance with the License.
#    You may obtain a copy of the License at
#
#        http://www.apache.org/licenses/LICENSE-2.0
#
#    Unless required by applicable law or agreed to in writing, software
#    distributed under the License is distributed on an "AS IS" BASIS,
#    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#    See the License for the specific language governing permissions and
#    limitations under the License.
#

require_relative 'helper'
require 'fluent/timezone'

class TimezoneTest < ::Test::Unit::TestCase
  # [+-]HH, [+-]HHMM and [+-]HH:MM with HH in 00..23 and MM in 00..59.
  VALID = [
    '+00', '-00', '+09', '-09', '+23', '-23',
    '+09:00', '-05:30', '+00:01', '+23:59', '-23:59', '+14:00', '-12:00',
    '+0900', '-0530', '+2359', '-2359',
  ]

  # An offset that is not a valid time of day. Either it carries into the hours
  # and silently means a different offset, or it leaves the range Time#localtime
  # can apply and fails on every record.
  INVALID = [
    '+09:60', '+0960', '+00:75', '-05:99', '+23:60',
    '+24:00', '-24:00', '+99:99', '-99:99', '+2400',
  ]

  def test_validate_accepts_offsets_within_a_day
    VALID.each do |timezone|
      assert_true Fluent::Timezone.validate(timezone), "#{timezone} should be valid"
    end
  end

  def test_validate_rejects_offsets_outside_a_day
    INVALID.each do |timezone|
      assert_false Fluent::Timezone.validate(timezone), "#{timezone} should be invalid"
      assert_raise(Fluent::ConfigError.new("Unsupported timezone '#{timezone}'")) do
        Fluent::Timezone.validate!(timezone)
      end
    end
  end

  def test_utc_offset_resolves_the_offsets_time_can_apply
    VALID.each do |timezone|
      offset = Fluent::Timezone.utc_offset(timezone)
      assert_true offset.is_a?(Integer), "#{timezone} does not resolve to an offset: #{offset.inspect}"
      assert_true offset.abs < 86400, "#{timezone} resolves out of range: #{offset}"
    end

    assert_equal 32400, Fluent::Timezone.utc_offset('+09:00')
    assert_equal 32400, Fluent::Timezone.utc_offset('+0900')
    assert_equal 32400, Fluent::Timezone.utc_offset('+09')
    assert_equal(-19800, Fluent::Timezone.utc_offset('-0530'))

    INVALID.each do |timezone|
      assert_nil Fluent::Timezone.utc_offset(timezone), "#{timezone} must not resolve to an offset"
    end
  end

  def test_formatter_builds_a_formatter_for_valid_offsets
    formatter = Fluent::Timezone.formatter('+09:00', '%Y-%m-%dT%H:%M:%S%z')
    assert_not_nil formatter
    assert_equal '1970-01-01T09:00:00+0900', formatter.call(0)

    # '+09:60' means ten o'clock UTC only because the minutes are carried, so it
    # must not be accepted as a stand-in for '+10:00'.
    assert_nil Fluent::Timezone.formatter('+09:60', '%Y-%m-%dT%H:%M:%S%z')
  end

  # America/New_York transition instants used below:
  #   fall-back:       2026-11-01 06:00 UTC (02:00 EDT -> 01:00 EST)
  #   spring-forward:  2026-03-08 07:00 UTC (02:00 EST -> 03:00 EDT)
  FALLBACK_UTC  = Time.utc(2026, 11, 1, 6, 0, 0).to_i
  SPRING_FWD_UTC = Time.utc(2026, 3, 8, 7, 0, 0).to_i

  def test_formatter_applies_the_offset_in_effect_at_each_instant
    formatter = Fluent::Timezone.formatter('America/New_York', '%Y-%m-%d %H:%M %z')
    assert_not_nil formatter

    # Run under a system timezone far from the named one. TZInfo 2 ignores the
    # UTC offset of the Time passed to period_for_utc, so a formatter that
    # looks up periods through the system local time applies DST transitions
    # at wrong instants.
    with_timezone('UTC-8') do
      # The local 01:00 hour happens twice around the fall-back.
      assert_equal '2026-11-01 01:00 -0400', formatter.call(FALLBACK_UTC - 3600)
      assert_equal '2026-11-01 01:00 -0500', formatter.call(FALLBACK_UTC)

      # The local 02:00 hour never happens around the spring-forward.
      assert_equal '2026-03-08 01:00 -0500', formatter.call(SPRING_FWD_UTC - 3600)
      assert_equal '2026-03-08 03:00 -0400', formatter.call(SPRING_FWD_UTC)
    end
  end

  def test_utc_offset_of_named_zone_follows_the_period_at_each_instant
    offset = Fluent::Timezone.utc_offset('America/New_York')
    assert_respond_to offset, :call

    with_timezone('UTC-8') do
      assert_equal(-14400, offset.call(FALLBACK_UTC - 1)) # EDT until 06:00 UTC
      assert_equal(-18000, offset.call(FALLBACK_UTC))     # EST from 06:00 UTC
      assert_equal(-18000, offset.call(SPRING_FWD_UTC - 1)) # EST until 07:00 UTC
      assert_equal(-14400, offset.call(SPRING_FWD_UTC))     # EDT from 07:00 UTC
    end
  end
end
