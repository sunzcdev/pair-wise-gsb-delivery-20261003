# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

# A request can carry both a W3C trace context and a B3 header family. The
# composite propagator must pick one complete family for the parent span and
# keep the trace flags and tracestate of that same family together.
describe OpenTelemetry::Propagator::B3::Single::TextMapPropagator, 'inside a composite' do
  Composite = OpenTelemetry::Context::Propagation::CompositeTextMapPropagator

  let(:w3c) { OpenTelemetry::Trace::Propagation::TraceContext.text_map_propagator }
  let(:b3) { OpenTelemetry::Propagator::B3::Single.text_map_propagator }
  let(:baggage) { OpenTelemetry::Baggage::Propagation.text_map_propagator }

  let(:w3c_trace_id) { '0af7651916cd43dd8448eb211c80319c' }
  let(:w3c_span_id) { 'b7ad6b7169203331' }
  let(:b3_trace_id) { 'b7ad6b7169203331f0d0f1b3b0b2c1e4' }
  let(:b3_span_id) { '00f067aa0ba902b7' }

  after do
    OpenTelemetry::Context.clear
  end

  describe 'both families are complete' do
    let(:carrier) do
      {
        'traceparent' => "00-#{w3c_trace_id}-#{w3c_span_id}-01",
        'tracestate' => 'vendor-w3c=one',
        'b3' => "#{b3_trace_id}-#{b3_span_id}-1",
        'baggage' => 'user=alice'
      }
    end

    it 'reports both candidates with their own flags' do
      composite = Composite.compose_propagators([w3c, b3, baggage])
      candidates = composite.candidates(carrier, context: OpenTelemetry::Context.empty)

      _(candidates[0]).must_be(:valid?)
      _(candidates[0].trace_id).must_equal(Array(w3c_trace_id).pack('H*'))
      _(candidates[0].trace_flags).must_be(:sampled?)
      _(candidates[1]).must_be(:valid?)
      _(candidates[1].trace_id).must_equal(Array(b3_trace_id).pack('H*'))
      _(candidates[1].trace_flags).must_be(:sampled?)
      _(candidates[2]).must_be(:missing?)
    end

    it 'adopts the complete family named by precedence' do
      composite = Composite.compose_propagators([w3c, b3, baggage], precedence: [b3])
      context = composite.extract(carrier, context: OpenTelemetry::Context.empty)
      span_context = OpenTelemetry::Trace.current_span(context).context

      _(span_context.hex_trace_id).must_equal(b3_trace_id)
      _(span_context.hex_span_id).must_equal(b3_span_id)
      _(span_context).must_be(:remote?)
      _(OpenTelemetry::Baggage.values(context: context)).must_equal({ 'user' => 'alice' })
    end

    it 'keeps the W3C tracestate out of a B3 parent and vice versa' do
      b3_first = Composite.compose_propagators([w3c, b3], precedence: [b3])
      b3_context = b3_first.extract(carrier, context: OpenTelemetry::Context.empty)
      _(OpenTelemetry::Trace.current_span(b3_context).context.tracestate.empty?).must_equal(true)

      w3c_first = Composite.compose_propagators([w3c, b3], precedence: [w3c])
      w3c_context = w3c_first.extract(carrier, context: OpenTelemetry::Context.empty)
      _(OpenTelemetry::Trace.current_span(w3c_context).context.tracestate.to_s).must_equal('vendor-w3c=one')
    end
  end

  describe 'only the W3C family is complete' do
    let(:carrier) do
      {
        'traceparent' => "00-#{w3c_trace_id}-#{w3c_span_id}-01",
        'tracestate' => 'vendor-w3c=one',
        'b3' => "#{b3_trace_id}-",
        'baggage' => 'user=alice'
      }
    end

    it 'adopts the complete family even when the partial one is preferred' do
      composite = Composite.compose_propagators([w3c, b3, baggage], precedence: [b3])
      context = composite.extract(carrier, context: OpenTelemetry::Context.empty)
      span_context = OpenTelemetry::Trace.current_span(context).context

      _(span_context.hex_trace_id).must_equal(w3c_trace_id)
      _(span_context.hex_span_id).must_equal(w3c_span_id)
      _(span_context.tracestate.to_s).must_equal('vendor-w3c=one')
    end

    it 'reports the B3 family as missing while the W3C family is complete' do
      composite = Composite.compose_propagators([w3c, b3, baggage])
      candidates = composite.candidates(carrier, context: OpenTelemetry::Context.empty)

      _(candidates[0]).must_be(:valid?)
      _(candidates[1]).must_be(:missing?)
    end
  end

  describe 'only the B3 family is complete' do
    let(:carrier) do
      {
        'traceparent' => 'not-a-traceparent',
        'b3' => "#{b3_trace_id}-#{b3_span_id}-0"
      }
    end

    it 'adopts B3 and reports the malformed W3C family as invalid' do
      composite = Composite.compose_propagators([w3c, b3])
      candidates = composite.candidates(carrier, context: OpenTelemetry::Context.empty)

      _(candidates[0]).must_be(:invalid?)
      _(candidates[1]).must_be(:valid?)

      context = composite.extract(carrier, context: OpenTelemetry::Context.empty)
      span_context = OpenTelemetry::Trace.current_span(context).context
      _(span_context.hex_trace_id).must_equal(b3_trace_id)
      _(span_context.trace_flags).must_equal(OpenTelemetry::Trace::TraceFlags::DEFAULT)
    end
  end
end
