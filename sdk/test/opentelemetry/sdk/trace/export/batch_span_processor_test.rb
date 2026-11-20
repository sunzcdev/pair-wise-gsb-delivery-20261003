# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor do
  BatchSpanProcessor = OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor
  SUCCESS = OpenTelemetry::SDK::Trace::Export::SUCCESS
  FAILURE = OpenTelemetry::SDK::Trace::Export::FAILURE
  TIMEOUT = OpenTelemetry::SDK::Trace::Export::TIMEOUT

  class TestExporter
    def initialize(status_codes: nil)
      @status_codes = status_codes || []
      @batches = []
      @failed_batches = []
    end

    attr_reader :batches, :failed_batches

    def export(batch, timeout: nil)
      # If status codes is empty, its a success for less verbose testing
      s = @status_codes.shift
      if s.nil? || s == SUCCESS
        @batches << batch
        SUCCESS
      else
        @failed_batches << batch
        s
      end
    end

    def shutdown(timeout: nil); end

    def force_flush(timeout: nil); end
  end

  class NotAnExporter
  end

  class RaisingExporter
    def export(batch, timeout: nil)
      raise 'boom!'
    end

    def shutdown(timeout: nil); end

    def force_flush(timeout: nil); end
  end

  # An ordinary exporter whose export call blocks until #resume is called. It
  # lets a test stall the export pipeline (so the queue fills up) and then let
  # every buffered span through again, without bypassing the normal exporter
  # path.
  class PausableExporter
    def initialize
      @mutex = Mutex.new
      @condition = ConditionVariable.new
      @paused = false
      @exporting = false
      @batches = []
    end

    def batches
      @mutex.synchronize { @batches.dup }
    end

    def pause
      @mutex.synchronize { @paused = true }
    end

    def resume
      @mutex.synchronize do
        @paused = false
        @condition.broadcast
      end
    end

    def wait_until_exporting
      @mutex.synchronize do
        @condition.wait(@mutex) until @exporting
      end
    end

    def export(batch, timeout: nil)
      @mutex.synchronize do
        @exporting = true
        @condition.broadcast
        @condition.wait(@mutex) while @paused
        @exporting = false
        @batches << batch
      end
      SUCCESS
    end

    def shutdown(timeout: nil); end

    def force_flush(timeout: nil); end
  end

  class TestMetricsReporter
    attr_reader :reported_metrics

    def initialize
      @reported_metrics = {}
    end

    def add_to_counter(metric, increment: 1, labels: {})
      @reported_metrics[metric] ||= []
      @reported_metrics[metric] << [increment, labels]
    end

    def record_value(metric, value:, labels: {}); end

    def observe_value(metric, value:, labels: {}); end
  end

  class TestSpan
    def initialize(id = nil, recording: true)
      trace_flags = recording ? OpenTelemetry::Trace::TraceFlags::SAMPLED : OpenTelemetry::Trace::TraceFlags::DEFAULT
      @context = OpenTelemetry::Trace::SpanContext.new(trace_flags: trace_flags)
      @id = id
      @recording = recording
    end

    attr_reader :id, :context

    def recording?
      @recording
    end

    def to_span_data
      self
    end
  end

  class GroupedTestSpan < TestSpan
    def initialize(group, id = nil)
      super(id)
      @group = group
    end

    attr_reader :group
  end

  # Minimal stand-in for OpenTelemetry::SDK::Resources::Resource that exposes a
  # service.name attribute through the public enumerator.
  class ServiceResource
    def initialize(service_name)
      @service_name = service_name
    end

    def attribute_enumerator
      [[OpenTelemetry::SemanticConventions::Resource::SERVICE_NAME, @service_name]].each
    end
  end

  class ServiceTestSpan < TestSpan
    def initialize(service_name, id = nil)
      super(id)
      @resource = ServiceResource.new(service_name)
    end

    attr_reader :resource
  end

  describe 'initialization' do
    it 'if max batch size is gt max queue size raise' do
      assert_raises ArgumentError do
        BatchSpanProcessor.new(TestExporter.new, max_queue_size: 6, max_export_batch_size: 999)
      end
    end

    it 'raises if OTEL_BSP_EXPORT_TIMEOUT env var is not numeric' do
      assert_raises ArgumentError do
        OpenTelemetry::TestHelpers.with_env('OTEL_BSP_EXPORT_TIMEOUT' => 'foo') do
          BatchSpanProcessor.new(TestExporter.new)
        end
      end
    end

    it 'raises if exporter is nil' do
      _(-> { BatchSpanProcessor.new(nil) }).must_raise(ArgumentError)
    end

    it 'raises if exporter is not an exporter' do
      _(-> { BatchSpanProcessor.new(NotAnExporter.new) }).must_raise(ArgumentError)
    end

    it 'sets parameters from the environment' do
      bsp = OpenTelemetry::TestHelpers.with_env('OTEL_BSP_EXPORT_TIMEOUT' => '4',
                                                'OTEL_BSP_SCHEDULE_DELAY' => '3',
                                                'OTEL_BSP_MAX_QUEUE_SIZE' => '2',
                                                'OTEL_BSP_MAX_EXPORT_BATCH_SIZE' => '1') do
        BatchSpanProcessor.new(TestExporter.new)
      end
      _(bsp.instance_variable_get(:@exporter_timeout_seconds)).must_equal 0.004
      _(bsp.instance_variable_get(:@delay_seconds)).must_equal 0.003
      _(bsp.instance_variable_get(:@max_queue_size)).must_equal 2
      _(bsp.instance_variable_get(:@batch_size)).must_equal 1
    end

    it 'prefers explicit parameters rather than the environment' do
      bsp = OpenTelemetry::TestHelpers.with_env('OTEL_BSP_EXPORT_TIMEOUT' => '4',
                                                'OTEL_BSP_SCHEDULE_DELAY' => '3',
                                                'OTEL_BSP_MAX_QUEUE_SIZE' => '2',
                                                'OTEL_BSP_MAX_EXPORT_BATCH_SIZE' => '1') do
        BatchSpanProcessor.new(TestExporter.new,
                               exporter_timeout: 10,
                               schedule_delay: 9,
                               max_queue_size: 8,
                               max_export_batch_size: 7)
      end
      _(bsp.instance_variable_get(:@exporter_timeout_seconds)).must_equal 0.01
      _(bsp.instance_variable_get(:@delay_seconds)).must_equal 0.009
      _(bsp.instance_variable_get(:@max_queue_size)).must_equal 8
      _(bsp.instance_variable_get(:@batch_size)).must_equal 7
    end

    it 'sets defaults for parameters not in the environment' do
      bsp = BatchSpanProcessor.new(TestExporter.new)
      _(bsp.instance_variable_get(:@exporter_timeout_seconds)).must_equal 30.0
      _(bsp.instance_variable_get(:@delay_seconds)).must_equal 5.0
      _(bsp.instance_variable_get(:@max_queue_size)).must_equal 2048
      _(bsp.instance_variable_get(:@batch_size)).must_equal 512
    end

    it 'spawns a thread on boot by default' do
      mock = Minitest::Mock.new
      mock.expect(:call, nil)

      Thread.stub(:new, mock) do
        BatchSpanProcessor.new(TestExporter.new)
      end

      mock.verify
    end

    it 'spawns a thread on boot if OTEL_RUBY_BSP_START_THREAD_ON_BOOT is true' do
      mock = Minitest::Mock.new
      mock.expect(:call, nil)

      Thread.stub(:new, mock) do
        OpenTelemetry::TestHelpers.with_env('OTEL_RUBY_BSP_START_THREAD_ON_BOOT' => 'true') do
          BatchSpanProcessor.new(TestExporter.new)
        end
      end

      mock.verify
    end

    it 'does not spawn a thread on boot if OTEL_RUBY_BSP_START_THREAD_ON_BOOT is false' do
      mock = Minitest::Mock.new
      mock.expect(:call, nil)

      Thread.stub(:new, mock) do
        OpenTelemetry::TestHelpers.with_env('OTEL_RUBY_BSP_START_THREAD_ON_BOOT' => 'false') do
          BatchSpanProcessor.new(TestExporter.new)
        end
      end
    end

    it 'prefers explicit start_thread_on_boot parameter rather than the environment' do
      mock = Minitest::Mock.new
      mock.expect(:call, nil)

      Thread.stub(:new, mock) do
        OpenTelemetry::TestHelpers.with_env('OTEL_RUBY_BSP_START_THREAD_ON_BOOT' => 'true') do
          BatchSpanProcessor.new(TestExporter.new,
                                 start_thread_on_boot: false)
        end
      end
    end
  end

  describe '#force_flush' do
    it 'reenqueues excess spans on timeout' do
      test_exporter = TestExporter.new
      bsp = BatchSpanProcessor.new(test_exporter)
      bsp.on_finish(TestSpan.new)
      bsp.on_finish(TestSpan.new)
      result = bsp.force_flush(timeout: 0)

      _(result).must_equal(TIMEOUT)

      _(test_exporter.failed_batches.size).must_equal(0)
      _(test_exporter.batches.size).must_equal(0)

      _(bsp.instance_variable_get(:@spans).size).must_equal(2)
    end
  end

  describe '#shutdown' do
    it 'respects the timeout' do
      test_exporter = TestExporter.new
      bsp = BatchSpanProcessor.new(test_exporter)
      bsp.on_finish(TestSpan.new)
      bsp.shutdown(timeout: 0)

      _(test_exporter.failed_batches.size).must_equal(0)
      _(test_exporter.batches.size).must_equal(0)

      _(bsp.instance_variable_get(:@spans).size).must_equal(0)
    end

    it 'works if the thread is not running' do
      bsp = BatchSpanProcessor.new(TestExporter.new, start_thread_on_boot: false)
      bsp.shutdown(timeout: 0)
    end

    it 'returns a SUCCESS status if no error' do
      test_exporter = TestExporter.new
      test_exporter.instance_eval do
        def shutdown(timeout: nil)
          SUCCESS
        end
      end

      bsp = BatchSpanProcessor.new(test_exporter)
      bsp.on_finish(TestSpan.new)
      result = bsp.shutdown(timeout: 0)

      _(result).must_equal(SUCCESS)
    end

    it 'returns a FAILURE status if a non specific export error occurs' do
      test_exporter = TestExporter.new
      test_exporter.instance_eval do
        def shutdown(timeout: nil)
          FAILURE
        end
      end

      bsp = BatchSpanProcessor.new(test_exporter)
      bsp.on_finish(TestSpan.new)
      result = bsp.shutdown(timeout: 0)

      _(result).must_equal(FAILURE)
    end

    it 'returns a TIMEOUT status if a timeout export error occurs' do
      test_exporter = TestExporter.new
      test_exporter.instance_eval do
        def shutdown(timeout: nil)
          TIMEOUT
        end
      end

      bsp = BatchSpanProcessor.new(test_exporter)
      bsp.on_finish(TestSpan.new)
      result = bsp.shutdown(timeout: 0)

      _(result).must_equal(TIMEOUT)
    end
  end

  describe 'lifecycle' do
    it 'should stop and start correctly' do
      bsp = BatchSpanProcessor.new(TestExporter.new)
      bsp.shutdown
    end

    it 'should flush everything on shutdown' do
      te = TestExporter.new
      bsp = BatchSpanProcessor.new(te)
      ts = TestSpan.new
      bsp.on_finish(ts)

      bsp.shutdown

      _(te.batches).must_equal [[ts]]
    end
  end

  describe 'batching' do
    it 'should batch up to but not over the max_batch' do
      te = TestExporter.new

      bsp = BatchSpanProcessor.new(te, max_queue_size: 6, max_export_batch_size: 3)

      tss = [TestSpan.new, TestSpan.new, TestSpan.new, TestSpan.new]
      tss.each { |ts| bsp.on_finish(ts) }
      bsp.shutdown

      _(te.batches[0].size).must_equal(3)
      _(te.batches[1].size).must_equal(1)
    end

    it 'should batch only recording samples' do
      te = TestExporter.new

      bsp = BatchSpanProcessor.new(te, max_queue_size: 6, max_export_batch_size: 3)

      tss = [TestSpan.new, TestSpan.new(nil, recording: false)]
      tss.each { |ts| bsp.on_finish(ts) }
      bsp.shutdown

      _(te.batches[0].size).must_equal(1)
    end
  end

  describe 'export retry' do
    it 'should not retry on FAILURE exports' do
      te = TestExporter.new(status_codes: [FAILURE, SUCCESS])

      bsp = BatchSpanProcessor.new(te,
                                   schedule_delay: 999,
                                   max_queue_size: 6,
                                   max_export_batch_size: 3)

      tss = [TestSpan.new, TestSpan.new, TestSpan.new, TestSpan.new]
      tss.each { |ts| bsp.on_finish(ts) }

      # Ensure that our work thread has time to loop
      sleep(1)
      bsp.shutdown

      _(te.batches.size).must_equal(1)
      _(te.batches[0].size).must_equal(1)

      _(te.failed_batches.size).must_equal(1)
      _(te.failed_batches[0].size).must_equal(3)
    end
  end

  describe 'stress test' do
    it 'shouldnt blow up with a lot of things' do
      te = TestExporter.new

      bsp = BatchSpanProcessor.new(te)
      producers = Array.new(10) do |i|
        Thread.new do
          x = i * 10
          10.times do |j|
            bsp.on_finish(TestSpan.new(x + j))
          end
          sleep(rand(0.01))
        end
      end
      producers.each(&:join)
      bsp.shutdown

      out = te.batches.flatten.map(&:id).sort

      expected = Array.new(100) { |i| i }

      _(out).must_equal(expected)
    end
  end

  describe 'faulty exporter' do
    let(:exporter) { RaisingExporter.new }
    let(:bsp) { BatchSpanProcessor.new(exporter, metrics_reporter: metrics_reporter) }
    let(:metrics_reporter) { TestMetricsReporter.new }

    it 'reports export failures' do
      tss = [TestSpan.new, TestSpan.new, TestSpan.new, TestSpan.new]
      tss.each { |ts| bsp.on_finish(ts) }
      bsp.shutdown

      _(metrics_reporter.reported_metrics['otel.bsp.error']).wont_be_nil
      _(metrics_reporter.reported_metrics['otel.bsp.error'][0][0]).must_equal(1)
      _(metrics_reporter.reported_metrics['otel.bsp.error'][0][1]).must_equal({ 'reason' => 'RuntimeError' })
    end
  end

  describe 'dropped spans metrics' do
    let(:metrics_reporter) { TestMetricsReporter.new }

    it 'reports consistent label keys' do
      exporter = TestExporter.new(status_codes: [FAILURE])
      bsp = BatchSpanProcessor.new(exporter,
                                   metrics_reporter: metrics_reporter,
                                   max_queue_size: 1,
                                   max_export_batch_size: 1,
                                   start_thread_on_boot: false)

      bsp.on_finish(TestSpan.new('dropped-because-buffer-full'))
      bsp.on_finish(TestSpan.new('causes-export-failure'))
      bsp.shutdown # triggers export failure, no function associated

      dropped = metrics_reporter.reported_metrics['otel.bsp.dropped_spans']
      _(dropped).wont_be_nil
      dropped_buffer_full_label_names, dropped_export_failure_label_names = dropped.map { |_count, labels| labels.keys.sort }
      _(dropped_buffer_full_label_names).must_equal(dropped_export_failure_label_names)
    end
  end

  describe 'group reservations' do
    let(:group_by) { ->(span) { span.group } }

    def received_groups(exporter)
      exporter.batches.flatten.map(&:group)
    end

    it 'rejects reservations that are not a hash of non-negative integers' do
      assert_raises(ArgumentError) { BatchSpanProcessor.new(TestExporter.new, group_reservations: 'nope') }
      assert_raises(ArgumentError) { BatchSpanProcessor.new(TestExporter.new, group_reservations: { 'a' => -1 }) }
      assert_raises(ArgumentError) { BatchSpanProcessor.new(TestExporter.new, group_reservations: { 'a' => 1.5 }) }
    end

    it 'rejects reservations that exceed the global queue cap' do
      assert_raises(ArgumentError) do
        BatchSpanProcessor.new(TestExporter.new, max_queue_size: 10, group_reservations: { 'a' => 6, 'b' => 5 })
      end
    end

    it 'rejects a group_by that does not respond to call' do
      assert_raises(ArgumentError) { BatchSpanProcessor.new(TestExporter.new, group_by: 'nope') }
    end

    it 'normalizes reservation keys to strings' do
      bsp = BatchSpanProcessor.new(TestExporter.new,
                                   max_queue_size: 10,
                                   max_export_batch_size: 1,
                                   group_reservations: { a: 4, b: 4 })
      _(bsp.instance_variable_get(:@group_reservations)).must_equal({ 'a' => 4, 'b' => 4 })
    end

    it 'keeps the original FIFO behavior when no reservations are configured' do
      exporter = TestExporter.new
      reporter = TestMetricsReporter.new
      bsp = BatchSpanProcessor.new(exporter,
                                   max_queue_size: 2,
                                   max_export_batch_size: 1,
                                   metrics_reporter: reporter,
                                   start_thread_on_boot: false)

      3.times { |i| bsp.on_finish(TestSpan.new(i)) }

      _(bsp.instance_variable_get(:@spans).map(&:span).map(&:id)).must_equal([1, 2])
      dropped = reporter.reported_metrics['otel.bsp.dropped_spans']
      _(dropped).must_equal([[1, { 'group' => 'default', 'reason' => 'buffer-full', OpenTelemetry::SemanticConventions::Trace::CODE_FUNCTION => 'on_finish' }]])
    end

    it 'lets an idle group lend its unused share to other groups' do
      exporter = TestExporter.new
      bsp = BatchSpanProcessor.new(exporter,
                                   max_queue_size: 10,
                                   max_export_batch_size: 3,
                                   group_reservations: { 'a' => 4, 'b' => 4 },
                                   group_by: group_by,
                                   start_thread_on_boot: false)

      10.times { bsp.on_finish(GroupedTestSpan.new('a')) }

      _(bsp.instance_variable_get(:@spans).size).must_equal(10)
      bsp.shutdown
      _(exporter.batches.flatten.size).must_equal(10)
    end

    it 'sheds only the overloaded group\'s own oldest entries when it is at its share' do
      reporter = TestMetricsReporter.new
      bsp = BatchSpanProcessor.new(TestExporter.new,
                                   max_queue_size: 10,
                                   max_export_batch_size: 2,
                                   group_reservations: { 'a' => 5, 'b' => 5 },
                                   group_by: group_by,
                                   metrics_reporter: reporter,
                                   start_thread_on_boot: false)

      5.times { bsp.on_finish(GroupedTestSpan.new('b', 'b')) }
      5.times { bsp.on_finish(GroupedTestSpan.new('a', 'a')) }
      bsp.on_finish(GroupedTestSpan.new('a', 'a-late'))

      # The full queue stays at each group's share; the overloaded group
      # replaces its own oldest span instead of evicting group 'b'.
      _(bsp.instance_variable_get(:@spans).size).must_equal(10)
      _(bsp.instance_variable_get(:@group_counts).to_h).must_equal({ 'b' => 5, 'a' => 5 })
      _(bsp.instance_variable_get(:@spans).count { |entry| entry.group == 'b' }).must_equal(5)
      _(bsp.instance_variable_get(:@spans).map(&:span).map(&:id)).must_equal(%w[b b b b b a a a a a-late])
      dropped = reporter.reported_metrics['otel.bsp.dropped_spans']
      _(dropped.size).must_equal(1)
      _(dropped[0][1]['group']).must_equal('a')
      _(dropped[0][1]['reason']).must_equal('buffer-full')
    end

    it 'drops incoming spans from a group with no reservation and no borrowed space' do
      reporter = TestMetricsReporter.new
      bsp = BatchSpanProcessor.new(TestExporter.new,
                                   max_queue_size: 2,
                                   max_export_batch_size: 1,
                                   group_reservations: { 'a' => 1, 'b' => 1 },
                                   group_by: group_by,
                                   metrics_reporter: reporter,
                                   start_thread_on_boot: false)

      bsp.on_finish(GroupedTestSpan.new('a'))
      bsp.on_finish(GroupedTestSpan.new('b'))
      bsp.on_finish(GroupedTestSpan.new('c'))

      _(bsp.instance_variable_get(:@spans).size).must_equal(2)
      dropped = reporter.reported_metrics['otel.bsp.dropped_spans']
      _(dropped.size).must_equal(1)
      _(dropped[0][1]['group']).must_equal('default')
    end

    it 'maps unconfigured groups and nil to a single bounded default group' do
      reporter = TestMetricsReporter.new
      bsp = BatchSpanProcessor.new(TestExporter.new,
                                   max_queue_size: 1,
                                   max_export_batch_size: 1,
                                   group_reservations: { 'a' => 1 },
                                   group_by: group_by,
                                   metrics_reporter: reporter,
                                   start_thread_on_boot: false)

      bsp.on_finish(GroupedTestSpan.new('a'))
      bsp.on_finish(GroupedTestSpan.new('b'))
      bsp.on_finish(GroupedTestSpan.new(nil))

      groups = reporter.reported_metrics['otel.bsp.dropped_spans'].map { |_count, labels| labels['group'] }
      _(groups.uniq).must_equal(['default'])
    end

    it 'falls back to the default group when group_by raises' do
      bsp = BatchSpanProcessor.new(TestExporter.new,
                                   group_reservations: { 'a' => 1 },
                                   group_by: ->(_span) { raise 'boom!' },
                                   start_thread_on_boot: false)

      bsp.on_finish(GroupedTestSpan.new('a'))

      _(bsp.instance_variable_get(:@spans).first.group).must_equal('default')
    end

    it 'groups spans by their resource service name when no group_by is given' do
      exporter = TestExporter.new
      bsp = BatchSpanProcessor.new(exporter,
                                   max_queue_size: 10,
                                   max_export_batch_size: 10,
                                   group_reservations: { 'service-a' => 4, 'service-b' => 4 },
                                   start_thread_on_boot: false)

      bsp.on_finish(ServiceTestSpan.new('service-a'))
      bsp.on_finish(ServiceTestSpan.new('service-b'))

      groups = bsp.instance_variable_get(:@spans).map(&:group)
      _(groups).must_equal(%w[service-a service-b])

      bsp.shutdown
      _(exporter.batches.flatten.size).must_equal(2)
    end

    it 'maps an unknown service name to the default group' do
      bsp = BatchSpanProcessor.new(TestExporter.new,
                                   group_reservations: { 'service-a' => 1 },
                                   start_thread_on_boot: false)

      bsp.on_finish(ServiceTestSpan.new('other-service'))

      _(bsp.instance_variable_get(:@spans).first.group).must_equal('default')
    end

    it 'reports bounded group and reason labels on every drop path' do
      reporter = TestMetricsReporter.new
      exporter = TestExporter.new(status_codes: [FAILURE])
      bsp = BatchSpanProcessor.new(exporter,
                                   max_queue_size: 1,
                                   max_export_batch_size: 1,
                                   group_reservations: { 'a' => 1 },
                                   group_by: group_by,
                                   metrics_reporter: reporter,
                                   start_thread_on_boot: false)

      bsp.on_finish(GroupedTestSpan.new('a', 'kept'))
      bsp.on_finish(GroupedTestSpan.new('b', 'dropped-because-buffer-full'))
      bsp.shutdown # triggers export failure, reporting an export-failure drop

      dropped = reporter.reported_metrics['otel.bsp.dropped_spans']
      _(dropped).wont_be_nil
      _(dropped.map { |_count, labels| labels.keys.sort }.uniq.size).must_equal(1)
      _(dropped.map { |_count, labels| labels['group'] }.sort.uniq).must_equal(%w[a default])
      _(dropped.map { |_count, labels| labels['reason'] }.sort.uniq).must_equal(%w[buffer-full export-failure])
    end

    it 'protects each group minimum while the exporter is paused' do
      queue_size = 10
      reservations = { 'a' => 6, 'b' => 4 }

      # The first span is already inside the blocked export call when the queue
      # fills, so the exporter ultimately receives the queue plus that one.
      total_received = queue_size + 1

      without_reservations = run_squeeze_scenario(queue_size: queue_size, reservations: nil)
      with_reservations = run_squeeze_scenario(queue_size: queue_size, reservations: reservations)

      # Without reservations the noisy group 'a' squeezes every 'b' span out.
      _(without_reservations.count('b')).must_equal(0)
      _(without_reservations.count('a')).must_equal(total_received)

      # With reservations the quieter group keeps at least its minimum share.
      _(with_reservations.count('b')).must_equal(reservations['b'])
      _(with_reservations.count('a')).must_equal(total_received - reservations['b'])
    end

    def run_squeeze_scenario(queue_size:, reservations:)
      exporter = PausableExporter.new
      bsp = BatchSpanProcessor.new(exporter,
                                   schedule_delay: 1,
                                   max_queue_size: queue_size,
                                   max_export_batch_size: 1,
                                   group_reservations: reservations || {},
                                   group_by: group_by)

      # Pause the exporter first, then hand it one span so the pipeline stalls
      # while the queue keeps filling from both services.
      exporter.pause
      bsp.on_finish(GroupedTestSpan.new('a'))
      exporter.wait_until_exporting

      5.times { bsp.on_finish(GroupedTestSpan.new('b')) }
      25.times { bsp.on_finish(GroupedTestSpan.new('a')) }

      exporter.resume
      bsp.shutdown

      received_groups(exporter)
    end
  end

  describe 'fork safety test' do
    let(:exporter) { TestExporter.new }
    let(:bsp) do
      BatchSpanProcessor.new(exporter,
                             max_queue_size: 10,
                             max_export_batch_size: 3)
    end

    it 'when ThreadError is raised it handles it gracefully' do
      parent_pid = bsp.instance_variable_get(:@pid)
      parent_work_thread_id = bsp.instance_variable_get(:@thread).object_id
      Process.stub(:pid, parent_pid + rand(1..10)) do
        Thread.stub(:new, -> { raise ThreadError }) do
          bsp.on_finish(TestSpan.new)
        end

        current_pid = bsp.instance_variable_get(:@pid)
        current_work_thread_id = bsp.instance_variable_get(:@thread).object_id
        _(parent_pid).wont_equal current_pid
        _(parent_work_thread_id).must_equal current_work_thread_id
      end
    end

    describe 'when a process fork occurs' do
      it 'creates new work thread when on_finish is called' do
        parent_pid = bsp.instance_variable_get(:@pid)
        parent_work_thread_id = bsp.instance_variable_get(:@thread).object_id
        Process.stub(:pid, parent_pid + rand(1..10)) do
          # Start a new span on the forked process and export it.
          bsp.on_finish(TestSpan.new)
          current_pid = bsp.instance_variable_get(:@pid)
          current_work_thread_id = bsp.instance_variable_get(:@thread).object_id
          _(parent_pid).wont_equal current_pid
          _(parent_work_thread_id).wont_equal current_work_thread_id
        end
      end

      it 'creates new work thread when force_flush' do
        parent_pid = bsp.instance_variable_get(:@pid)
        parent_work_thread_id = bsp.instance_variable_get(:@thread).object_id
        Process.stub(:pid, parent_pid + rand(1..10)) do
          # Force flush on the forked process.
          bsp.force_flush
          current_pid = bsp.instance_variable_get(:@pid)
          current_work_thread_id = bsp.instance_variable_get(:@thread).object_id
          _(parent_pid).wont_equal current_pid
          _(parent_work_thread_id).wont_equal current_work_thread_id
        end
      end
    end
  end
end
