# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::Context::Propagation::CompositeTextMapPropagator do
  class TestInjector
    def initialize(key)
      @key = key
    end

    def inject(carrier, context:, setter: nil)
      if setter
        setter.set(carrier, @key, context[@key])
      else
        carrier[@key] = context[@key]
      end
      carrier
    end

    def fields
      [@key]
    end
  end

  class TestExtractor
    def initialize(key)
      @key = key
    end

    def extract(carrier, context:, getter: nil)
      value = getter ? getter.get(carrier, @key) : carrier[@key]
      context.set_value(@key, value)
    end
  end

  class BuggyInjector
    def inject(carrier, context:, setter: nil)
      raise 'oops'
    end
  end

  class BuggyExtractor
    def extract(carrier, context:, getter: nil)
      raise 'oops'
    end
  end

  let(:propagator) do
    OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose(injectors: injectors, extractors: extractors)
  end

  after do
    Context.clear
  end

  describe 'with working injectors / extractors' do
    let(:injectors) { %w[k1 k2 k3].map { |k| TestInjector.new(k) } }
    let(:extractors) { %w[k1 k2 k3].map { |k| TestExtractor.new(k) } }

    describe '#inject' do
      it 'injects values from current context into carrier' do
        Context.with_values('k1' => 'v1', 'k2' => 'v2', 'k3' => 'v3') do
          carrier = {}
          propagator.inject(carrier)
          _(carrier).must_equal({ 'k1' => 'v1', 'k2' => 'v2', 'k3' => 'v3' })
        end
      end

      it 'accepts explicit context' do
        Context.with_values('k1' => 'v1', 'k2' => 'v2') do
          ctx = Context.current.set_value('k3', 'v3')
          carrier = {}
          propagator.inject(carrier, context: ctx)
          _(carrier).must_equal({ 'k1' => 'v1', 'k2' => 'v2', 'k3' => 'v3' })
        end
      end

      it 'executes setter' do
        Context.with_values('k1' => 'v1', 'k2' => 'v2', 'k3' => 'v3') do
          setter = Class.new do
            def set(carrier, key, value)
              carrier[key.upcase] = value.upcase
            end
          end.new
          result = {}
          propagator.inject(result, setter: setter)
          _(result).must_equal({ 'K1' => 'V1', 'K2' => 'V2', 'K3' => 'V3' })
        end
      end
    end

    describe '#extract' do
      it 'extracts values from carrier into context' do
        carrier = { 'k1' => 'v1', 'k2' => 'v2', 'k3' => 'v3' }
        context = propagator.extract(carrier)
        _(context['k1']).must_equal('v1')
        _(context['k2']).must_equal('v2')
        _(context['k3']).must_equal('v3')
      end

      it 'accepts explicit context' do
        ctx = Context.empty.set_value('k0', 'v0')
        carrier = { 'k1' => 'v1', 'k2' => 'v2', 'k3' => 'v3' }
        context = propagator.extract(carrier, context: ctx)
        _(context['k0']).must_equal('v0')
        _(context['k1']).must_equal('v1')
        _(context['k2']).must_equal('v2')
        _(context['k3']).must_equal('v3')
      end

      it 'executes getter' do
        carrier = { 'k1' => 'v1', 'k2' => 'v2', 'k3' => 'v3' }
        getter = Class.new do
          def get(carrier, key)
            carrier[key]&.upcase
          end
        end.new
        context = propagator.extract(carrier, getter: getter)
        _(context['k1']).must_equal('V1')
        _(context['k2']).must_equal('V2')
        _(context['k3']).must_equal('V3')
      end
    end

    describe '#fields' do
      it 'returns all fields from injectors' do
        _(propagator.fields).must_equal(%w[k1 k2 k3])
      end
    end
  end

  describe 'with buggy injectors / extractors' do
    let(:injectors) do
      %w[k1 k2 k3].map do |k|
        k == 'k2' ? BuggyInjector.new : TestInjector.new(k)
      end
    end
    let(:extractors) do
      %w[k1 k2 k3].map do |k|
        k == 'k2' ? BuggyExtractor.new : TestExtractor.new(k)
      end
    end

    describe '#inject' do
      it 'injects values from working injectors' do
        Context.with_values('k1' => 'v1', 'k2' => 'v2', 'k3' => 'v3') do
          carrier = {}
          propagator.inject(carrier)
          _(carrier).must_equal({ 'k1' => 'v1', 'k3' => 'v3' })
        end
      end
    end

    describe '#extract' do
      it 'extracts values from working extractors' do
        carrier = { 'k1' => 'v1', 'k2' => 'v2', 'k3' => 'v3' }
        context = propagator.extract(carrier)
        _(context['k1']).must_equal('v1')
        _(context['k2']).must_be_nil
        _(context['k3']).must_equal('v3')
      end
    end
  end

  describe 'with competing trace context providers' do
    # A minimal propagator that recognizes a single header with a pair of hex
    # identifiers, so a carrier can carry two independent tracing header
    # families and each family can be inspected in isolation.
    class HexTracePropagator
      attr_reader :key

      def initialize(key, tracestate_key: nil, trace_flags: OpenTelemetry::Trace::TraceFlags::SAMPLED)
        @key = key
        @tracestate_key = tracestate_key
        @trace_flags = trace_flags
      end

      def inject(carrier, context:, setter: nil)
        span_context = OpenTelemetry::Trace.current_span(context).context
        return unless span_context.valid?

        write(carrier, @key, "#{span_context.hex_trace_id}-#{span_context.hex_span_id}", setter)
        return unless @tracestate_key && !span_context.tracestate.empty?

        write(carrier, @tracestate_key, span_context.tracestate.to_s, setter)
        nil
      end

      def extract(carrier, context: OpenTelemetry::Context.current, getter: OpenTelemetry::Context::Propagation.text_map_getter)
        raw = getter.get(carrier, @key)
        return context if raw.nil?

        trace_id, span_id = raw.split('-', 2)
        span_context = OpenTelemetry::Trace::SpanContext.new(
          trace_id: trace_id.nil? || trace_id.empty? ? OpenTelemetry::Trace::INVALID_TRACE_ID : Array(trace_id).pack('H*'),
          span_id: span_id.nil? || span_id.empty? ? OpenTelemetry::Trace::INVALID_SPAN_ID : Array(span_id).pack('H*'),
          trace_flags: @trace_flags,
          tracestate: OpenTelemetry::Trace::Tracestate.from_string(getter.get(carrier, @tracestate_key)),
          remote: true
        )
        OpenTelemetry::Trace.context_with_span(OpenTelemetry::Trace.non_recording_span(span_context), parent_context: context)
      end

      def fields
        [@key, @tracestate_key].compact
      end

      private

      def write(carrier, key, value, setter)
        if setter
          setter.set(carrier, key, value)
        else
          carrier[key] = value
        end
      end
    end

    let(:first) { HexTracePropagator.new('traceparent-a', tracestate_key: 'tracestate-a') }
    let(:second) { HexTracePropagator.new('traceparent-b', tracestate_key: 'tracestate-b', trace_flags: OpenTelemetry::Trace::TraceFlags::DEFAULT) }
    let(:baggage) { OpenTelemetry::Baggage::Propagation.text_map_propagator }
    let(:trace_id) { '0af7651916cd43dd8448eb211c80319c' }
    let(:other_trace_id) { 'b7ad6b7169203331f0d0f1b3b0b2c1e4' }
    let(:span_id) { 'b7ad6b7169203331' }
    let(:other_span_id) { '00f067aa0ba902b7' }

    let(:both_valid_carrier) do
      {
        'traceparent-a' => "#{trace_id}-#{span_id}",
        'tracestate-a' => 'vendor-a=a',
        'traceparent-b' => "#{other_trace_id}-#{other_span_id}",
        'tracestate-b' => 'vendor-b=b',
        'baggage' => 'user=alice'
      }
    end

    let(:partial_second_span_id_carrier) do
      {
        'traceparent-a' => "#{trace_id}-#{span_id}",
        'tracestate-a' => 'vendor-a=a',
        'traceparent-b' => "#{other_trace_id}-",
        'tracestate-b' => 'vendor-b=b',
        'baggage' => 'user=alice'
      }
    end

    let(:partial_first_span_id_carrier) do
      {
        'traceparent-a' => "#{trace_id}-",
        'tracestate-a' => 'vendor-a=a',
        'traceparent-b' => "#{other_trace_id}-#{other_span_id}",
        'tracestate-b' => 'vendor-b=b',
        'baggage' => 'user=alice'
      }
    end

    let(:second_partial_carrier) do
      {
        'traceparent-b' => "#{other_trace_id}-",
        'tracestate-b' => 'vendor-b=b',
        'baggage' => 'user=alice'
      }
    end

    after do
      Context.clear
    end

    describe '#candidates' do
      it 'reports each propagator separately, keeping flags and tracestate with their own family' do
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators([first, second, baggage])
        candidates = composite.candidates(both_valid_carrier, context: Context.empty)

        _(candidates.map(&:state)).must_equal(%i[valid valid missing])
        _(candidates[0].propagator).must_be_same_as(first)
        _(candidates[0].trace_id).must_equal(Array(trace_id).pack('H*'))
        _(candidates[0].span_id).must_equal(Array(span_id).pack('H*'))
        _(candidates[0].trace_flags).must_equal(OpenTelemetry::Trace::TraceFlags::SAMPLED)
        _(candidates[0].tracestate.to_s).must_equal('vendor-a=a')

        _(candidates[1].propagator).must_be_same_as(second)
        _(candidates[1].trace_id).must_equal(Array(other_trace_id).pack('H*'))
        _(candidates[1].trace_flags).must_equal(OpenTelemetry::Trace::TraceFlags::DEFAULT)
        _(candidates[1].tracestate.to_s).must_equal('vendor-b=b')

        _(candidates[2]).must_be(:missing?)
      end

      it 'distinguishes a partial candidate from a missing one' do
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators([first, second])
        candidates = composite.candidates(second_partial_carrier, context: Context.empty)

        _(candidates[0]).must_be(:missing?)
        _(candidates[1]).must_be(:partial?)
        _(candidates[1]).wont_be(:valid?)
        _(candidates[1].trace_id).must_equal(Array(other_trace_id).pack('H*'))
      end

      it 'orders candidates by the explicit precedence, not composition order' do
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators(
          [first, second],
          precedence: [second]
        )
        candidates = composite.candidates(both_valid_carrier, context: Context.empty)

        _(candidates.first.propagator).must_be_same_as(second)
        _(candidates.last.propagator).must_be_same_as(first)
      end

      it 'accepts propagator classes as precedence entries' do
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators(
          [first, second],
          precedence: [HexTracePropagator]
        )
        candidates = composite.candidates(second_partial_carrier, context: Context.empty)
        _(candidates.map(&:propagator)).must_equal([first, second])
      end

      it 'distinguishes a malformed header from a missing one using the propagator own classification' do
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators(
          [OpenTelemetry::Trace::Propagation::TraceContext.text_map_propagator, first]
        )
        candidates = composite.candidates({ 'traceparent' => 'FF-000000000000000000000000000000AA-00000000000000ea-01' }, context: Context.empty)

        _(candidates[0]).must_be(:invalid?)
        _(candidates[0].propagator).must_be_same_as(OpenTelemetry::Trace::Propagation::TraceContext.text_map_propagator)
        _(candidates[1]).must_be(:missing?)
      end
    end

    describe '#extract with two complete header families' do
      it 'adopts the last complete provider by default, keeping its flags and tracestate together' do
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators([first, second])
        context = composite.extract(both_valid_carrier, context: Context.empty)
        span_context = OpenTelemetry::Trace.current_span(context).context

        _(span_context.hex_trace_id).must_equal(other_trace_id)
        _(span_context.hex_span_id).must_equal(other_span_id)
        _(span_context.trace_flags).must_equal(OpenTelemetry::Trace::TraceFlags::DEFAULT)
        _(span_context.tracestate.to_s).must_equal('vendor-b=b')
        _(span_context).must_be(:remote?)
      end

      it 'keeps the parent identifiers, flags and tracestate from one provider only' do
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators([first, second])
        context = composite.extract(both_valid_carrier, context: Context.empty)
        span_context = OpenTelemetry::Trace.current_span(context).context

        # The adopted trace ID and span ID must belong to the same header family
        # as the adopted tracestate and flags.
        _(span_context.hex_trace_id).must_equal(other_trace_id)
        _(span_context.hex_span_id).must_equal(other_span_id)
        _(span_context.tracestate.value('vendor-b')).must_equal('b')
        _(span_context.tracestate.value('vendor-a')).must_be_nil
      end

      it 'adopts the explicitly preferred provider, without borrowing the other provider fields' do
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators(
          [first, second],
          precedence: [first]
        )
        context = composite.extract(both_valid_carrier, context: Context.empty)
        span_context = OpenTelemetry::Trace.current_span(context).context

        _(span_context.hex_trace_id).must_equal(trace_id)
        _(span_context.hex_span_id).must_equal(span_id)
        _(span_context.trace_flags).must_equal(OpenTelemetry::Trace::TraceFlags::SAMPLED)
        _(span_context.tracestate.to_s).must_equal('vendor-a=a')
      end

      it 'keeps baggage extraction independent of which trace context wins' do
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators(
          [first, second, baggage],
          precedence: [second]
        )
        context = composite.extract(both_valid_carrier, context: Context.empty)

        _(OpenTelemetry::Baggage.values(context: context)).must_equal({ 'user' => 'alice' })
        _(OpenTelemetry::Trace.current_span(context).context.hex_trace_id).must_equal(other_trace_id)
      end

      it 're-injects only the adopted parent, dropping the other family identifiers' do
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators(
          [first, second],
          precedence: [second]
        )
        context = composite.extract(both_valid_carrier, context: Context.empty)
        carrier = {}
        composite.inject(carrier, context: context)

        _(carrier['traceparent-a']).must_equal("#{other_trace_id}-#{other_span_id}")
        _(carrier['tracestate-a']).must_equal('vendor-b=b')
        _(carrier['traceparent-b']).must_equal("#{other_trace_id}-#{other_span_id}")
        _(carrier['tracestate-b']).must_equal('vendor-b=b')
      end
      it 'keeps the complete family when a later extractor only fills in part' do
        # A custom extractor that emits only a trace ID must not replace the
        # complete parent that an earlier extractor produced.
        partial_extractor = Class.new do
          def initialize(trace_id)
            @trace_id = trace_id
          end

          def extract(carrier, context:, getter: nil)
            span_context = OpenTelemetry::Trace::SpanContext.new(
              trace_id: Array(@trace_id).pack('H*'),
              span_id: OpenTelemetry::Trace::INVALID_SPAN_ID,
              remote: true
            )
            OpenTelemetry::Trace.context_with_span(OpenTelemetry::Trace.non_recording_span(span_context), parent_context: context)
          end
        end.new('b7ad6b7169203331f0d0f1b3b0b2c1e4')

        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose(
          injectors: [],
          extractors: [first, partial_extractor]
        )
        context = composite.extract(both_valid_carrier, context: Context.empty)
        span_context = OpenTelemetry::Trace.current_span(context).context

        _(span_context.hex_trace_id).must_equal(trace_id)
        _(span_context.hex_span_id).must_equal(span_id)
        _(composite.candidates(both_valid_carrier, context: Context.empty).map(&:state)).must_equal(%i[valid partial])
      end
    end

    describe '#extract when only one header family is complete' do
      it 'adopts the second complete family when the first is only partial' do
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators([first, second])
        context = composite.extract(partial_first_span_id_carrier, context: Context.empty)
        span_context = OpenTelemetry::Trace.current_span(context).context

        _(span_context.hex_trace_id).must_equal(other_trace_id)
        _(span_context.hex_span_id).must_equal(other_span_id)
        _(span_context.trace_flags).must_equal(OpenTelemetry::Trace::TraceFlags::DEFAULT)
        _(span_context.tracestate.to_s).must_equal('vendor-b=b')
      end

      it 'adopts the first complete family when the second is only partial' do
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators([first, second])
        context = composite.extract(partial_second_span_id_carrier, context: Context.empty)
        span_context = OpenTelemetry::Trace.current_span(context).context

        _(span_context.hex_trace_id).must_equal(trace_id)
        _(span_context.hex_span_id).must_equal(span_id)
        _(span_context.trace_flags).must_equal(OpenTelemetry::Trace::TraceFlags::SAMPLED)
        _(span_context.tracestate.to_s).must_equal('vendor-a=a')
      end

      it 'does not splice a partial candidate into a missing one' do
        # The preferred propagator contributes nothing, the second contributes
        # only a trace ID. The composite must not combine the two.
        partial = HexTracePropagator.new('traceparent-b')
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators(
          [first, partial],
          precedence: [first]
        )
        context = composite.extract({ 'traceparent-b' => "#{other_trace_id}-" }, context: Context.empty)
        span_context = OpenTelemetry::Trace.current_span(context).context

        _(span_context.valid?).must_equal(false)
      end

      it 'leaves the incoming context span untouched when no family is complete' do
        base_span_context = OpenTelemetry::Trace::SpanContext.new(
          trace_id: Array(trace_id).pack('H*'),
          span_id: Array(span_id).pack('H*')
        )
        base_context = OpenTelemetry::Trace.context_with_span(
          OpenTelemetry::Trace.non_recording_span(base_span_context),
          parent_context: Context.empty
        )
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators([first, second, baggage])
        context = composite.extract(second_partial_carrier, context: base_context)

        _(OpenTelemetry::Trace.current_span(context).context).must_be_same_as(base_span_context)
        _(OpenTelemetry::Baggage.values(context: context)).must_equal({ 'user' => 'alice' })
      end

      it 're-injects the inherited parent when the only family is partial' do
        base_span_context = OpenTelemetry::Trace::SpanContext.new(
          trace_id: Array(trace_id).pack('H*'),
          span_id: Array(span_id).pack('H*')
        )
        base_context = OpenTelemetry::Trace.context_with_span(
          OpenTelemetry::Trace.non_recording_span(base_span_context),
          parent_context: Context.empty
        )
        composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator.compose_propagators([first, second])
        context = composite.extract(second_partial_carrier, context: base_context)
        carrier = {}
        composite.inject(carrier, context: context)

        _(carrier['traceparent-a']).must_equal("#{trace_id}-#{span_id}")
      end
    end
  end
end
