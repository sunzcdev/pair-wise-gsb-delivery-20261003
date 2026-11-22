# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::SDK::Trace::TracerProvider do
  let(:samplers) { OpenTelemetry::SDK::Trace::Samplers }
  let(:subject) { OpenTelemetry::SDK::Trace::TracerProvider }
  let(:tracer_provider) { subject.new }

  describe '#initialize' do
    it 'activates a default SpanLimits and Sampler' do
      _(tracer_provider.span_limits).must_equal(SpanLimits::DEFAULT)
      _(tracer_provider.sampler).must_equal samplers.parent_based(root: samplers::ALWAYS_ON)
    end

    it 'configures samplers from environment' do
      sampler = OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_SAMPLER' => 'always_on') { subject.new.sampler }
      _(sampler).must_equal samplers::ALWAYS_ON

      sampler = OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_SAMPLER' => 'always_off') { subject.new.sampler }
      _(sampler).must_equal samplers::ALWAYS_OFF

      sampler = OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_SAMPLER' => 'traceidratio', 'OTEL_TRACES_SAMPLER_ARG' => '0.1') { subject.new.sampler }
      _(sampler).must_equal samplers.trace_id_ratio_based(0.1)

      sampler = OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_SAMPLER' => 'traceidratio') { subject.new.sampler }
      _(sampler).must_equal samplers.trace_id_ratio_based(1.0)

      sampler = OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_SAMPLER' => 'parentbased_always_on') { subject.new.sampler }
      _(sampler).must_equal samplers.parent_based(root: samplers::ALWAYS_ON)

      sampler = OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_SAMPLER' => 'parentbased_always_off') { subject.new.sampler }
      _(sampler).must_equal samplers.parent_based(root: samplers::ALWAYS_OFF)

      sampler = OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_SAMPLER' => 'parentbased_traceidratio', 'OTEL_TRACES_SAMPLER_ARG' => '0.2') { subject.new.sampler }
      _(sampler).must_equal samplers.parent_based(root: samplers.trace_id_ratio_based(0.2))

      sampler = OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_SAMPLER' => 'parentbased_traceidratio') { subject.new.sampler }
      _(sampler).must_equal samplers.parent_based(root: samplers.trace_id_ratio_based(1.0))
    end
  end

  describe '#shutdown' do
    let(:mock_span_processor)  { Minitest::Mock.new }
    let(:mock_span_processor2) { Minitest::Mock.new }

    it 'notifies the span processor' do
      mock_span_processor.expect(:shutdown, nil, [], timeout: nil)
      tracer_provider.add_span_processor(mock_span_processor)
      tracer_provider.shutdown
      mock_span_processor.verify
    end

    it 'warns if called more than once' do
      mock_logger = Minitest::Mock.new
      mock_logger.expect(:warn, nil, [String])
      tracer_provider.shutdown
      OpenTelemetry.stub :logger, mock_logger do
        tracer_provider.shutdown
      end
      mock_logger.verify
    end

    it 'only notifies the span processor once' do
      mock_span_processor.expect(:shutdown, nil, [], timeout: nil)
      tracer_provider.add_span_processor(mock_span_processor)
      tracer_provider.shutdown
      tracer_provider.shutdown
      mock_span_processor.verify
    end

    it 'supports multiple span processors' do
      mock_span_processor.expect(:shutdown, nil, [], timeout: nil)
      mock_span_processor2.expect(:shutdown, nil, [], timeout: nil)
      tracer_provider.add_span_processor(mock_span_processor)
      tracer_provider.add_span_processor(mock_span_processor2)
      tracer_provider.shutdown
      mock_span_processor.verify
      mock_span_processor2.verify
    end

    it 'does not deadlock if span processor is traced' do
      span_processor = OpenTelemetry::SDK::Trace::SpanProcessor.new
      tracer_provider.add_span_processor(span_processor)
      span_processor.stub(:shutdown, ->(timeout: nil) { tracer_provider.tracer.in_span('shutdown') {} }) do # rubocop:disable Lint/UnusedBlockArgument
        tracer_provider.shutdown
      end
      pass 'no deadlock'
    end
  end

  describe '#force_flush' do
    let(:mock_span_processor)  { Minitest::Mock.new }
    let(:mock_span_processor2) { Minitest::Mock.new }

    it 'notifies the span processor' do
      mock_span_processor.expect(:force_flush, nil, [], timeout: nil)
      tracer_provider.add_span_processor(mock_span_processor)
      tracer_provider.force_flush
      mock_span_processor.verify
    end

    it 'supports multiple span processors' do
      mock_span_processor.expect(:force_flush, nil, [], timeout: nil)
      mock_span_processor2.expect(:force_flush, nil, [], timeout: nil)
      tracer_provider.add_span_processor(mock_span_processor)
      tracer_provider.add_span_processor(mock_span_processor2)
      tracer_provider.force_flush
      mock_span_processor.verify
      mock_span_processor2.verify
    end
  end

  describe '#add_span_processor' do
    it 'does not add the processor if stopped' do
      mock_span_processor = Minitest::Mock.new
      tracer_provider.shutdown
      tracer_provider.add_span_processor(mock_span_processor)
      tracer_provider.tracer.in_span('span') {}
      mock_span_processor.verify
    end

    it 'adds the span processor to the active span processors' do
      mock_span_processor = Minitest::Mock.new
      mock_span_processor.expect(:on_start, nil, [Span, Context])
      mock_span_processor.expect(:on_finish, nil, [Span])
      tracer_provider.add_span_processor(mock_span_processor)
      tracer_provider.tracer.in_span('span') {}
      mock_span_processor.verify
    end

    it 'adds multiple span processors to the active span processors' do
      mock_processors = Array.new(2) { Minitest::Mock.new }
      mock_processors.each do |p|
        p.expect(:on_start, nil, [Span, Context])
        p.expect(:on_finish, nil, [Span])
        tracer_provider.add_span_processor(p)
      end

      tracer_provider.tracer.in_span('span') {}
      mock_processors.each(&:verify)
    end
  end

  describe '#remove_span_processor' do
    # Records the callbacks it receives so tests can assert on delivery order.
    class RecordingSpanProcessor
      def initialize(events)
        @events = events
      end

      def on_start(span, _parent_context)
        @events << [:on_start, span.name]
      end

      def on_finishing(span)
        @events << [:on_finishing, span.name]
      end

      def on_finish(span)
        @events << [:on_finish, span.name]
      end

      def force_flush(timeout: nil)
        OpenTelemetry::SDK::Trace::Export::SUCCESS
      end

      def shutdown(timeout: nil)
        @events << [:shutdown]
        OpenTelemetry::SDK::Trace::Export::SUCCESS
      end
    end

    let(:events) { [] }
    let(:recording_processor) { RecordingSpanProcessor.new(events) }
    let(:tracer) { tracer_provider.tracer('remove-processor-test', '1.0') }

    it 'returns nil for a processor that was never added' do
      _(tracer_provider.remove_span_processor(Object.new)).must_be_nil
    end

    it 'delivers paired callbacks for overlapping spans before shutting down' do
      tracer_provider.add_span_processor(recording_processor)
      first = tracer.start_span('first')
      second = tracer.start_span('second')

      registration = tracer_provider.remove_span_processor(recording_processor)

      # The processor is not shut down while spans it already saw are still
      # running.
      _(registration.done?).must_equal(false)

      second.finish
      first.finish

      _(registration.wait).must_equal(OpenTelemetry::SDK::Trace::Export::SUCCESS)
      _(events).must_equal(
        [
          [:on_start, 'first'],
          [:on_start, 'second'],
          [:on_finishing, 'second'],
          [:on_finish, 'second'],
          [:on_finishing, 'first'],
          [:on_finish, 'first'],
          [:shutdown]
        ]
      )
    end

    it 'delivers on_finish only after on_start for a span that straddles removal' do
      tracer_provider.add_span_processor(recording_processor)
      span = tracer.start_span('straddling')

      registration = tracer_provider.remove_span_processor(recording_processor)
      span.finish

      _(registration.wait).must_equal(OpenTelemetry::SDK::Trace::Export::SUCCESS)
      _(events).must_equal(
        [
          [:on_start, 'straddling'],
          [:on_finishing, 'straddling'],
          [:on_finish, 'straddling'],
          [:shutdown]
        ]
      )
    end

    it 'does not deliver spans started after removal' do
      tracer_provider.add_span_processor(recording_processor)
      tracer.in_span('before') {}
      tracer_provider.remove_span_processor(recording_processor).wait
      tracer.in_span('after') {}

      _(events).must_equal(
        [
          [:on_start, 'before'],
          [:on_finishing, 'before'],
          [:on_finish, 'before'],
          [:shutdown]
        ]
      )
    end

    it 'does not shut down before the in-flight spans finish' do
      tracer_provider.add_span_processor(recording_processor)
      span = tracer.start_span('in-flight')

      registration = tracer_provider.remove_span_processor(recording_processor)

      _(events).wont_include([:shutdown])
      _(registration.wait(timeout: 0.01)).must_equal(OpenTelemetry::SDK::Trace::Export::TIMEOUT)

      span.finish

      _(registration.wait(timeout: 1)).must_equal(OpenTelemetry::SDK::Trace::Export::SUCCESS)
      _(events.last).must_equal([:shutdown])
      _(events.map(&:first).count(:on_finish)).must_equal(1)
      _(events.index([:shutdown])).must_be :>, events.index([:on_finish, 'in-flight'])
    end

    it 'shuts down exactly once per registration' do
      tracer_provider.add_span_processor(recording_processor)
      tracer.in_span('span') {}

      tracer_provider.remove_span_processor(recording_processor).wait
      tracer_provider.remove_span_processor(recording_processor)

      _(events.count([:shutdown])).must_equal(1)
    end

    it 'registers a processor at most once' do
      first = tracer_provider.add_span_processor(recording_processor)
      second = tracer_provider.add_span_processor(recording_processor)

      tracer.in_span('span') {}

      _(second).must_equal(first)
      _(events).must_equal(
        [
          [:on_start, 'span'],
          [:on_finishing, 'span'],
          [:on_finish, 'span']
        ]
      )
    end

    it 'still delivers the pair when the processor raises during on_finish' do
      events = []
      raising = Object.new
      raising.define_singleton_method(:on_start) { |span, _| events << [:on_start, span.name] }
      raising.define_singleton_method(:on_finish) { |span| events << [:on_finish, span.name]; raise 'boom' }
      raising.define_singleton_method(:shutdown) { |timeout: nil| events << [:shutdown]; OpenTelemetry::SDK::Trace::Export::SUCCESS }
      raising.define_singleton_method(:force_flush) { |timeout: nil| OpenTelemetry::SDK::Trace::Export::SUCCESS }

      tracer_provider.add_span_processor(raising)
      span = tracer.start_span('raising')
      registration = tracer_provider.remove_span_processor(raising)

      _ { span.finish }.must_raise(RuntimeError)

      _(registration.wait(timeout: 1)).must_equal(OpenTelemetry::SDK::Trace::Export::SUCCESS)
      _(events).must_equal([[:on_start, 'raising'], [:on_finish, 'raising'], [:shutdown]])
    end

    it 'does not wait forever on a span whose start failed' do
      events = []
      failing = Object.new
      failing.define_singleton_method(:on_start) { |_span, _ctx| raise 'boom' }
      failing.define_singleton_method(:on_finish) { |_span| events << [:on_finish] }
      failing.define_singleton_method(:shutdown) { |timeout: nil| events << [:shutdown]; OpenTelemetry::SDK::Trace::Export::SUCCESS }
      failing.define_singleton_method(:force_flush) { |timeout: nil| OpenTelemetry::SDK::Trace::Export::SUCCESS }

      tracer_provider.add_span_processor(failing)
      _ { tracer.start_span('failing') }.must_raise(RuntimeError)
      registration = tracer_provider.remove_span_processor(failing)

      _(registration.wait(timeout: 1)).must_equal(OpenTelemetry::SDK::Trace::Export::SUCCESS)
      _(events).must_equal([[:shutdown]])
    end

    it 'blocks provider shutdown until in-flight spans have been delivered' do
      registration = tracer_provider.add_span_processor(recording_processor)
      span = tracer.start_span('in-flight')

      shutdown_thread = Thread.new { tracer_provider.shutdown }
      # Let the shutdown thread block waiting for the span without assuming a
      # fixed delay.
      deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 5
      Thread.pass while shutdown_thread.alive? &&
                        shutdown_thread.status != 'sleep' &&
                        Process.clock_gettime(Process::CLOCK_MONOTONIC) < deadline

      _(registration.done?).must_equal(false)
      _(events).wont_include([:shutdown])

      span.finish
      shutdown_thread.join

      _(events.index([:shutdown])).must_be :>, events.index([:on_finish, 'in-flight'])
    end
  end

  describe '#tracer' do
    # Legacy positional calling conventions
    it 'returns the same tracer for the same arguments' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        tracer1 = tracer_provider.tracer('component', '1.0')
        tracer2 = tracer_provider.tracer('component', '1.0')
        _(tracer1).must_equal(tracer2)
        _(log_stream.string).must_be_empty
      end
    end

    it 'defaults version to empty string when given positional name only' do
      tracer = tracer_provider.tracer('component')
      _(tracer).wont_be_nil
    end

    it 'returns different tracers for different names' do
      tracer1 = tracer_provider.tracer('component1', '1.0')
      tracer2 = tracer_provider.tracer('component2', '1.0')
      _(tracer1).wont_equal(tracer2)
    end

    it 'returns different tracers for different versions' do
      tracer1 = tracer_provider.tracer('component', '1.0')
      tracer2 = tracer_provider.tracer('component', '2.0')
      _(tracer1).wont_equal(tracer2)
    end

    it 'warns when no name is passed for the tracer' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        tracer_provider.tracer
        _(log_stream.string).must_match(/calling TracerProvider#tracer without providing a tracer name./)
      end
    end

    # Keyword calling conventions
    it 'accepts all keyword arguments' do
      tracer = tracer_provider.tracer(name: 'component', version: '1.0', attributes: { 'key' => 'value' })
      _(tracer).wont_be_nil
    end

    it 'returns the same tracer for equivalent positional and keyword arguments' do
      tracer1 = tracer_provider.tracer('component', '1.0')
      tracer2 = tracer_provider.tracer(name: 'component', version: '1.0')
      _(tracer1).must_equal(tracer2)
    end

    # Mixed positional + keyword
    it 'accepts positional name with keyword version' do
      tracer = tracer_provider.tracer('component', version: '1.0')
      _(tracer).wont_be_nil
    end

    it 'prefers keyword arguments over positional arguments' do
      tracer1 = tracer_provider.tracer('positional', '1.0')
      tracer2 = tracer_provider.tracer('positional', '1.0', name: 'keyword', version: '2.0')
      _(tracer1).wont_equal(tracer2)
    end

    # Attributes
    it 'returns the same tracer for the same name, version, and attributes' do
      tracer1 = tracer_provider.tracer('component', '1.0', attributes: { 'key' => 'value' })
      tracer2 = tracer_provider.tracer('component', '1.0', attributes: { 'key' => 'value' })
      _(tracer1).must_equal(tracer2)
    end

    it 'returns different tracers for different attributes' do
      tracer1 = tracer_provider.tracer('component', '1.0', attributes: { 'key' => 'value1' })
      tracer2 = tracer_provider.tracer('component', '1.0', attributes: { 'key' => 'value2' })
      _(tracer1).wont_equal(tracer2)
    end

    it 'returns different tracers for attributes vs no attributes' do
      tracer1 = tracer_provider.tracer('component', '1.0')
      tracer2 = tracer_provider.tracer('component', '1.0', attributes: { 'key' => 'value' })
      _(tracer1).wont_equal(tracer2)
    end

    it 'treats nil attributes the same as no attributes' do
      tracer1 = tracer_provider.tracer('component', '1.0')
      tracer2 = tracer_provider.tracer('component', '1.0', attributes: nil)
      _(tracer1).must_equal(tracer2)
    end

    it 'does not allow mutation of attributes after tracer creation' do
      attrs = { 'key' => 'value' }
      tracer_provider.tracer('component', '1.0', attributes: attrs)
      attrs['key'] = 'mutated'
      tracer = tracer_provider.tracer('component', '1.0', attributes: { 'key' => 'value' })
      _(tracer).wont_be_nil
    end
  end
end
