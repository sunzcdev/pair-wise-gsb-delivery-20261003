require_relative 'helper'
require 'fluent/event_router'
require 'fluent/system_config'
require 'fluent/root_agent'
require_relative 'test_plugin_classes'

# Tests for the retry-safe handoff when leaving with-source-only mode.
#
# The temporary file buffer is the resumable continuation point:
#   * while in source-only mode, inputs stage events into local buffer files;
#   * a failed switch must leave that backlog (and the source-only mode)
#     intact so the switch can be retried;
#   * on retry, already completed phases are skipped, old buffered events are
#     flushed before newly arriving live events, and chunks that were already
#     delivered are purged and never sent again.
class SourceOnlyHandoffTest < ::Test::Unit::TestCase
  include Fluent

  # Manual input: the test decides exactly when each event is emitted.
  class HandoffTestInput < Fluent::Plugin::Input
    Fluent::Plugin.register_input('handoff_test_in', self)

    attr_reader :started

    def initialize
      super
      @emit_records_metrics = FluentTest::FluentTestCounterMetrics.new
      @emit_size_metrics = FluentTest::FluentTestCounterMetrics.new
    end

    def start
      super
      @started = true
    end

    def multi_workers_ready?
      true
    end

    def emit_one(batch, number)
      router.emit('handoff.event', Fluent::EventTime.now, {'batch' => batch, 'num' => number})
    end
  end

  # Buffered output whose writes can be forced to fail. When `write` raises,
  # the output framework takes the dequeued chunk back onto disk and retries
  # it later, which models a handoff that fails halfway and then recovers.
  class HandoffTestOutput < Fluent::Plugin::Output
    Fluent::Plugin.register_output('handoff_test_out', self)

    attr_reader :events, :write_attempts

    def initialize
      super
      @events = Hash.new { |h, k| h[k] = [] }
      @events_mutex = Mutex.new
      @write_attempts = 0
      @fail_next_writes = 0
      @fail_mutex = Mutex.new
      @num_errors_metrics = FluentTest::FluentTestCounterMetrics.new
      @emit_count_metrics = FluentTest::FluentTestCounterMetrics.new
      @emit_records_metrics = FluentTest::FluentTestCounterMetrics.new
      @emit_size_metrics = FluentTest::FluentTestCounterMetrics.new
      @write_count_metrics = FluentTest::FluentTestCounterMetrics.new
      @write_secondary_count_metrics = FluentTest::FluentTestCounterMetrics.new
      @rollback_count_metrics = FluentTest::FluentTestCounterMetrics.new
      @flush_time_count_metrics = FluentTest::FluentTestCounterMetrics.new
      @slow_flush_count_metrics = FluentTest::FluentTestCounterMetrics.new
    end

    def prefer_buffered_processing
      true
    end

    def format(tag, time, record)
      [time, record].to_msgpack
    end

    def formatted_to_msgpack_binary?
      true
    end

    # Make the next +count+ chunk writes raise.
    def fail_next_writes!(count)
      @fail_mutex.synchronize { @fail_next_writes = count }
    end

    def write(chunk)
      @fail_mutex.synchronize do
        @write_attempts += 1
        if @fail_next_writes > 0
          @fail_next_writes -= 1
          raise "injected handoff write failure (attempt #{@write_attempts})"
        end
      end

      received = []
      chunk.each { |_time, record| received << record }
      @events_mutex.synchronize { @events[chunk.metadata.tag].concat(received) }
    end
  end

  def conf
    <<~EOC
      <source>
        @type handoff_test_in
        @id handoff_test_in
      </source>

      <match handoff.**>
        @type handoff_test_out
        @id handoff_test_out
        <buffer tag>
          @type file
          flush_mode interval
          flush_interval 0.2
          retry_wait 0.2
          retry_max_interval 0.5
        </buffer>
      </match>
    EOC
  end

  def setup
    Fluent::Test.setup
    @tmp_dir = File.join(File.dirname(__FILE__), 'tmp', "source_only_handoff_#{Process.pid}_#{object_id}")
    @tmp_root_dir = File.join(@tmp_dir, 'root')
    FileUtils.mkdir_p(@tmp_root_dir)

    @system_config = SystemConfig.new(
      Config::Element.new('system', '', {
        'with_source_only' => true,
        'root_dir' => @tmp_root_dir,
      }, [
        Config::Element.new('source_only_buffer', '', {
          'flush_interval' => '0.2s',
        }, []),
      ])
    )
    @root_agent = RootAgent.new(log: $log, system_config: @system_config)
    stub(Engine).root_agent { @root_agent }
    stub(Engine).system_config { @system_config }
    @root_agent.configure(Config.parse(conf, '(test)', '(test_dir)'))

    @buffer_dir = @root_agent.instance_variable_get(:@source_only_buffer_agent)
                                   .instance_variable_get(:@base_buffer_dir)
  end

  def teardown
    @root_agent&.shutdown
  ensure
    FileUtils.rm_rf(@tmp_dir)
  end

  def input
    @root_agent.inputs.first
  end

  def output
    @root_agent.outputs.first
  end

  def buffer_agent
    @root_agent.instance_variable_get(:@source_only_buffer_agent)
  end

  def mode
    @root_agent.instance_variable_get(:@source_only_mode)
  end

  def buffer_files
    return [] unless Dir.exist?(@buffer_dir)
    Dir.glob(File.join(@buffer_dir, '**', '*')).select { |p| File.file?(p) }
  end

  def wait_output_records(expected_size, timeout: 20)
    waiting(timeout) do
      sleep 0.1 until output.events['handoff.event']&.size.to_i >= expected_size
    end
  end

  def wait_buffer_files_empty(timeout: 20)
    waiting(timeout) do
      sleep 0.1 until buffer_files.empty?
    end
  end

  # Make the *first* flushing SourceOnlyBufferAgent fail while starting. The
  # hook runs when the buffer output inside the flushing agent is starting;
  # the original buffering agent is still alive at that point.
  def fail_first_loading_agent_start!
    original_start = Fluent::Plugin::Output.instance_method(:start)
    attempted = 0
    Fluent::Plugin::Output.send(:define_method, :start) do |*args, &block|
      if is_a?(Fluent::Plugin::BufferOutput)
        attempted += 1
        raise 'injected loading agent start failure' if attempted == 1
      end
      original_start.bind(self).call(*args, &block)
    end
    -> { Fluent::Plugin::Output.send(:define_method, :start, original_start) }
  end

  test 'starts in source-only mode: inputs stage into local buffer files and the real output stays stopped' do
    @root_agent.start

    assert { input.started? }
    assert { !output.started? }
    assert { @root_agent.filters.none?(&:started?) }

    input.emit_one('old', 0)
    input.emit_one('old', 1)

    waiting(5) { sleep 0.1 until buffer_files.any? }
    assert { output.events['handoff.event'].nil? || output.events['handoff.event'].empty? }
    assert { buffer_files.size >= 1 }
  end

  test 'a failed first switch preserves the staging buffer and a retry delivers the old batch then the new batch' do
    @root_agent.start

    # Old batch: buffered while in source-only mode.
    old_size = 5
    old_size.times { |i| input.emit_one('old', i) }
    waiting(5) { sleep 0.1 until buffer_files.any? }
    assert { buffer_files.any? }

    restore = fail_first_loading_agent_start!
    begin
      raised = assert_raise(RuntimeError) { @root_agent.cancel_source_only! }
      assert_match(/injected loading agent start failure/, raised.message)
    ensure
      restore.call
    end

    # Retry-safe continuation point: the mode is still active, the completed
    # phases (real plugins started, inputs switched) are recorded and the
    # backlog files are still on disk.
    assert { mode.enabled? }
    assert { @root_agent.instance_variable_get(:@source_only_real_plugins_started) }
    assert { @root_agent.instance_variable_get(:@source_only_inputs_switched) }
    assert { buffer_files.any? }
    # The original buffering agent is still the owner of the backlog.
    assert { buffer_agent.outputs.first.buffer_config.flush_thread_count.zero? }

    # New batch: after the failed switch, live inputs keep emitting into the
    # normal router. First let the retried handoff drain the old backlog, so
    # the ordering "old buffered batch, then new live batch" is deterministic.
    new_size = 3
    @root_agent.cancel_source_only!
    assert { !mode.enabled? }
    wait_output_records(old_size)
    assert_equal (['old'] * old_size), output.events['handoff.event'].map { |r| r['batch'] }

    new_size.times { |i| input.emit_one('new', i) }

    total = old_size + new_size
    wait_output_records(total)
    wait_buffer_files_empty

    records = output.events['handoff.event']
    batches = records.map { |r| r['batch'] }
    nums = records.map { |r| r['num'] }

    # Every event is delivered exactly once.
    assert_equal total, records.size
    assert_equal (0...old_size).to_a, records.select { |r| r['batch'] == 'old' }.map { |r| r['num'] }.sort
    assert_equal (0...new_size).to_a, records.select { |r| r['batch'] == 'new' }.map { |r| r['num'] }.sort

    # Explainable ordering: the whole old buffered batch is handed off before
    # the new live batch is admitted to the output.
    assert_equal (['old'] * old_size) + (['new'] * new_size), batches
    assert_equal (0...old_size).to_a + (0...new_size).to_a, nums
  end

  test 'a configuration failure while building the loading agent keeps the backlog and is recoverable on retry' do
    @root_agent.start

    input.emit_one('old', 0)
    input.emit_one('old', 1)
    waiting(5) { sleep 0.1 until buffer_files.any? }
    files_before = buffer_files.sort

    original_new = Fluent::SourceOnlyBufferAgent.method(:new)
    Fluent::SourceOnlyBufferAgent.define_singleton_method(:new) do |**kwargs|
      agent = original_new.call(**kwargs)
      def agent.configure(flush: false)
        raise Fluent::ConfigError, 'injected configuration failure'
      end
      agent
    end

    begin
      assert_raise(Fluent::ConfigError) { @root_agent.cancel_source_only! }
    ensure
      Fluent::SourceOnlyBufferAgent.define_singleton_method(:new, original_new)
    end

    # Staging is preserved byte-for-byte and the mode stays retryable.
    assert { mode.enabled? }
    assert_equal files_before, buffer_files.sort
    assert { @root_agent.instance_variable_get(:@source_only_real_plugins_started) }
    assert { @root_agent.instance_variable_get(:@source_only_inputs_switched) }

    # After configuration recovers, a plain retry flushes the backlog.
    @root_agent.cancel_source_only!
    wait_output_records(2)
    wait_buffer_files_empty

    records = output.events['handoff.event']
    assert_equal [0, 1], records.map { |r| r['num'] }.sort
    assert_equal ['old', 'old'], records.map { |r| r['batch'] }
  end

  test 'a mid-flush write failure rolls chunks back and a retry never re-sends delivered events' do
    @root_agent.start

    input.emit_one('old', 0)
    input.emit_one('old', 1)
    waiting(5) { sleep 0.1 until buffer_files.any? }

    # First handoff completes the switch, but the first chunk write fails in
    # the flushing agent's flush thread: the chunk is taken back and retried.
    output.fail_next_writes!(1)
    @root_agent.cancel_source_only!
    assert { !mode.enabled? }

    # The retry happens inside the flushing output; all records arrive once.
    wait_output_records(2)
    wait_buffer_files_empty

    records = output.events['handoff.event']
    assert_equal 2, records.size
    assert_equal [0, 1], records.map { |r| r['num'] }.sort
    assert_equal records.size, records.map { |r| r['num'] }.uniq.size
    assert { output.write_attempts >= 2 }
  end
end
