require_relative '../helper'
require 'fluent/plugin/output'
require 'fluent/plugin/buffer'
require 'fluent/event'

require 'json'
require 'time'
require 'timeout'
require 'timecop'

module FluentPluginOutputAsBufferedRetryTest
  class DummyBareOutput < Fluent::Plugin::Output
    def register(name, &block)
      instance_variable_set("@#{name}", block)
    end
  end
  class DummySyncOutput < DummyBareOutput
    def initialize
      super
      @process = nil
    end
    def process(tag, es)
      @process ? @process.call(tag, es) : nil
    end
  end
  class DummyFullFeatureOutput < DummyBareOutput
    def initialize
      super
      @prefer_buffered_processing = nil
      @prefer_delayed_commit = nil
      @process = nil
      @format = nil
      @write = nil
      @try_write = nil
    end
    def prefer_buffered_processing
      @prefer_buffered_processing ? @prefer_buffered_processing.call : false
    end
    def prefer_delayed_commit
      @prefer_delayed_commit ? @prefer_delayed_commit.call : false
    end
    def process(tag, es)
      @process ? @process.call(tag, es) : nil
    end
    def format(tag, time, record)
      @format ? @format.call(tag, time, record) : [tag, time, record].to_json
    end
    def write(chunk)
      @write ? @write.call(chunk) : nil
    end
    def try_write(chunk)
      @try_write ? @try_write.call(chunk) : nil
    end
  end
  class DummyFullFeatureOutput2 < DummyFullFeatureOutput
    def prefer_buffered_processing; true; end
    def prefer_delayed_commit; super; end
    def format(tag, time, record); super; end
    def write(chunk); super; end
    def try_write(chunk); super; end
  end
end

class BufferedOutputRetryTest < Test::Unit::TestCase
  def create_output(type=:full)
    case type
    when :bare then FluentPluginOutputAsBufferedRetryTest::DummyBareOutput.new
    when :sync then FluentPluginOutputAsBufferedRetryTest::DummySyncOutput.new
    when :full then FluentPluginOutputAsBufferedRetryTest::DummyFullFeatureOutput.new
    else
      raise ArgumentError, "unknown type: #{type}"
    end
  end
  def create_metadata(timekey: nil, tag: nil, variables: nil)
    Fluent::Plugin::Buffer::Metadata.new(timekey, tag, variables)
  end
  def waiting(seconds)
    begin
      Timeout.timeout(seconds) do
        yield
      end
    rescue Timeout::Error
      STDERR.print(*@i.log.out.logs)
      raise
    end
  end
  def dummy_event_stream
    Fluent::ArrayEventStream.new([
      [ event_time('2016-04-13 18:33:00'), {"name" => "moris", "age" => 36, "message" => "data1"} ],
      [ event_time('2016-04-13 18:33:13'), {"name" => "moris", "age" => 36, "message" => "data2"} ],
      [ event_time('2016-04-13 18:33:32'), {"name" => "moris", "age" => 36, "message" => "data3"} ],
    ])
  end
  def get_log_time(msg, logs)
    log_time = nil
    log = logs.find{|l| l.include?(msg) }
    if log && /^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} [-+]\d{4}) \[error\]/ =~ log
      log_time = Time.parse($1)
    end
    log_time
  end

  setup do
    @i = create_output
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
    Timecop.return
  end

  sub_test_case 'buffered output for retries with exponential backoff' do
    test 'exponential backoff is default strategy for retries' do
      chunk_key = 'tag'
      hash = {
        'flush_interval' => 1,
        'flush_thread_burst_interval' => 0.1,
        'retry_randomize' => false,
        'queued_chunks_limit_size' => 100
      }
      @i.configure(config_element('ROOT','',{},[config_element('buffer',chunk_key,hash)]))
      @i.register(:prefer_buffered_processing){ true }
      @i.start
      @i.after_start

      assert_equal :exponential_backoff, @i.buffer_config.retry_type
      assert_equal 1, @i.buffer_config.retry_wait
      assert_equal 2.0, @i.buffer_config.retry_exponential_backoff_base
      assert !@i.buffer_config.retry_randomize

      now = Time.parse('2016-04-13 18:17:00 -0700')
      Timecop.freeze( now )

      retry_state = @i.retry_state( @i.buffer_config.retry_randomize )
      retry_state.step
      assert_equal (1 * (2 ** 1)), (retry_state.next_time - now)
      retry_state.step
      assert_equal (1 * (2 ** 2)), (retry_state.next_time - now)
      retry_state.step
      assert_equal (1 * (2 ** 3)), (retry_state.next_time - now)
      retry_state.step
      assert_equal (1 * (2 ** 4)), (retry_state.next_time - now)
    end

    test 'does retries correctly when #write fails' do
      chunk_key = 'tag'
      hash = {
        'flush_interval' => 1,
        'flush_thread_burst_interval' => 0.1,
        'retry_randomize' => false,
        'retry_max_interval' => 60 * 60,
      }
      @i.configure(config_element('ROOT','',{},[config_element('buffer',chunk_key,hash)]))
      @i.register(:prefer_buffered_processing){ true }
      @i.register(:format){|tag,time,record| [tag,time.to_i,record].to_json + "\n" }
      @i.register(:write){|chunk| raise "yay, your #write must fail" }
      @i.start
      @i.after_start

      @i.interrupt_flushes

      now = Time.parse('2016-04-13 18:33:30 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.1", dummy_event_stream())

      now = Time.parse('2016-04-13 18:33:32 -0700')
      Timecop.freeze( now )

      @i.enqueue_thread_wait

      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until @i.write_count > 0 }

      assert{ @i.write_count > 0 }
      assert{ @i.num_errors > 0 }

      now = @i.next_flush_time
      Timecop.freeze( now )
      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until @i.write_count > 1 }

      assert{ @i.write_count > 1 }
      assert{ @i.num_errors > 1 }
    end

    test 'max retry interval is limited by retry_max_interval' do
      chunk_key = 'tag'
      hash = {
        'flush_interval' => 1,
        'flush_thread_burst_interval' => 0.1,
        'retry_randomize' => false,
        'retry_max_interval' => 60,
      }
      @i.configure(config_element('ROOT','',{},[config_element('buffer',chunk_key,hash)]))
      @i.register(:prefer_buffered_processing){ true }
      @i.register(:format){|tag,time,record| [tag,time.to_i,record].to_json + "\n" }
      @i.register(:write){|chunk| raise "yay, your #write must fail" }
      @i.start
      @i.after_start

      @i.interrupt_flushes

      now = Time.parse('2016-04-13 18:33:30 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.1", dummy_event_stream())

      now = Time.parse('2016-04-13 18:33:32 -0700')
      Timecop.freeze( now )

      @i.enqueue_thread_wait

      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until @i.write_count > 0 && @i.num_errors > 0 }

      assert{ @i.write_count > 0 }
      assert{ @i.num_errors > 0 }

      prev_write_count = @i.write_count
      prev_num_errors = @i.num_errors

      10.times do
        now = @i.next_flush_time
        Timecop.freeze( now )
        @i.flush_thread_wakeup
        waiting(4){ Thread.pass until @i.write_count > prev_write_count && @i.num_errors > prev_num_errors }

        assert{ @i.write_count > prev_write_count }
        assert{ @i.num_errors > prev_num_errors }

        prev_write_count = @i.write_count
        prev_num_errors = @i.num_errors
      end
      # exponential backoff interval: 1 * 2 ** 10 == 1024
      # but it should be limited by retry_max_interval=60
      assert_equal 60, (@i.next_flush_time - now)
    end

    test 'output plugin give retries up by retry_timeout, and clear queue in buffer' do
      written_tags = []

      chunk_key = 'tag'
      hash = {
        'flush_interval' => 1,
        'flush_thread_burst_interval' => 0.1,
        'retry_randomize' => false,
        'retry_timeout' => 3600,
        'queued_chunks_limit_size' => 100
      }
      @i.configure(config_element('ROOT','',{},[config_element('buffer',chunk_key,hash)]))
      @i.register(:prefer_buffered_processing){ true }
      @i.register(:format){|tag,time,record| [tag,time.to_i,record].to_json + "\n" }
      @i.register(:write){|chunk| written_tags << chunk.metadata.tag; raise "yay, your #write must fail" }
      @i.start
      @i.after_start

      @i.interrupt_flushes

      now = Time.parse('2016-04-13 18:33:30 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.1", dummy_event_stream())

      now = Time.parse('2016-04-13 18:33:31 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.2", dummy_event_stream())

      assert_equal 0, @i.write_count
      assert_equal 0, @i.num_errors

      @i.enqueue_thread_wait
      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until @i.write_count > 0 && @i.num_errors > 0 }

      assert{ @i.buffer.queue.size > 0 }
      assert{ @i.buffer.queue.first.metadata.tag == 'test.tag.1' }

      assert{ @i.write_count > 0 }
      assert{ @i.num_errors > 0 }

      prev_write_count = @i.write_count
      prev_num_errors = @i.num_errors

      first_failure = @i.retry.start

      15.times do |i| # large enough
        now = @i.next_flush_time
        # p({i: i, now: now, diff: (now - Time.now)})
        # * if loop count is 12:
        # {:i=>0, :now=>2016-04-13 18:33:32 -0700, :diff=>1.0}
        # {:i=>1, :now=>2016-04-13 18:33:34 -0700, :diff=>2.0}
        # {:i=>2, :now=>2016-04-13 18:33:38 -0700, :diff=>4.0}
        # {:i=>3, :now=>2016-04-13 18:33:46 -0700, :diff=>8.0}
        # {:i=>4, :now=>2016-04-13 18:34:02 -0700, :diff=>16.0}
        # {:i=>5, :now=>2016-04-13 18:34:34 -0700, :diff=>32.0}
        # {:i=>6, :now=>2016-04-13 18:35:38 -0700, :diff=>64.0}
        # {:i=>7, :now=>2016-04-13 18:37:46 -0700, :diff=>128.0}
        # {:i=>8, :now=>2016-04-13 18:42:02 -0700, :diff=>256.0}
        # {:i=>9, :now=>2016-04-13 18:50:34 -0700, :diff=>512.0}
        # {:i=>10, :now=>2016-04-13 19:07:38 -0700, :diff=>1024.0}
        # {:i=>11, :now=>2016-04-13 19:33:31 -0700, :diff=>1553.0} # clear_queue!

        Timecop.freeze( now )
        @i.enqueue_thread_wait
        @i.flush_thread_wakeup
        waiting(4){ Thread.pass until @i.write_count > prev_write_count && @i.num_errors > prev_num_errors }

        assert{ @i.write_count > prev_write_count }
        assert{ @i.num_errors > prev_num_errors }

        break if @i.buffer.queue.size == 0

        prev_write_count = @i.write_count
        prev_num_errors = @i.num_errors

        assert{ @i.buffer.queue.first.metadata.tag == 'test.tag.1' }
      end
      assert{ now >= first_failure + 3600 }

      assert{ @i.buffer.stage.size == 0 }
      assert{ written_tags.all?('test.tag.1') }

      @i.emit_events("test.tag.3", dummy_event_stream())

      logs = @i.log.out.logs
      assert{ logs.any?{|l| l.include?("[error]: Hit limit for retries. dropping all chunks in the buffer queue.") } }
    end

    test 'output plugin give retries up by retry_max_times, and clear queue in buffer' do
      written_tags = []

      chunk_key = 'tag'
      hash = {
        'flush_interval' => 1,
        'flush_thread_burst_interval' => 0.1,
        'retry_randomize' => false,
        'retry_max_times' => 10,
        'queued_chunks_limit_size' => 100
      }
      @i.configure(config_element('ROOT','',{},[config_element('buffer',chunk_key,hash)]))
      @i.register(:prefer_buffered_processing){ true }
      @i.register(:format){|tag,time,record| [tag,time.to_i,record].to_json + "\n" }
      @i.register(:write){|chunk| written_tags << chunk.metadata.tag; raise "yay, your #write must fail" }
      @i.start
      @i.after_start

      @i.interrupt_flushes

      now = Time.parse('2016-04-13 18:33:30 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.1", dummy_event_stream())

      now = Time.parse('2016-04-13 18:33:31 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.2", dummy_event_stream())

      assert_equal 0, @i.write_count
      assert_equal 0, @i.num_errors

      @i.enqueue_thread_wait
      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until @i.write_count > 0 && @i.num_errors > 0 }

      assert{ @i.buffer.queue.size > 0 }
      assert{ @i.buffer.queue.first.metadata.tag == 'test.tag.1' }

      assert{ @i.write_count > 0 }
      assert{ @i.num_errors > 0 }

      prev_write_count = @i.write_count
      prev_num_errors = @i.num_errors

      _first_failure = @i.retry.start

      chunks = @i.buffer.queue.dup

      20.times do |i| # large times enough
        now = @i.next_flush_time

        Timecop.freeze( now )
        @i.enqueue_thread_wait
        @i.flush_thread_wakeup
        waiting(4){ Thread.pass until @i.write_count > prev_write_count && @i.num_errors > prev_num_errors }

        assert{ @i.write_count > prev_write_count }
        assert{ @i.num_errors > prev_num_errors }

        break if @i.buffer.queue.size == 0

        prev_write_count = @i.write_count
        prev_num_errors = @i.num_errors

        assert{ @i.buffer.queue.first.metadata.tag == 'test.tag.1' }
      end
      assert{ @i.buffer.stage.size == 0 }
      assert{ written_tags.all?('test.tag.1') }

      @i.emit_events("test.tag.3", dummy_event_stream())

      logs = @i.log.out.logs
      assert{ logs.any?{|l| l.include?("[error]: Hit limit for retries. dropping all chunks in the buffer queue.") && l.include?("retry_times=10") } }

      assert{ @i.buffer.queue.size == 0 }
      assert{ @i.buffer.stage.size == 1 }
      assert{ chunks.all?{|c| c.empty? } }
    end

    test 'output plugin limits queued chunks via queued_chunks_limit_size' do
      chunk_key = 'tag'
      hash = {
        'flush_interval' => 1,
        'flush_thread_burst_interval' => 0.1,
        'retry_randomize' => false,
        'retry_max_times' => 7,
        'queued_chunks_limit_size' => 2,
      }
      @i.configure(config_element('ROOT','',{},[config_element('buffer',chunk_key,hash)]))
      @i.register(:prefer_buffered_processing) { true }
      @i.register(:format) { |tag,time,record| [tag,time.to_i,record].to_json + "\n" }
      @i.register(:write) { |chunk| raise "yay, your #write must fail" }
      @i.start
      @i.after_start

      @i.interrupt_flushes

      now = Time.parse('2016-04-13 18:33:30 -0700')
      Timecop.freeze(now)

      @i.emit_events("test.tag.1", dummy_event_stream())

      now = Time.parse('2016-04-13 18:33:31 -0700')
      Timecop.freeze(now)

      @i.emit_events("test.tag.2", dummy_event_stream())

      @i.enqueue_thread_wait
      @i.flush_thread_wakeup
      waiting(4) { Thread.pass until @i.write_count > 0 && @i.num_errors > 0 }

      assert { @i.buffer.queue.size > 0 }
      assert { @i.buffer.queue.first.metadata.tag == 'test.tag.1' }

      assert { @i.write_count > 0 }
      assert { @i.num_errors > 0 }

      prev_write_count = @i.write_count
      prev_num_errors = @i.num_errors

      20.times do |i| # large times enough
        now = @i.next_flush_time

        Timecop.freeze(now)
        @i.enqueue_thread_wait
        @i.flush_thread_wakeup
        waiting(4) { Thread.pass until @i.write_count > prev_write_count && @i.num_errors > prev_num_errors }

        @i.emit_events("test.tag.1", dummy_event_stream())
        assert { @i.buffer.queue.size <= 2 }
        assert { @i.buffer.stage.size == 1 } # all new data is stored into staged chunk

        break if @i.buffer.queue.size == 0

        prev_write_count = @i.write_count
        prev_num_errors = @i.num_errors
      end
    end
  end

  sub_test_case 'buffered output for retries with periodical retry' do
    test 'periodical retries should retry to write in failing status per retry_wait' do
      chunk_key = 'tag'
      hash = {
        'flush_interval' => 1,
        'flush_thread_burst_interval' => 0.1,
        'retry_type' => :periodic,
        'retry_wait' => 3,
        'retry_randomize' => false,
        'queued_chunks_limit_size' => 100
      }
      @i.configure(config_element('ROOT','',{},[config_element('buffer',chunk_key,hash)]))
      @i.register(:prefer_buffered_processing){ true }
      @i.register(:format){|tag,time,record| [tag,time.to_i,record].to_json + "\n" }
      @i.register(:write){|chunk| raise "yay, your #write must fail" }
      @i.start
      @i.after_start

      @i.interrupt_flushes

      now = Time.parse('2016-04-13 18:33:30 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.1", dummy_event_stream())

      now = Time.parse('2016-04-13 18:33:32 -0700')
      Timecop.freeze( now )

      @i.enqueue_thread_wait

      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until @i.write_count > 0 }

      assert{ @i.write_count > 0 }
      assert{ @i.num_errors > 0 }

      now = @i.next_flush_time
      Timecop.freeze( now )
      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until @i.write_count > 1 }

      assert{ @i.write_count > 1 }
      assert{ @i.num_errors > 1 }
    end

    test 'output plugin give retries up by retry_timeout, and clear queue in buffer' do
      written_tags = []

      chunk_key = 'tag'
      hash = {
        'flush_interval' => 1,
        'flush_thread_burst_interval' => 0.1,
        'retry_type' => :periodic,
        'retry_wait' => 30,
        'retry_randomize' => false,
        'retry_timeout' => 120,
        'queued_chunks_limit_size' => 100
      }
      @i.configure(config_element('ROOT','',{},[config_element('buffer',chunk_key,hash)]))
      @i.register(:prefer_buffered_processing){ true }
      @i.register(:format){|tag,time,record| [tag,time.to_i,record].to_json + "\n" }
      @i.register(:write){|chunk| written_tags << chunk.metadata.tag; raise "yay, your #write must fail" }
      @i.start
      @i.after_start

      @i.interrupt_flushes

      now = Time.parse('2016-04-13 18:33:30 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.1", dummy_event_stream())

      now = Time.parse('2016-04-13 18:33:31 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.2", dummy_event_stream())

      assert_equal 0, @i.write_count
      assert_equal 0, @i.num_errors

      @i.enqueue_thread_wait
      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until @i.write_count > 0 && @i.num_errors > 0 }

      assert{ @i.buffer.queue.size > 0 }
      assert{ @i.buffer.queue.first.metadata.tag == 'test.tag.1' }

      assert{ @i.write_count > 0 }
      assert{ @i.num_errors > 0 }

      prev_write_count = @i.write_count
      prev_num_errors = @i.num_errors

      first_failure = @i.retry.start

      3.times do |i|
        now = @i.next_flush_time

        Timecop.freeze( now )
        @i.enqueue_thread_wait
        @i.flush_thread_wakeup
        waiting(4){ Thread.pass until @i.write_count > prev_write_count && @i.num_errors > prev_num_errors }

        assert{ @i.write_count > prev_write_count }
        assert{ @i.num_errors > prev_num_errors }

        prev_write_count = @i.write_count
        prev_num_errors = @i.num_errors
      end

      assert{ @i.next_flush_time >= first_failure + 120 }

      assert{ @i.buffer.queue.size == 2 }
      assert{ @i.buffer.queue.first.metadata.tag == 'test.tag.1' }
      assert{ @i.buffer.stage.size == 0 }

      assert{ written_tags.all?('test.tag.1') }

      chunks = @i.buffer.queue.dup

      @i.emit_events("test.tag.3", dummy_event_stream())

      now = @i.next_flush_time
      Timecop.freeze( now )
      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until @i.write_count > prev_write_count && @i.num_errors > prev_num_errors }

      logs = @i.log.out.logs

      target_time = Time.parse("2016-04-13 18:35:31 -0700")
      target_msg = "[error]: Hit limit for retries. dropping all chunks in the buffer queue."
      assert{ logs.any?{|l| l.include?(target_msg) } }

      log_time = get_log_time(target_msg, logs)
      assert_equal target_time.localtime, log_time.localtime

      assert{ @i.buffer.queue.size == 0 }
      assert{ @i.buffer.stage.size == 1 }
      assert{ chunks.all?{|c| c.empty? } }
    end

    test 'retry_max_times can limit maximum times for retries' do
      written_tags = []

      chunk_key = 'tag'
      hash = {
        'flush_interval' => 1,
        'flush_thread_burst_interval' => 0.1,
        'retry_type' => :periodic,
        'retry_wait' => 3,
        'retry_randomize' => false,
        'retry_max_times' => 10,
        'queued_chunks_limit_size' => 100
      }
      @i.configure(config_element('ROOT','',{},[config_element('buffer',chunk_key,hash)]))
      @i.register(:prefer_buffered_processing){ true }
      @i.register(:format){|tag,time,record| [tag,time.to_i,record].to_json + "\n" }
      @i.register(:write){|chunk| written_tags << chunk.metadata.tag; raise "yay, your #write must fail" }
      @i.start
      @i.after_start

      @i.interrupt_flushes

      now = Time.parse('2016-04-13 18:33:30 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.1", dummy_event_stream())

      now = Time.parse('2016-04-13 18:33:31 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.2", dummy_event_stream())

      assert_equal 0, @i.write_count
      assert_equal 0, @i.num_errors

      @i.enqueue_thread_wait
      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until @i.write_count > 0 && @i.num_errors > 0 }

      assert{ @i.buffer.queue.size > 0 }
      assert{ @i.buffer.queue.first.metadata.tag == 'test.tag.1' }

      assert{ @i.write_count > 0 }
      assert{ @i.num_errors > 0 }

      prev_write_count = @i.write_count
      prev_num_errors = @i.num_errors

      _first_failure = @i.retry.start

      chunks = @i.buffer.queue.dup

      20.times do |i|
        now = @i.next_flush_time

        Timecop.freeze( now )
        @i.enqueue_thread_wait
        @i.flush_thread_wakeup
        waiting(4){ Thread.pass until @i.write_count > prev_write_count && @i.num_errors > prev_num_errors }

        assert{ @i.write_count > prev_write_count }
        assert{ @i.num_errors > prev_num_errors }

        break if @i.buffer.queue.size == 0

        prev_write_count = @i.write_count
        prev_num_errors = @i.num_errors

        assert{ @i.buffer.queue.first.metadata.tag == 'test.tag.1' }
      end
      assert{ @i.buffer.stage.size == 0 }
      assert{ written_tags.all?('test.tag.1') }


      @i.emit_events("test.tag.3", dummy_event_stream())

      logs = @i.log.out.logs
      assert{ logs.any?{|l| l.include?("[error]: Hit limit for retries. dropping all chunks in the buffer queue.") && l.include?("retry_times=10") } }

      assert{ @i.buffer.queue.size == 0 }
      assert{ @i.buffer.stage.size == 1 }
      assert{ chunks.all?{|c| c.empty? } }
    end

    test 'Do not retry when retry_max_times is 0' do
      written_tags = []

      chunk_key = 'tag'
      hash = {
        'flush_interval' => 1,
        'flush_thread_burst_interval' => 0.1,
        'retry_type' => :periodic,
        'retry_wait' => 1,
        'retry_randomize' => false,
        'retry_max_times' => 0,
        'queued_chunks_limit_size' => 100
      }
      @i.configure(config_element('ROOT','',{},[config_element('buffer',chunk_key,hash)]))
      @i.register(:prefer_buffered_processing){ true }
      @i.register(:format){|tag,time,record| [tag,time.to_i,record].to_json + "\n" }
      @i.register(:write){|chunk| written_tags << chunk.metadata.tag; raise "yay, your #write must fail" }
      @i.start
      @i.after_start

      @i.interrupt_flushes

      now = Time.parse('2016-04-13 18:33:30 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.1", dummy_event_stream())

      now = Time.parse('2016-04-13 18:33:31 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.2", dummy_event_stream())

      assert_equal(0, @i.write_count)
      assert_equal(0, @i.num_errors)

      @i.enqueue_thread_wait
      @i.flush_thread_wakeup
      waiting(2){ Thread.pass until @i.write_count == 1 && @i.num_errors == 1 }

      assert(@i.write_count == 1)
      assert(@i.num_errors == 1)
      assert(@i.log.out.logs.any?{|l| l.include?("[error]: Hit limit for retries. dropping all chunks in the buffer queue.") && l.include?("retry_times=0") })
      assert(@i.buffer.queue.size == 0)
      assert(@i.buffer.stage.size == 1)
      assert(@i.buffer.queue.all?{|c| c.empty? })
    end
  end

  sub_test_case 'buffered output configured as retry_forever' do
    setup do
      Fluent::Plugin.register_output('output_retries_secondary_test', FluentPluginOutputAsBufferedRetryTest::DummyFullFeatureOutput2)
    end

    test 'warning logs are generated if secondary section is configured' do
      chunk_key = 'tag'
      hash = {
        'retry_forever' => true,
        'retry_randomize' => false,
      }
      i = create_output()
      i.configure(config_element('ROOT','',{},[config_element('buffer',chunk_key,hash),config_element('secondary','', {'@type' => 'output_retries_secondary_test'})]))
      logs = i.log.out.logs
      assert { logs.any? { |l| l.include?("<secondary> with 'retry_forever', only unrecoverable errors are moved to secondary") } }
    end

    test 'retry_timeout and retry_max_times will be ignored if retry_forever is true for exponential backoff' do
      written_tags = []

      chunk_key = 'tag'
      hash = {
        'flush_interval' => 1,
        'flush_thread_burst_interval' => 0.1,
        'retry_type' => :exponential_backoff,
        'retry_forever' => true,
        'retry_randomize' => false,
        'retry_timeout' => 3600,
        'retry_max_times' => 10,
        'queued_chunks_limit_size' => 100
      }
      @i.configure(config_element('ROOT','',{},[config_element('buffer',chunk_key,hash)]))
      @i.register(:prefer_buffered_processing){ true }
      @i.register(:format){|tag,time,record| [tag,time.to_i,record].to_json + "\n" }
      @i.register(:write){|chunk| written_tags << chunk.metadata.tag; raise "yay, your #write must fail" }
      @i.start
      @i.after_start

      @i.interrupt_flushes

      now = Time.parse('2016-04-13 18:33:30 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.1", dummy_event_stream())

      now = Time.parse('2016-04-13 18:33:31 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.2", dummy_event_stream())

      assert_equal 0, @i.write_count
      assert_equal 0, @i.num_errors

      @i.enqueue_thread_wait
      @i.flush_thread_wakeup
      waiting(4){ sleep 0.1 until @i.write_count > 0 && @i.num_errors > 0 }

      assert{ @i.buffer.queue.size > 0 }
      assert{ @i.buffer.queue.first.metadata.tag == 'test.tag.1' }

      assert{ @i.write_count > 0 }
      assert{ @i.num_errors > 0 }

      prev_write_count = @i.write_count
      prev_num_errors = @i.num_errors

      first_failure = @i.retry.start

      15.times do |i|
        now = @i.next_flush_time

        Timecop.freeze( now + 1 )
        @i.enqueue_thread_wait
        @i.flush_thread_wakeup
        waiting(4){ sleep 0.1 until @i.write_count > prev_write_count && @i.num_errors > prev_num_errors }

        assert{ @i.write_count > prev_write_count }
        assert{ @i.num_errors > prev_num_errors }

        prev_write_count = @i.write_count
        prev_num_errors = @i.num_errors
      end

      assert{ @i.buffer.queue.size == 2 }
      assert{ @i.retry.steps > 10 }
      assert{ now > first_failure + 3600 }
    end

    test 'retry_timeout and retry_max_times will be ignored if retry_forever is true for periodical retries' do
      written_tags = []

      chunk_key = 'tag'
      hash = {
        'flush_interval' => 1,
        'flush_thread_burst_interval' => 0.1,
        'retry_type' => :periodic,
        'retry_forever' => true,
        'retry_randomize' => false,
        'retry_wait' => 30,
        'retry_timeout' => 360,
        'retry_max_times' => 10,
        'queued_chunks_limit_size' => 100
      }
      @i.configure(config_element('ROOT','',{},[config_element('buffer',chunk_key,hash)]))
      @i.register(:prefer_buffered_processing){ true }
      @i.register(:format){|tag,time,record| [tag,time.to_i,record].to_json + "\n" }
      @i.register(:write){|chunk| written_tags << chunk.metadata.tag; raise "yay, your #write must fail" }
      @i.start
      @i.after_start

      @i.interrupt_flushes

      now = Time.parse('2016-04-13 18:33:30 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.1", dummy_event_stream())

      now = Time.parse('2016-04-13 18:33:31 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.2", dummy_event_stream())

      assert_equal 0, @i.write_count
      assert_equal 0, @i.num_errors

      @i.enqueue_thread_wait
      @i.flush_thread_wakeup
      waiting(4){ sleep 0.1 until @i.write_count > 0 && @i.num_errors > 0 }

      assert{ @i.buffer.queue.size > 0 }
      assert{ @i.buffer.queue.first.metadata.tag == 'test.tag.1' }

      assert{ @i.write_count > 0 }
      assert{ @i.num_errors > 0 }

      prev_write_count = @i.write_count
      prev_num_errors = @i.num_errors

      first_failure = @i.retry.start

      15.times do |i|
        now = @i.next_flush_time

        Timecop.freeze( now + 1 )
        @i.enqueue_thread_wait
        @i.flush_thread_wakeup
        waiting(4){ sleep 0.1 until @i.write_count > prev_write_count && @i.num_errors > prev_num_errors }

        assert{ @i.write_count > prev_write_count }
        assert{ @i.num_errors > prev_num_errors }

        prev_write_count = @i.write_count
        prev_num_errors = @i.num_errors
      end

      assert{ @i.buffer.queue.size == 2 }
      assert{ @i.retry.steps > 10 }
      assert{ now > first_failure + 360 }
    end
  end

  sub_test_case 'buffered output with delayed commit' do
    test 'does retries correctly when #try_write fails' do
      chunk_key = 'tag'
      hash = {
        'flush_interval' => 1,
        'flush_thread_burst_interval' => 0.1,
        'retry_randomize' => false,
        'retry_max_interval' => 60 * 60,
      }
      @i.configure(config_element('ROOT','',{},[config_element('buffer',chunk_key,hash)]))
      @i.register(:prefer_buffered_processing){ true }
      @i.register(:prefer_delayed_commit){ true }
      @i.register(:format){|tag,time,record| [tag,time.to_i,record].to_json + "\n" }
      @i.register(:try_write){|chunk| raise "yay, your #write must fail" }
      @i.start
      @i.after_start

      @i.interrupt_flushes

      now = Time.parse('2016-04-13 18:33:30 -0700')
      Timecop.freeze( now )

      @i.emit_events("test.tag.1", dummy_event_stream())

      now = Time.parse('2016-04-13 18:33:32 -0700')
      Timecop.freeze( now )

      @i.enqueue_thread_wait

      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until @i.write_count > 0 && @i.num_errors > 0 }
      waiting(4) do
        state = @i.instance_variable_get(:@output_flush_threads).first
        state.thread.status == 'sleep'
      end

      assert(@i.write_count > 0)
      assert(@i.num_errors > 0)

      now = @i.next_flush_time
      Timecop.freeze( now )
      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until @i.write_count > 1 && @i.num_errors > 1 }
      waiting(4) do
        state = @i.instance_variable_get(:@output_flush_threads).first
        state.thread.status == 'sleep'
      end

      assert(@i.write_count > 1)
      assert(@i.num_errors > 1)
    end
  end

  sub_test_case 'buffered output retries with group isolation' do
    def event_stream_of(id)
      Fluent::ArrayEventStream.new([[event_time('2016-04-13 18:33:00'), {'id' => id}]])
    end

    def configure_grouped_output(hash, fail_times)
      attempts = Hash.new(0) # tag => number of write attempts
      arrivals = [] # [tag, id] of successfully written records, in arrival order

      @i.configure(config_element('ROOT','',{},[config_element('buffer','tag',hash)]))
      @i.register(:prefer_buffered_processing){ true }
      @i.register(:format){|tag,time,record| [tag,time.to_i,record].to_json + "\n" }
      @i.register(:write) do |chunk|
        tag = chunk.metadata.tag
        attempts[tag] += 1
        if fail_times[tag] && fail_times[tag] > 0
          fail_times[tag] -= 1
          raise "yay, your #write must fail"
        end
        chunk.read.split("\n").each do |line|
          _tag, _time, record = JSON.parse(line)
          arrivals << [tag, record['id']]
        end
      end
      @i.start
      @i.after_start

      return attempts, arrivals
    end

    test 'retry backoff is isolated per group, keeping the order of chunks in the failed group' do
      # the group of 'test.fail' fails explicitly twice, and recovers after that
      attempts, arrivals = configure_grouped_output(
        {
          'flush_mode' => 'immediate',
          'flush_thread_interval' => 0.1,
          'flush_thread_burst_interval' => 0.1,
          'retry_randomize' => false,
          'retry_wait' => 1,
          'retry_group_isolation' => true,
        },
        { 'test.fail' => 2 },
      )

      now = Time.parse('2016-04-13 18:33:30 -0700')
      Timecop.freeze( now )

      # two groups of on-memory data: the failing group (f1, f2) and the healthy group (g1, g2)
      @i.emit_events('test.fail', event_stream_of('f1'))
      @i.emit_events('test.fail', event_stream_of('f2'))
      @i.emit_events('test.ok', event_stream_of('g1'))
      @i.emit_events('test.ok', event_stream_of('g2'))

      # the first flush of the failing group fails explicitly (1st failure)
      waiting(4){ Thread.pass until attempts['test.fail'] >= 1 }

      # the healthy group arrives without waiting for the backoff of the failing group,
      # even though the retry time of the failing group (now + 1) has not come yet
      waiting(4){ Thread.pass until arrivals.size >= 2 }

      assert_equal [['test.ok', 'g1'], ['test.ok', 'g2']], arrivals.dup
      assert_equal 1, attempts['test.fail'] # the failing group is in backoff: not retried yet
      assert_nil @i.retry # the global retry state is not used with retry_group_isolation

      retry_states = @i.instance_variable_get(:@retry_states)
      assert_equal [['test.fail', nil]], retry_states.keys # only the failed group has a retry state

      # the 2nd explicit failure of the failing group, after its 1st backoff
      Timecop.freeze( now + 1.5 )
      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until attempts['test.fail'] >= 2 }

      assert_equal [['test.ok', 'g1'], ['test.ok', 'g2']], arrivals.dup

      # the failing group recovers after its 2nd backoff (retry_wait * 2 ** 1)
      Timecop.freeze( now + 4 )
      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until arrivals.size >= 4 }

      # the failed group arrives in its original order after recovery
      assert_equal [['test.ok', 'g1'], ['test.ok', 'g2'], ['test.fail', 'f1'], ['test.fail', 'f2']], arrivals.dup
      assert_equal 4, attempts['test.fail'] # f1: 2 failures + 1 success, f2: 1 success
      assert_equal [], @i.instance_variable_get(:@retry_states).keys # retry states are cleared on success
    end

    test 'retry backoff is shared by all groups without retry_group_isolation' do
      # the group of 'test.fail' fails explicitly once, and recovers after that
      attempts, arrivals = configure_grouped_output(
        {
          'flush_mode' => 'immediate',
          'flush_thread_interval' => 0.1,
          'flush_thread_burst_interval' => 0.1,
          'retry_randomize' => false,
          'retry_wait' => 1,
        },
        { 'test.fail' => 1 },
      )

      now = Time.parse('2016-04-13 18:33:30 -0700')
      Timecop.freeze( now )

      @i.emit_events('test.fail', event_stream_of('f1'))
      @i.emit_events('test.ok', event_stream_of('g1'))

      # the first flush of the failing group fails explicitly
      waiting(4){ Thread.pass until attempts['test.fail'] >= 1 }

      # the healthy group is also blocked by the shared retry backoff (existing behavior)
      sleep 0.5
      assert_equal [], arrivals.dup
      assert_not_nil @i.retry

      # after the shared backoff, the failed chunk is retried first and the healthy group follows
      Timecop.freeze( now + 1.5 )
      @i.flush_thread_wakeup
      waiting(4){ Thread.pass until arrivals.size >= 2 }

      assert_equal [['test.fail', 'f1'], ['test.ok', 'g1']], arrivals.dup
    end
  end
end
