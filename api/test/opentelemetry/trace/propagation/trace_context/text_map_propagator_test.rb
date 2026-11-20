# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::Trace::Propagation::TraceContext::TextMapPropagator do
  Span = OpenTelemetry::Trace::Span
  SpanContext = OpenTelemetry::Trace::SpanContext

  let(:traceparent_key) { 'traceparent' }
  let(:tracestate_key) { 'tracestate' }
  let(:propagator) do
    OpenTelemetry::Trace::Propagation::TraceContext::TextMapPropagator.new
  end
  let(:valid_traceparent_header) do
    '00-000000000000000000000000000000AA-00000000000000ea-01'
  end
  let(:invalid_traceparent_header) do
    'FF-000000000000000000000000000000AA-00000000000000ea-01'
  end
  let(:tracestate_header) { 'vendorname=opaquevalue' }
  let(:tracestate) { OpenTelemetry::Trace::Tracestate.from_hash('vendorname' => 'opaquevalue') }
  let(:carrier) do
    {
      traceparent_key => valid_traceparent_header,
      tracestate_key => tracestate_header
    }
  end
  let(:context) { Context.empty }
  let(:context_with_tracestate) do
    span_context = SpanContext.new(trace_id: ("\xff" * 16).b, span_id: ("\x11" * 8).b,
                                   tracestate: tracestate_header)
    span = OpenTelemetry::Trace.non_recording_span(span_context)
    OpenTelemetry::Trace.context_with_span(span, parent_context: Context.empty)
  end
  let(:context_without_tracestate) do
    span_context = SpanContext.new(trace_id: ("\xff" * 16).b, span_id: ("\x11" * 8).b)
    span = OpenTelemetry::Trace.non_recording_span(span_context)
    OpenTelemetry::Trace.context_with_span(span, parent_context: Context.empty)
  end

  describe '#extract' do
    it 'returns a remote SpanContext with fields from the traceparent and tracestate headers' do
      ctx = propagator.extract(carrier, context: context) { |c, k| c[k] }
      span_context = OpenTelemetry::Trace.current_span(ctx).context
      _(span_context).must_be :remote?
      _(span_context.trace_id).must_equal((("\0" * 15) + "\xaa").b)
      _(span_context.span_id).must_equal((("\0" * 7) + "\xea").b)
      _(span_context.trace_flags).must_be :sampled?
      _(span_context.tracestate).must_equal(tracestate)
    end

    it 'uses a default getter if one is not provided' do
      ctx = propagator.extract(carrier, context: context)
      span_context = OpenTelemetry::Trace.current_span(ctx).context
      _(span_context).must_be :remote?
      _(span_context.trace_id).must_equal((("\0" * 15) + "\xaa").b)
      _(span_context.span_id).must_equal((("\0" * 7) + "\xea").b)
      _(span_context.trace_flags).must_be :sampled?
      _(span_context.tracestate).must_equal(tracestate)
    end

    it 'returns original context on error' do
      ctx = propagator.extract({}, context: context) { invalid_traceparent_header }
      _(ctx).must_equal(context)
    end

    it 'should apply current span on argument context' do
      key = Context.create_key('key1')
      ctx = Context::ROOT.set_value(key, 'value1')
      extracted_ctx = propagator.extract(carrier, context: ctx) { |c, k| c[k] }

      _(extracted_ctx.value(key)).must_equal('value1')
    end
  end

  describe '#extract_candidate' do
    it 'reports a complete candidate with this header family flags and tracestate' do
      candidate = propagator.extract_candidate(carrier, context: context)

      _(candidate).must_be(:valid?)
      _(candidate).wont_be(:missing?)
      _(candidate).wont_be(:invalid?)
      _(candidate.propagator).must_be_same_as(propagator)
      _(candidate.trace_id).must_equal((("\0" * 15) + "\xaa").b)
      _(candidate.span_id).must_equal((("\0" * 7) + "\xea").b)
      _(candidate.trace_flags).must_be(:sampled?)
      _(candidate.tracestate.to_s).must_equal(tracestate_header)
      _(candidate).must_be(:remote?)
    end

    it 'reports a missing candidate when there is no traceparent header' do
      candidate = propagator.extract_candidate({ tracestate_key => tracestate_header }, context: context)

      _(candidate).must_be(:missing?)
      _(candidate.state).must_equal(:missing)
      _(candidate.trace_id).must_be_nil
      _(candidate.trace_flags).must_be_nil
      _(candidate.tracestate).must_be_nil
    end

    it 'distinguishes a malformed traceparent header from a missing one' do
      candidate = propagator.extract_candidate({ traceparent_key => invalid_traceparent_header }, context: context)

      _(candidate).wont_be(:missing?)
      _(candidate).must_be(:invalid?)
      _(candidate.state).must_equal(:invalid)
    end

    it 'exposes a hash form for logging and assertions' do
      candidate = propagator.extract_candidate(carrier, context: context)
      hash = candidate.to_h

      _(hash[:state]).must_equal(:valid)
      _(hash[:trace_id]).must_equal('0' * 30 + 'aa')
      _(hash[:span_id]).must_equal('0' * 14 + 'ea')
      _(hash[:tracestate]).must_equal(tracestate_header)
      _(hash[:remote]).must_equal(true)
    end
  end

  describe '#inject' do
    it 'writes traceparent into the carrier' do
      carrier = {}
      propagator.inject(carrier, context: context_without_tracestate)
      _(carrier[traceparent_key]).must_equal('00-ffffffffffffffffffffffffffffffff-1111111111111111-00')
    end

    it 'does not write the tracestate into carrier, if nil' do
      carrier = {}
      propagator.inject(carrier, context: context_without_tracestate)
      _(carrier).wont_include(tracestate_key)
    end

    it 'writes the tracestate into the context, if provided' do
      carrier = {}
      propagator.inject(carrier, context: context_with_tracestate)
      _(carrier).must_include(tracestate_key)
    end

    it 'uses the default setter if one is not provided' do
      carrier = {}
      propagator.inject(carrier, context: context_with_tracestate)
      _(carrier[traceparent_key]).must_equal('00-ffffffffffffffffffffffffffffffff-1111111111111111-00')
      _(carrier[tracestate_key]).must_equal(tracestate_header)
    end

    it 'propagates remote context without current span' do
      carrier = {}
      propagator.inject(carrier, context: context_with_tracestate)
      _(carrier[traceparent_key]).must_equal('00-ffffffffffffffffffffffffffffffff-1111111111111111-00')
      _(carrier[tracestate_key]).must_equal(tracestate_header)
    end
  end
end
