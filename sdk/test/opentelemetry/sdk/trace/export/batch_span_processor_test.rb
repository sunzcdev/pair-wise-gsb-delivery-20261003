# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor do
  BatchSpanProcessor = OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor
  ExportResult = OpenTelemetry::SDK::Trace::Export::ExportResult
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

  # Reports a per-span receipt for each batch. +results+ is consumed one call
  # at a time; a nil entry means a plain SUCCESS.
  class PartialExporter
    attr_reader :batches

    def initialize(results)
      @results = results
      @batches = []
    end

    def export(batch, timeout: nil)
      @batches << batch
      @results.shift || SUCCESS
    end

    def shutdown(timeout: nil); end

    def force_flush(timeout: nil)
      SUCCESS
    end
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

  describe 'partial acceptance' do
    let(:metrics_reporter) { TestMetricsReporter.new }

    # Builds three spans with stable ids so assertions can identify them after
    # they cross the exporter boundary.
    def three_spans
      [TestSpan.new('accepted'), TestSpan.new('rejected'), TestSpan.new('retryable')]
    end

    def dropped_counts(reporter)
      (reporter.reported_metrics['otel.bsp.dropped_spans'] || []).each_with_object(Hash.new(0)) do |(count, labels), acc|
        acc[labels['reason']] += count
      end
    end

    it 'resends only the retryable segment and reconciles the final drop counts' do
      exporter = PartialExporter.new([
                                       ExportResult.new(accepted: [0], rejected: [1], retryable: [2]),
                                       ExportResult.new(accepted: [0])
                                     ])
      bsp = BatchSpanProcessor.new(exporter,
                                   metrics_reporter: metrics_reporter,
                                   max_queue_size: 6,
                                   max_export_batch_size: 3,
                                   start_thread_on_boot: false)
      spans = three_spans
      spans.each { |span| bsp.on_finish(span) }
      result = bsp.force_flush

      _(result).must_equal(SUCCESS)

      # First call receives the whole batch, second call only the retryable span.
      _(exporter.batches.size).must_equal(2)
      _(exporter.batches[0]).must_equal(spans)
      _(exporter.batches[1]).must_equal([spans[2]])

      # Permanent rejection is counted as dropped, the retried span was accepted.
      _(dropped_counts(metrics_reporter)).must_equal({ 'export-rejected' => 1 })
      _(metrics_reporter.reported_metrics['otel.bsp.exported_spans'].sum { |count, _| count }).must_equal(2)

      # Nothing is left behind in the queue or re-exported.
      _(bsp.instance_variable_get(:@spans)).must_be_empty
    end

    it 'keeps the legacy SUCCESS return value working' do
      exporter = PartialExporter.new([])
      bsp = BatchSpanProcessor.new(exporter,
                                   metrics_reporter: metrics_reporter,
                                   max_queue_size: 6,
                                   max_export_batch_size: 3,
                                   start_thread_on_boot: false)
      spans = three_spans
      spans.each { |span| bsp.on_finish(span) }

      _(bsp.force_flush).must_equal(SUCCESS)
      _(exporter.batches).must_equal([spans])
      _(dropped_counts(metrics_reporter)).must_be_empty
    end

    it 'keeps the legacy FAILURE return value working' do
      exporter = PartialExporter.new([FAILURE])
      bsp = BatchSpanProcessor.new(exporter,
                                   metrics_reporter: metrics_reporter,
                                   max_queue_size: 6,
                                   max_export_batch_size: 3,
                                   start_thread_on_boot: false)
      spans = three_spans
      spans.each { |span| bsp.on_finish(span) }

      _(bsp.force_flush).must_equal(FAILURE)
      _(exporter.batches.size).must_equal(1)
      _(dropped_counts(metrics_reporter)).must_equal({ 'export-failure' => 3 })
    end

    it 'treats an all-accepted receipt as success and does not resend' do
      exporter = PartialExporter.new([ExportResult.new(accepted: [0, 1, 2])])
      bsp = BatchSpanProcessor.new(exporter,
                                   metrics_reporter: metrics_reporter,
                                   max_queue_size: 6,
                                   max_export_batch_size: 3,
                                   start_thread_on_boot: false)
      three_spans.each { |span| bsp.on_finish(span) }

      _(bsp.force_flush).must_equal(SUCCESS)
      _(exporter.batches.size).must_equal(1)
      _(dropped_counts(metrics_reporter)).must_be_empty
      _(metrics_reporter.reported_metrics['otel.bsp.export.success'].size).must_equal(1)
    end

    it 'drops spans that are still retryable once the attempt limit is reached' do
      always_retry = ExportResult.new(retryable: [0, 1, 2])
      exporter = PartialExporter.new([always_retry] * BatchSpanProcessor::MAX_EXPORT_ATTEMPTS)
      bsp = BatchSpanProcessor.new(exporter,
                                   metrics_reporter: metrics_reporter,
                                   max_queue_size: 6,
                                   max_export_batch_size: 3,
                                   start_thread_on_boot: false)
      three_spans.each { |span| bsp.on_finish(span) }

      _(bsp.force_flush).must_equal(FAILURE)
      _(exporter.batches.size).must_equal(BatchSpanProcessor::MAX_EXPORT_ATTEMPTS)
      _(dropped_counts(metrics_reporter)).must_equal({ 'export-retry-exhausted' => 3 })
    end

    it 'does not resend a span that was both rejected and retryable' do
      exporter = PartialExporter.new([ExportResult.new(rejected: [1], retryable: [1])])
      bsp = BatchSpanProcessor.new(exporter,
                                   metrics_reporter: metrics_reporter,
                                   max_queue_size: 6,
                                   max_export_batch_size: 3,
                                   start_thread_on_boot: false)
      spans = three_spans
      spans.each { |span| bsp.on_finish(span) }

      _(bsp.force_flush).must_equal(SUCCESS)
      _(exporter.batches.size).must_equal(1)
      _(dropped_counts(metrics_reporter)).must_equal({ 'export-rejected' => 1 })
      _(metrics_reporter.reported_metrics['otel.bsp.exported_spans'].sum { |count, _| count }).must_equal(2)
    end

    it 'ignores receipts that point outside the batch' do
      exporter = PartialExporter.new([ExportResult.new(accepted: [0, 1, 2], rejected: [99], retryable: [-1])])
      bsp = BatchSpanProcessor.new(exporter,
                                   metrics_reporter: metrics_reporter,
                                   max_queue_size: 6,
                                   max_export_batch_size: 3,
                                   start_thread_on_boot: false)
      three_spans.each { |span| bsp.on_finish(span) }

      _(bsp.force_flush).must_equal(SUCCESS)
      _(exporter.batches.size).must_equal(1)
      _(dropped_counts(metrics_reporter)).must_be_empty
    end

    it 'counts permanent rejections separately from an exhausted retry' do
      exporter = PartialExporter.new([
                                       ExportResult.new(rejected: [0], retryable: [1, 2]),
                                       ExportResult.new(rejected: [0], retryable: [1]),
                                       ExportResult.new(retryable: [0])
                                     ])
      bsp = BatchSpanProcessor.new(exporter,
                                   metrics_reporter: metrics_reporter,
                                   max_queue_size: 6,
                                   max_export_batch_size: 3,
                                   start_thread_on_boot: false)
      three_spans.each { |span| bsp.on_finish(span) }

      _(bsp.force_flush).must_equal(FAILURE)
      _(exporter.batches.size).must_equal(BatchSpanProcessor::MAX_EXPORT_ATTEMPTS)
      _(dropped_counts(metrics_reporter)).must_equal({ 'export-rejected' => 2, 'export-retry-exhausted' => 1 })
    end

    it 'reports dropped spans with consistent label keys' do
      exporter = PartialExporter.new([ExportResult.new(rejected: [0], retryable: [1, 2]),
                                      ExportResult.new(retryable: [0, 1]),
                                      ExportResult.new(retryable: [0, 1])])
      bsp = BatchSpanProcessor.new(exporter,
                                   metrics_reporter: metrics_reporter,
                                   max_queue_size: 6,
                                   max_export_batch_size: 3,
                                   start_thread_on_boot: false)
      three_spans.each { |span| bsp.on_finish(span) }
      bsp.force_flush

      dropped = metrics_reporter.reported_metrics['otel.bsp.dropped_spans']
      reasons = dropped.map { |_count, labels| labels['reason'] }
      label_key_sets = dropped.map { |_count, labels| labels.keys.sort }.uniq

      _(reasons).must_include 'export-rejected'
      _(reasons).must_include 'export-retry-exhausted'
      _(label_key_sets.size).must_equal(1)
      _(label_key_sets.first).must_equal(
        ['reason', OpenTelemetry::SemanticConventions::Trace::CODE_FUNCTION].sort
      )
    end

    it 'applies the receipt to batches exported by the work thread' do
      exporter = PartialExporter.new([
                                       ExportResult.new(accepted: [0], rejected: [1], retryable: [2]),
                                       ExportResult.new(accepted: [0])
                                     ])
      bsp = BatchSpanProcessor.new(exporter,
                                   metrics_reporter: metrics_reporter,
                                   schedule_delay: 1,
                                   max_queue_size: 6,
                                   max_export_batch_size: 3)
      spans = three_spans
      spans.each { |span| bsp.on_finish(span) }

      # Let the work thread loop through both export attempts.
      sleep(1)
      bsp.shutdown

      _(exporter.batches.size).must_equal(2)
      _(exporter.batches[1]).must_equal([spans[2]])
      _(dropped_counts(metrics_reporter)).must_equal({ 'export-rejected' => 1 })
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
