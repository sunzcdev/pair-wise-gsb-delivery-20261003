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

require_relative '../helper'
require 'fluent/plugin/output'
require 'fluent/plugin/buffer'
require 'fluent/event'

require 'time'

# Verifies that timekey based chunking and placeholder expansion behave
# deterministically around DST transitions of a named timekey_zone.
#
# America/New_York (system timezone is forced to UTC+8 to make sure the
# behavior does not depend on the timezone of the machine running tests):
#
#   fall-back:       2026-11-01 02:00 EDT -> 01:00 EST (06:00 UTC)
#                    The local 01:00 hour happens twice:
#                      01:00-02:00 EDT = 05:00-06:00 UTC
#                      01:00-02:00 EST = 06:00-07:00 UTC
#   spring-forward:  2026-03-08 02:00 EST -> 03:00 EDT (07:00 UTC)
#                    The local 02:00 hour never happens.
class OutputTimekeyDSTTest < Test::Unit::TestCase
  class DummyDSTOutput < Fluent::Plugin::Output
    def write(chunk)
      # do nothing: chunks are inspected through the memory buffer stage
    end
  end

  ZONE = 'America/New_York'

  # The repeated local 01:00 hour of the fall-back transition
  FALLBACK_FIRST  = Time.utc(2026, 11, 1, 5, 30, 0) # 01:30 EDT
  FALLBACK_SECOND = Time.utc(2026, 11, 1, 6, 30, 0) # 01:30 EST
  # The hours around the skipped local 02:00 hour of the spring-forward
  SPRING_BEFORE = Time.utc(2026, 3, 8, 6, 30, 0)    # 01:30 EST
  SPRING_AFTER  = Time.utc(2026, 3, 8, 7, 30, 0)    # 03:30 EDT

  setup do
    @i = nil
  end

  teardown do
    if @i
      @i.stop unless @i.stopped?
      @i.before_shutdown unless @i.before_shutdown?
      @i.shutdown unless @i.shutdown?
      @i.after_shutdown unless @i.after_shutdown?
      @i.close unless @i.closed?
      @i.terminate unless @i.terminated?
    end
  end

  def create_plugin(buffer_hash)
    i = DummyDSTOutput.new
    i.configure(config_element('ROOT', '', {}, [config_element('buffer', 'time', buffer_hash)]))
    i.start
    i.after_start
    i.thread_wait_until_start
    i.interrupt_flushes
    i
  end

  def emit(i, *times)
    times.each do |t|
      es = Fluent::ArrayEventStream.new([[Fluent::EventTime.from_time(t), {'message' => t.to_s}]])
      i.emit_events('test.dst', es)
    end
  end

  def staged_timekeys(i)
    i.buffer.stage.keys.map(&:timekey).sort
  end

  def staged_paths(i, template)
    i.buffer.stage.map { |metadata, chunk| i.extract_placeholders(template, chunk) }.sort
  end

  # UTC+8 is far enough from America/New_York to break every code path that
  # accidentally reads TZInfo periods through the system local time.
  def with_far_system_timezone(&block)
    with_timezone('UTC-8', &block)
  end

  sub_test_case 'fall-back: the local 01:00 hour happens twice' do
    test 'two instants of the repeated hour are assigned to distinct chunks' do
      with_far_system_timezone do
        @i = create_plugin('@type' => 'memory', 'timekey' => 3600, 'timekey_zone' => ZONE)
        emit(@i, FALLBACK_FIRST, FALLBACK_SECOND)

        assert_equal 2, @i.buffer.stage.size
        assert_equal [Time.utc(2026, 11, 1, 5, 0, 0).to_i, # 01:00 EDT
                      Time.utc(2026, 11, 1, 6, 0, 0).to_i], # 01:00 EST
                     staged_timekeys(@i)
      end
    end

    test 'placeholders carrying the UTC offset distinguish both chunks' do
      with_far_system_timezone do
        @i = create_plugin('@type' => 'memory', 'timekey' => 3600, 'timekey_zone' => ZONE)
        emit(@i, FALLBACK_FIRST, FALLBACK_SECOND)

        paths = staged_paths(@i, '/log/%Y%m%d/%H_%z.log')
        assert_equal ['/log/20261101/01_-0400.log', '/log/20261101/01_-0500.log'], paths
        assert_equal 2, paths.uniq.size
      end
    end

    test 'plain local-time placeholders cannot disambiguate the repeated hour' do
      with_far_system_timezone do
        @i = create_plugin('@type' => 'memory', 'timekey' => 3600, 'timekey_zone' => ZONE)
        emit(@i, FALLBACK_FIRST, FALLBACK_SECOND)

        # Both chunks legitimately start at local 01:00, so a template without
        # %z maps both of them to the same path. Use %z (or timekey_use_utc)
        # to keep the two segments apart.
        assert_equal ['/log/20261101/01.log', '/log/20261101/01.log'],
                     staged_paths(@i, '/log/%Y%m%d/%H.log')
      end
    end

    test 'utc mode separates the two instants with plain placeholders' do
      with_far_system_timezone do
        @i = create_plugin('@type' => 'memory', 'timekey' => 3600, 'timekey_use_utc' => true)
        emit(@i, FALLBACK_FIRST, FALLBACK_SECOND)

        assert_equal [Time.utc(2026, 11, 1, 5, 0, 0).to_i,
                      Time.utc(2026, 11, 1, 6, 0, 0).to_i],
                     staged_timekeys(@i)
        assert_equal ['/log/20261101/05.log', '/log/20261101/06.log'],
                     staged_paths(@i, '/log/%Y%m%d/%H.log')
      end
    end
  end

  sub_test_case 'spring-forward: the local 02:00 hour never happens' do
    test 'chunks and paths skip the non-existent hour deterministically' do
      with_far_system_timezone do
        @i = create_plugin('@type' => 'memory', 'timekey' => 3600, 'timekey_zone' => ZONE)
        emit(@i, SPRING_BEFORE, SPRING_AFTER)

        assert_equal [Time.utc(2026, 3, 8, 6, 0, 0).to_i, # 01:00 EST
                      Time.utc(2026, 3, 8, 7, 0, 0).to_i], # 03:00 EDT
                     staged_timekeys(@i)
        assert_equal ['/log/20260308/01_-0500.log', '/log/20260308/03_-0400.log'],
                     staged_paths(@i, '/log/%Y%m%d/%H_%z.log')
      end
    end

    test 'utc mode is unaffected by the jump' do
      with_far_system_timezone do
        @i = create_plugin('@type' => 'memory', 'timekey' => 3600, 'timekey_use_utc' => true)
        emit(@i, SPRING_BEFORE, SPRING_AFTER)

        assert_equal ['/log/20260308/06.log', '/log/20260308/07.log'],
                     staged_paths(@i, '/log/%Y%m%d/%H.log')
      end
    end
  end

  sub_test_case 'offset resolution at the event instant' do
    test 'daily timekey uses the offset in effect at each event, not at lookup time' do
      with_far_system_timezone do
        @i = create_plugin('@type' => 'memory', 'timekey' => 86400, 'timekey_zone' => ZONE)

        # 00:30 EDT on 2026-11-01, before the fall-back. The day boundary must
        # be computed with the DST offset (-0400), i.e. local midnight is
        # 04:00 UTC. Reading the TZInfo period through the system local time
        # (UTC+8) would see EST already and pick 05:00 UTC of the previous day.
        timekey = @i.calculate_timekey(Fluent::EventTime.from_time(Time.utc(2026, 11, 1, 4, 30, 0)))
        assert_equal Time.utc(2026, 11, 1, 4, 0, 0).to_i, timekey

        # 08:00 EDT on 2026-10-31: local midnight of that day is 04:00 UTC.
        timekey = @i.calculate_timekey(Fluent::EventTime.from_time(Time.utc(2026, 10, 31, 12, 0, 0)))
        assert_equal Time.utc(2026, 10, 31, 4, 0, 0).to_i, timekey
      end
    end
  end
end
