# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

# Exercises the composite propagator through the SDK: a carrier can arrive with
# two tracing header families, and both the parent of the new span and the
# values written on the next hop must come from a single, explicitly preferred
# provider.
describe OpenTelemetry::SDK, 'composite propagation' do
  Composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator
  TraceContext = OpenTelemetry::Trace::Propagation::TraceContext
  BaggagePropagator = OpenTelemetry::Baggage::Propagation

  let(:sdk) { OpenTelemetry::SDK }
  let(:exporter) { sdk::Trace::Export::InMemorySpanExporter.new }
  let(:provider) do
    sdk::Trace::TracerProvider.new.tap do |provider|
      provider.add_span_processor(sdk::Trace::Export::SimpleSpanProcessor.new(exporter))
    end
  end
  let(:tracer) { provider.tracer(__FILE__, sdk::VERSION) }

  # A second trace context format, deliberately kept simple: a single header
  # carrying "<trace_id>-<span_id>-<sampled>" plus its own tracestate header.
  class FlatTracePropagator
    IDENTITY_KEY = 'flat-trace'
    TRACESTATE_KEY = 'flat-tracestate'

    def inject(carrier, context: OpenTelemetry::Context.current, setter: OpenTelemetry::Context::Propagation.text_map_setter)
      span_context = OpenTelemetry::Trace.current_span(context).context
      return unless span_context.valid?

      sampled = span_context.trace_flags.sampled? ? '1' : '0'
      setter.set(carrier, IDENTITY_KEY, "#{span_context.hex_trace_id}-#{span_context.hex_span_id}-#{sampled}")
      setter.set(carrier, TRACESTATE_KEY, span_context.tracestate.to_s) unless span_context.tracestate.empty?
      nil
    end

    def extract(carrier, context: OpenTelemetry::Context.current, getter: OpenTelemetry::Context::Propagation.text_map_getter)
      raw = getter.get(carrier, IDENTITY_KEY)
      return context if raw.nil?

      trace_id, span_id, sampled = raw.split('-', 3)
      span_context = OpenTelemetry::Trace::SpanContext.new(
        trace_id: trace_id.nil? || trace_id.empty? ? OpenTelemetry::Trace::INVALID_TRACE_ID : Array(trace_id).pack('H*'),
        span_id: span_id.nil? || span_id.empty? ? OpenTelemetry::Trace::INVALID_SPAN_ID : Array(span_id).pack('H*'),
        trace_flags: sampled == '1' ? OpenTelemetry::Trace::TraceFlags::SAMPLED : OpenTelemetry::Trace::TraceFlags::DEFAULT,
        tracestate: OpenTelemetry::Trace::Tracestate.from_string(getter.get(carrier, TRACESTATE_KEY)),
        remote: true
      )
      OpenTelemetry::Trace.context_with_span(OpenTelemetry::Trace.non_recording_span(span_context), parent_context: context)
    end

    def fields
      [IDENTITY_KEY, TRACESTATE_KEY]
    end
  end

  let(:flat) { FlatTracePropagator.new }
  let(:w3c) { TraceContext.text_map_propagator }
  let(:baggage) { BaggagePropagator.text_map_propagator }

  let(:w3c_trace_id) { '0af7651916cd43dd8448eb211c80319c' }
  let(:w3c_span_id) { 'b7ad6b7169203331' }
  let(:flat_trace_id) { 'b7ad6b7169203331f0d0f1b3b0b2c1e4' }
  let(:flat_span_id) { '00f067aa0ba902b7' }

  after do
    OpenTelemetry::Context.clear
  end

  def child_span(carrier, composite)
    parent_context = composite.extract(carrier, context: OpenTelemetry::Context.empty)
    OpenTelemetry::Context.with_current(parent_context) do
      tracer.in_span('child') {}
    end
    exporter.finished_spans.first
  end

  def reinject(span_data, composite)
    span_context = OpenTelemetry::Trace::SpanContext.new(
      trace_id: span_data.trace_id,
      span_id: span_data.span_id,
      trace_flags: span_data.trace_flags,
      tracestate: span_data.tracestate
    )
    context = OpenTelemetry::Trace.context_with_span(OpenTelemetry::Trace.non_recording_span(span_context), parent_context: OpenTelemetry::Context.empty)
    carrier = {}
    composite.inject(carrier, context: context)
    carrier
  end

  describe 'both formats carry a complete trace context' do
    let(:carrier) do
      {
        'traceparent' => "00-#{w3c_trace_id}-#{w3c_span_id}-01",
        'tracestate' => 'vendor-w3c=one',
        FlatTracePropagator::IDENTITY_KEY => "#{flat_trace_id}-#{flat_span_id}-1",
        FlatTracePropagator::TRACESTATE_KEY => 'vendor-flat=two',
        'baggage' => 'user=alice'
      }
    end

    it 'parents the new span on the last complete provider by default' do
      composite = Composite.compose_propagators([w3c, flat, baggage])
      span_data = child_span(carrier, composite)

      _(span_data.hex_trace_id).must_equal(flat_trace_id)
      _(span_data.parent_span_id.unpack1('H*')).must_equal(flat_span_id)
      _(span_data.parent_span_is_remote).must_equal(true)
      _(span_data.tracestate.to_s).must_equal('vendor-flat=two')
      _(span_data.trace_flags).must_be(:sampled?)
    end

    it 'parents the new span on the explicitly preferred provider instead' do
      composite = Composite.compose_propagators([w3c, flat, baggage], precedence: [w3c])
      span_data = child_span(carrier, composite)

      _(span_data.hex_trace_id).must_equal(w3c_trace_id)
      _(span_data.parent_span_id.unpack1('H*')).must_equal(w3c_span_id)
      _(span_data.tracestate.to_s).must_equal('vendor-w3c=one')
      _(span_data.trace_flags).must_be(:sampled?)
    end

    it 're-injects the parent identifiers, flags and tracestate of the adopted provider' do
      composite = Composite.compose_propagators([w3c, flat, baggage], precedence: [flat])
      span_data = child_span(carrier, composite)
      next_hop = reinject(span_data, composite)

      _(next_hop['traceparent']).must_equal("00-#{flat_trace_id}-#{span_data.hex_span_id}-01")
      _(next_hop['tracestate']).must_equal('vendor-flat=two')
      _(next_hop[FlatTracePropagator::IDENTITY_KEY]).must_equal("#{flat_trace_id}-#{span_data.hex_span_id}-1")
      _(next_hop[FlatTracePropagator::TRACESTATE_KEY]).must_equal('vendor-flat=two')
    end

    it 'keeps baggage independent of the selected trace context' do
      composite = Composite.compose_propagators([w3c, flat, baggage], precedence: [flat])
      extracted = composite.extract(carrier, context: OpenTelemetry::Context.empty)

      _(OpenTelemetry::Baggage.values(context: extracted)).must_equal({ 'user' => 'alice' })
      _(OpenTelemetry::Trace.current_span(extracted).context.hex_trace_id).must_equal(flat_trace_id)
    end
  end

  describe 'only one format carries a complete trace context' do
    let(:carrier) do
      {
        'traceparent' => "00-#{w3c_trace_id}-#{w3c_span_id}-01",
        'tracestate' => 'vendor-w3c=one',
        FlatTracePropagator::IDENTITY_KEY => "#{flat_trace_id}-",
        FlatTracePropagator::TRACESTATE_KEY => 'vendor-flat=two',
        'baggage' => 'user=alice'
      }
    end

    it 'adopts the only complete provider, ignoring the preferred partial one' do
      composite = Composite.compose_propagators([w3c, flat, baggage], precedence: [flat])
      span_data = child_span(carrier, composite)

      _(span_data.hex_trace_id).must_equal(w3c_trace_id)
      _(span_data.parent_span_id.unpack1('H*')).must_equal(w3c_span_id)
      _(span_data.tracestate.to_s).must_equal('vendor-w3c=one')
      _(span_data.trace_flags).must_be(:sampled?)
    end

    it 'never splices the partial identifiers into the adopted parent' do
      composite = Composite.compose_propagators([w3c, flat, baggage], precedence: [flat])
      span_data = child_span(carrier, composite)
      next_hop = reinject(span_data, composite)

      _(next_hop[FlatTracePropagator::IDENTITY_KEY]).must_equal("#{w3c_trace_id}-#{span_data.hex_span_id}-1")
      _(next_hop['traceparent']).must_equal("00-#{w3c_trace_id}-#{span_data.hex_span_id}-01")
    end

    it 'does not create a remote parent from a partial trace context alone' do
      partial_carrier = { FlatTracePropagator::IDENTITY_KEY => "#{flat_trace_id}-" }
      composite = Composite.compose_propagators([w3c, flat])
      extracted = composite.extract(partial_carrier, context: OpenTelemetry::Context.empty)

      _(OpenTelemetry::Trace.current_span(extracted).context.valid?).must_equal(false)
    end
  end
end
