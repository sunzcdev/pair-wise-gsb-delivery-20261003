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

    it 'deep copies array-valued attributes so caller mutations cannot change the registered scope' do
      values = ['a', 'b']
      attrs = { 'key' => values }
      tracer = tracer_provider.tracer('component', '1.0', attributes: attrs)

      values << 'mutated'

      scope_attributes = tracer.start_root_span('test').instrumentation_scope.attributes
      expected_attributes = { 'key' => ['a', 'b'] }
      _(scope_attributes).must_equal(expected_attributes)
      _(scope_attributes).wont_equal(attrs)
    end

    it 'deep copies string keys and scalar values so caller mutations cannot change the registered scope' do
      key = +'key'
      value = +'value'
      tracer = tracer_provider.tracer('component', '1.0', attributes: { key => value })

      key << '-mutated'
      value << '-mutated'

      scope_attributes = tracer.start_root_span('test').instrumentation_scope.attributes
      expected_attributes = { 'key' => 'value' }
      _(scope_attributes).must_equal(expected_attributes)
    end

    it 'freezes the registered attributes hash, its keys, and its array values' do
      tracer = tracer_provider.tracer('component', '1.0', attributes: { 'key' => ['a', 'b'] })
      attributes = tracer.start_root_span('test').instrumentation_scope.attributes

      _(attributes).must_be :frozen?
      _(attributes.keys.first).must_be :frozen?
      _(attributes.values.first).must_be :frozen?
      _(attributes.values.first.first).must_be :frozen?
    end

    it 'shares one deep-frozen value between the registry key and the exported scope' do
      tracer = tracer_provider.tracer('component', '1.0', attributes: { 'key' => ['a', 'b'] })
      registry_key = tracer_provider.instance_variable_get(:@registry).keys.last
      scope_attributes = tracer.start_root_span('test').instrumentation_scope.attributes

      _(registry_key.attributes).must_equal(scope_attributes)
      assert_same(registry_key.attributes, scope_attributes)
    end

    it 'reuses the tracer when attributes are equal regardless of hash insertion order' do
      tracer1 = tracer_provider.tracer('component', '1.0', attributes: { 'a' => '1', 'b' => '2' })
      tracer2 = tracer_provider.tracer('component', '1.0', attributes: { 'b' => '2', 'a' => '1' })

      _(tracer1).must_equal(tracer2)
    end

    it 'returns different tracers when array element order differs' do
      tracer1 = tracer_provider.tracer('component', '1.0', attributes: { 'key' => ['a', 'b'] })
      tracer2 = tracer_provider.tracer('component', '1.0', attributes: { 'key' => ['b', 'a'] })

      _(tracer1).wont_equal(tracer2)
    end

    it 'keeps the originally exported scope stable and creates a separate tracer for the mutated value' do
      exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
      tracer_provider.add_span_processor(OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter))

      values = ['a', 'b']
      original_tracer = tracer_provider.tracer('component', '1.0', attributes: { 'key' => values })
      original_tracer.in_span('before-mutation') {}

      values << 'c'

      old_value_tracer = tracer_provider.tracer('component', '1.0', attributes: { 'key' => ['a', 'b'] })
      old_value_tracer.in_span('old-value') {}

      new_value_tracer = tracer_provider.tracer('component', '1.0', attributes: { 'key' => ['a', 'b', 'c'] })
      new_value_tracer.in_span('new-value') {}

      _(old_value_tracer).must_equal(original_tracer)
      _(new_value_tracer).wont_equal(original_tracer)

      spans = exporter.finished_spans
      expected_names = %w[before-mutation old-value new-value]
      _(spans.map(&:name)).must_equal(expected_names)

      old_attributes = { 'key' => ['a', 'b'] }
      new_attributes = { 'key' => ['a', 'b', 'c'] }
      _(spans[0].instrumentation_scope.attributes).must_equal(old_attributes)
      _(spans[1].instrumentation_scope.attributes).must_equal(old_attributes)
      _(spans[2].instrumentation_scope.attributes).must_equal(new_attributes)
    end
  end
end
