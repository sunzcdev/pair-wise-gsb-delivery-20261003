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

  class LifecycleExporter < TestExporter
    attr_reader :force_flush_calls, :shutdown_calls

    def initialize(status_codes: nil)
      super
      @force_flush_calls = 0
      @shutdown_calls = 0
    end

    def force_flush(timeout: nil)
      @force_flush_calls += 1
      SUCCESS
    end

    def shutdown(timeout: nil)
      @shutdown_calls += 1
      SUCCESS
    end
  end

  class RaisingExporter
    def export(batch, timeout: nil)
      raise 'boom!'
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

  describe 'exporter factory' do
    # Drives a processor through a simulated fork without spawning a process.
    # Process.pid is replaced for the duration of the block, which is how the
    # processor decides whether it still owns its exporter. State is captured
    # and restored around the block so that, as with a real fork, changes made
    # by the child are invisible to the parent. Thread.new is stubbed out so
    # the child's simulated worker never races with the assertions.
    def in_forked_process(processor, pid)
      saved_ivars = processor.instance_variables.to_h { |name| [name, processor.instance_variable_get(name)] }
      saved_spans = processor.instance_variable_get(:@spans).dup
      Process.stub(:pid, pid) do
        Thread.stub(:new, ->(&_block) { nil }) do
          yield
        end
      end
    ensure
      saved_ivars&.each { |name, value| processor.instance_variable_set(name, value) }
      processor.instance_variable_set(:@spans, saved_spans) if saved_spans
    end

    let(:parent_pid) { Process.pid }
    let(:child_pid) { parent_pid + 1 }
    let(:parent_exporter) { LifecycleExporter.new }
    let(:child_exporter) { LifecycleExporter.new }
    let(:factory_calls) { [] }
    let(:factory) do
      lambda do
        factory_calls << Process.pid
        child_exporter
      end
    end
    let(:bsp) do
      BatchSpanProcessor.new(parent_exporter,
                             exporter_factory: factory,
                             max_queue_size: 10,
                             max_export_batch_size: 3,
                             start_thread_on_boot: false)
    end

    it 'does not require an exporter when a factory is given' do
      processor = BatchSpanProcessor.new(exporter_factory: -> { TestExporter.new }, start_thread_on_boot: false)
      _(processor.instance_variable_get(:@exporter)).must_be_nil
    end

    it 'raises when neither an exporter nor a factory is given' do
      _(-> { BatchSpanProcessor.new }).must_raise(ArgumentError)
    end

    it 'raises when the factory is not callable' do
      _(-> { BatchSpanProcessor.new(exporter_factory: Object.new) }).must_raise(ArgumentError)
    end

    it 'never calls the factory in the process that constructed it' do
      bsp.on_finish(TestSpan.new)
      bsp.shutdown

      _(factory_calls).must_be_empty
      _(parent_exporter.shutdown_calls).must_equal(1)
      _(child_exporter.shutdown_calls).must_equal(0)
    end

    it 'calls the factory on the first on_finish in a forked process' do
      in_forked_process(bsp, child_pid) do
        _(factory_calls).must_be_empty
        bsp.on_finish(TestSpan.new)
        _(factory_calls).must_equal([child_pid])
        bsp.on_finish(TestSpan.new)
        _(factory_calls).must_equal([child_pid])
      end
    end

    it 'exports spans from a forked process through its own exporter' do
      span = TestSpan.new
      in_forked_process(bsp, child_pid) do
        bsp.on_finish(span)
        _(bsp.force_flush).must_equal(SUCCESS)
      end

      _(child_exporter.batches).must_equal([[span]])
      _(parent_exporter.batches).must_be_empty
    end

    it 'settles each process with its own exporter exactly once' do
      in_forked_process(bsp, child_pid) do
        bsp.on_finish(TestSpan.new)
        _(bsp.shutdown).must_equal(SUCCESS)
        _(child_exporter.shutdown_calls).must_equal(1)
      end

      _(parent_exporter.shutdown_calls).must_equal(0)

      bsp.shutdown
      _(parent_exporter.shutdown_calls).must_equal(1)
      _(child_exporter.shutdown_calls).must_equal(1)
    end

    it 'does not create an exporter in a forked process that has nothing to flush' do
      in_forked_process(bsp, child_pid) do
        _(bsp.force_flush).must_equal(SUCCESS)
        _(factory_calls).must_be_empty

        _(bsp.shutdown).must_equal(SUCCESS)
        _(factory_calls).must_be_empty
        _(child_exporter.force_flush_calls).must_equal(0)
        _(child_exporter.shutdown_calls).must_equal(0)
      end
    end

    it 'does not hand spans queued by the parent to the child exporter' do
      parent_span = TestSpan.new
      bsp.on_finish(parent_span)

      child_span = TestSpan.new
      in_forked_process(bsp, child_pid) do
        bsp.on_finish(child_span)
        bsp.force_flush
      end

      _(child_exporter.batches.flatten).must_equal([child_span])
      _(parent_exporter.batches).must_be_empty
    end

    it 'does not export a batch taken by the parent through the child exporter' do
      # Simulate a worker that removed the batch before the fork and finishes
      # exporting after it: the exporter paired with the batch is the parent's.
      batch = [TestSpan.new]
      in_forked_process(bsp, child_pid) do
        bsp.on_finish(TestSpan.new) # create the child exporter
        result = bsp.send(:export_batch, batch, exporter: parent_exporter)
        _(result).must_equal(FAILURE)
      end

      _(parent_exporter.batches).must_be_empty
      _(child_exporter.batches).must_be_empty
    end

    it 'drops a parent snapshot instead of requeueing it when the fork happens mid-flush' do
      # The parent takes the snapshot, then the process forks during the
      # export. The spans left in the snapshot belong to the parent and must
      # not be requeued in the child or exported through the child's exporter.
      current_pid = parent_pid
      forked_pid = child_pid
      forking_exporter = Class.new(TestExporter) do
        define_method(:export) do |batch, timeout: nil|
          current_pid = forked_pid # the fork happens here
          super(batch, timeout: timeout)
        end
      end

      metrics_reporter = TestMetricsReporter.new
      processor = BatchSpanProcessor.new(exporter_factory: -> { forking_exporter.new(status_codes: [FAILURE]) },
                                         metrics_reporter: metrics_reporter,
                                         max_queue_size: 10,
                                         max_export_batch_size: 1,
                                         start_thread_on_boot: false)

      Process.stub(:pid, -> { current_pid }) do
        Thread.stub(:new, ->(&_block) { nil }) do
          3.times { processor.on_finish(TestSpan.new) }
          _(processor.force_flush).must_equal(FAILURE)
        end
      end

      dropped = metrics_reporter.reported_metrics['otel.bsp.dropped_spans']
      reasons = dropped.map { |_increment, labels| labels['reason'] }
      _(reasons).must_include('fork')
      _(processor.instance_variable_get(:@spans)).must_be_empty
    end

    it 'reports a factory that raises without propagating it' do
      reported = []
      OpenTelemetry.stub(:handle_error, ->(exception: nil, message: nil) { reported << [exception, message] }) do
        processor = BatchSpanProcessor.new(exporter_factory: -> { raise 'no exporter for you' }, start_thread_on_boot: false)
        processor.on_finish(TestSpan.new)
        _(processor.instance_variable_get(:@exporter)).must_be_nil

        # The span cannot be exported, so the flush reports a failure instead
        # of raising; the factory error is the first one reported.
        _(processor.force_flush).must_equal(FAILURE)
      end
      _(reported.first.first).must_be_instance_of(RuntimeError)
      _(reported.first.first.message).must_equal('no exporter for you')
    end

    it 'reports a factory that returns something that is not an exporter' do
      reported = []
      factory = -> { NotAnExporter.new }
      OpenTelemetry.stub(:handle_error, ->(exception: nil, message: nil) { reported << [exception, message] }) do
        processor = BatchSpanProcessor.new(exporter_factory: factory, start_thread_on_boot: false)
        processor.on_finish(TestSpan.new)
        _(processor.instance_variable_get(:@exporter)).must_be_nil
      end
      _(reported.size).must_equal(1)
      _(reported.first.first).must_be_instance_of(ArgumentError)
    end

    it 'does not retry a failing factory for later spans' do
      calls = 0
      factory = lambda do
        calls += 1
        raise 'nope'
      end
      OpenTelemetry.stub(:handle_error, ->(exception: nil, message: nil) {}) do
        processor = BatchSpanProcessor.new(exporter_factory: factory, start_thread_on_boot: false)
        3.times { processor.on_finish(TestSpan.new) }
      end
      _(calls).must_equal(1)
    end

    it 'recreates the exporter after the pid changes a second time' do
      second_child_pid = child_pid + 1
      second_child_exporter = LifecycleExporter.new
      factory = lambda do
        factory_calls << Process.pid
        Process.pid == second_child_pid ? second_child_exporter : child_exporter
      end
      processor = BatchSpanProcessor.new(exporter_factory: factory, start_thread_on_boot: false)

      in_forked_process(processor, child_pid) { processor.on_finish(TestSpan.new) }
      in_forked_process(processor, second_child_pid) { processor.on_finish(TestSpan.new) }

      _(factory_calls).must_equal([child_pid, second_child_pid])
    end

    it 'creates the exporter on the first on_finish when only a factory is given' do
      exporter = LifecycleExporter.new
      processor = BatchSpanProcessor.new(exporter_factory: -> { exporter },
                                         schedule_delay: 10,
                                         max_queue_size: 10,
                                         max_export_batch_size: 1)
      span = TestSpan.new
      processor.on_finish(span)

      deadline = Time.now + 5
      sleep(0.01) until exporter.batches.any? || Time.now > deadline

      _(processor.shutdown).must_equal(SUCCESS)
      _(exporter.batches).must_equal([[span]])
      _(exporter.shutdown_calls).must_equal(1)
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

