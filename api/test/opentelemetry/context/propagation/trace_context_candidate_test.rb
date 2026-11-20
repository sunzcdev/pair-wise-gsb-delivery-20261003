# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::Context::Propagation::TraceContextCandidate do
  Candidate = OpenTelemetry::Context::Propagation::TraceContextCandidate

  let(:trace_id) { '0af7651916cd43dd8448eb211c80319c' }
  let(:span_id) { 'b7ad6b7169203331' }
  let(:propagator) { Object.new }
  let(:before_context) { OpenTelemetry::Context.empty }

  def context_with_span(span_context)
    OpenTelemetry::Trace.context_with_span(
      OpenTelemetry::Trace.non_recording_span(span_context),
      parent_context: OpenTelemetry::Context.empty
    )
  end

  def span_context(trace_id: Array(self.trace_id).pack('H*'), span_id: Array(self.span_id).pack('H*'), **options)
    OpenTelemetry::Trace::SpanContext.new(trace_id: trace_id, span_id: span_id, **options)
  end

  describe '.missing' do
    it 'is missing and has no span context' do
      candidate = Candidate.missing(propagator)

      _(candidate).must_be(:missing?)
      _(candidate.state).must_equal(:missing)
      _(candidate.span_context).must_be_nil
      _(candidate.trace_id).must_be_nil
      _(candidate.span_id).must_be_nil
      _(candidate.trace_flags).must_be_nil
      _(candidate.tracestate).must_be_nil
      _(candidate.remote?).must_equal(false)
    end
  end

  describe '.invalid' do
    it 'is present but not valid' do
      candidate = Candidate.invalid(propagator)

      _(candidate).must_be(:invalid?)
      _(candidate).must_be(:present?)
      _(candidate).wont_be(:valid?)
      _(candidate).wont_be(:partial?)
      _(candidate.span_context).must_be_nil
    end
  end

  describe '.from_context' do
    it 'reports a complete span context as valid with its own flags and tracestate' do
      tracestate = OpenTelemetry::Trace::Tracestate.from_hash('vendor' => 'value')
      after_context = context_with_span(span_context(trace_flags: OpenTelemetry::Trace::TraceFlags::SAMPLED, tracestate: tracestate, remote: true))
      candidate = Candidate.from_context(propagator: propagator, before_context: before_context, after_context: after_context)

      _(candidate).must_be(:valid?)
      _(candidate).must_be(:complete?)
      _(candidate.trace_id).must_equal(Array(trace_id).pack('H*'))
      _(candidate.span_id).must_equal(Array(span_id).pack('H*'))
      _(candidate.trace_flags).must_equal(OpenTelemetry::Trace::TraceFlags::SAMPLED)
      _(candidate.tracestate).must_equal(tracestate)
      _(candidate.remote?).must_equal(true)
    end

    it 'reports a span context with only a trace ID as partial' do
      after_context = context_with_span(span_context(span_id: OpenTelemetry::Trace::INVALID_SPAN_ID))
      candidate = Candidate.from_context(propagator: propagator, before_context: before_context, after_context: after_context)

      _(candidate).must_be(:partial?)
      _(candidate).must_be(:present?)
      _(candidate).wont_be(:valid?)
      _(candidate.trace_id).must_equal(Array(trace_id).pack('H*'))
      _(candidate.span_id).must_equal(OpenTelemetry::Trace::INVALID_SPAN_ID)
    end

    it 'reports a span context with only a span ID as partial' do
      after_context = context_with_span(span_context(trace_id: OpenTelemetry::Trace::INVALID_TRACE_ID))
      candidate = Candidate.from_context(propagator: propagator, before_context: before_context, after_context: after_context)

      _(candidate).must_be(:partial?)
      _(candidate.span_id).must_equal(Array(span_id).pack('H*'))
    end

    it 'reports a span context with neither identifier as invalid' do
      after_context = context_with_span(span_context(trace_id: OpenTelemetry::Trace::INVALID_TRACE_ID, span_id: OpenTelemetry::Trace::INVALID_SPAN_ID))
      candidate = Candidate.from_context(propagator: propagator, before_context: before_context, after_context: after_context)

      _(candidate).must_be(:invalid?)
      _(candidate).wont_be(:partial?)
    end

    it 'reports missing when the current span did not change' do
      base = context_with_span(span_context)
      after_context = base.set_value('unrelated', true)
      candidate = Candidate.from_context(propagator: propagator, before_context: base, after_context: after_context)

      _(candidate).must_be(:missing?)
    end
  end

  describe '.from_span_context' do
    it 'treats the previous span context as missing' do
      span = span_context
      candidate = Candidate.from_span_context(propagator, span, previous_span_context: span)

      _(candidate).must_be(:missing?)
    end
  end

  describe '#to_h' do
    it 'hex encodes identifiers and reports the state' do
      after_context = context_with_span(span_context(trace_flags: OpenTelemetry::Trace::TraceFlags::SAMPLED, remote: true))
      candidate = Candidate.from_context(propagator: propagator, before_context: before_context, after_context: after_context)

      _(candidate.to_h).must_equal({
        propagator: propagator,
        state: :valid,
        trace_id: trace_id,
        span_id: span_id,
        trace_flags: OpenTelemetry::Trace::TraceFlags::SAMPLED,
        tracestate: '',
        remote: true
      })
    end
  end
end
