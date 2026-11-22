# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe 'span processor sampling decision filter' do
  Decision = OpenTelemetry::SDK::Trace::Samplers::Decision

  # A sampler that always returns the same decision.
  class FixedDecisionSampler
    def initialize(decision)
      @decision = decision
    end

    def should_sample?(trace_id:, parent_context:, links:, name:, kind:, attributes:)
      OpenTelemetry::SDK::Trace::Samplers::Result.new(decision: @decision, tracestate: nil)
    end

    def description
      "FixedDecisionSampler(#{@decision})"
    end
  end

  # A local debug processor that records the spans it is called with. The
  # optional +sampling_decision_filter+ restricts which sampling decisions
  # are delivered to it; nil means all recording spans.
  class RecordingProcessor
    attr_reader :sampling_decision_filter, :started_spans, :finished_spans

    def initialize(sampling_decision_filter = nil)
      @sampling_decision_filter = sampling_decision_filter
      @started_spans = []
      @finished_spans = []
    end

    def on_start(span, parent_context)
      @started_spans << span
    end

    def on_finish(span)
      @finished_spans << span
    end

    def force_flush(timeout: nil)
      OpenTelemetry::SDK::Trace::Export::SUCCESS
    end

    def shutdown(timeout: nil)
      OpenTelemetry::SDK::Trace::Export::SUCCESS
    end
  end

  let(:exporter) { OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new }
  let(:debug_processor) { RecordingProcessor.new([Decision::RECORD_ONLY]) }
  let(:unfiltered_processor) { RecordingProcessor.new }

  def build_tracer_provider(decision)
    provider = OpenTelemetry::SDK::Trace::TracerProvider.new(sampler: FixedDecisionSampler.new(decision))
    provider.add_span_processor(debug_processor)
    provider.add_span_processor(unfiltered_processor)
    provider.add_span_processor(OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter))
    provider
  end

  describe 'when the sampler decides DROP' do
    it 'notifies no processor and exports nothing' do
      provider = build_tracer_provider(Decision::DROP)
      span = provider.tracer('test').start_span('dropped')
      _(span.recording?).must_equal false
      _(span.context.trace_flags.sampled?).must_equal false
      span.finish

      _(debug_processor.started_spans).must_be_empty
      _(debug_processor.finished_spans).must_be_empty
      _(unfiltered_processor.started_spans).must_be_empty
      _(unfiltered_processor.finished_spans).must_be_empty
      _(exporter.finished_spans).must_be_empty
    end
  end

  describe 'when the sampler decides RECORD_ONLY' do
    it 'delivers start and finish to the filtered debug processor, keeps the exporter empty, and preserves unsampled trace flags' do
      provider = build_tracer_provider(Decision::RECORD_ONLY)
      span = provider.tracer('test').start_span('record-only')
      _(span.recording?).must_equal true
      # The local processor recording the span must not mark the outbound
      # context as sampled.
      _(span.context.trace_flags.sampled?).must_equal false
      span.finish

      # The same span's start and finish go to the same filtered processor.
      _(debug_processor.started_spans).must_equal [span]
      _(debug_processor.finished_spans).must_equal [span]

      # Unfiltered processors see all recording spans.
      _(unfiltered_processor.started_spans).must_equal [span]
      _(unfiltered_processor.finished_spans).must_equal [span]

      # The regular export path only receives sampled spans.
      _(exporter.finished_spans).must_be_empty
    end
  end

  describe 'when the sampler decides RECORD_AND_SAMPLE' do
    it 'exports the span, skips the record-only debug processor, and preserves sampled trace flags' do
      provider = build_tracer_provider(Decision::RECORD_AND_SAMPLE)
      span = provider.tracer('test').start_span('sampled')
      _(span.recording?).must_equal true
      _(span.context.trace_flags.sampled?).must_equal true
      span.finish

      _(debug_processor.started_spans).must_be_empty
      _(debug_processor.finished_spans).must_be_empty

      _(unfiltered_processor.started_spans).must_equal [span]
      _(unfiltered_processor.finished_spans).must_equal [span]

      finished = exporter.finished_spans
      _(finished.length).must_equal 1
      _(finished[0].name).must_equal 'sampled'
      _(finished[0].trace_flags.sampled?).must_equal true
    end
  end
end
