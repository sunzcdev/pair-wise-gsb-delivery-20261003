# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::SDK::Trace::Tracer do
  Tracer = OpenTelemetry::SDK::Trace::Tracer

  let(:exporter) do
    OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
  end
  let(:span_processor) do
    OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter)
  end
  let(:tracer_provider) do
    OpenTelemetry::SDK::Trace::TracerProvider.new.tap do |provider|
      provider.add_span_processor(span_processor)
    end
  end
  let(:tracer) do
    OpenTelemetry.tracer_provider = tracer_provider
    OpenTelemetry.tracer_provider.tracer('fiber-tracer', '1.0.0')
  end

  describe 'interleaved fibers' do
    # Two fibers are driven manually with Fiber#transfer so the ordering is
    # fully deterministic. Each fiber opens its own root span and a child,
    # and the two are paused and resumed in an alternating order. The span
    # parent must always follow the fiber's own current context.
    it 'keeps span parents aligned with the current context across fibers' do
      spans = {}
      main = Fiber.current
      fiber_a = nil
      fiber_b = nil
      a_finished = false
      b_finished = false

      fiber_a = Fiber.new do
        tracer.in_span('A-root') do |a_root|
          spans[:a_root] = a_root

          # A activates its root, then yields while B does the same.
          fiber_b.transfer

          tracer.in_span('A-child') do |a_child|
            spans[:a_child] = a_child
            _(OpenTelemetry::Trace.current_span).must_equal(a_child)

            # Yield again with both of A's spans still active on A's stack.
            fiber_b.transfer

            _(OpenTelemetry::Trace.current_span).must_equal(a_child)
          end

          _(OpenTelemetry::Trace.current_span).must_equal(a_root)
        end

        _(OpenTelemetry::Trace.current_span).wont_be(:recording?)
        a_finished = true
        main.transfer
      end

      fiber_b = Fiber.new do
        tracer.in_span('B-root') do |b_root|
          spans[:b_root] = b_root
          _(OpenTelemetry::Trace.current_span).must_equal(b_root)

          # B's current span must not leak in from A's stack.
          _(b_root.context.trace_id).wont_equal(spans[:a_root].context.trace_id)

          fiber_a.transfer

          tracer.in_span('B-child') do |b_child|
            spans[:b_child] = b_child
            _(OpenTelemetry::Trace.current_span).must_equal(b_child)
          end

          _(OpenTelemetry::Trace.current_span).must_equal(b_root)
        end

        b_finished = true
        fiber_a.transfer
      end

      fiber_a.transfer # A opens A-root and transfers to B
      # B opens B-root and transfers back to A
      # A opens A-child and transfers back to B
      # B opens/closes B-child, closes B-root and transfers back to A
      # A closes A-child and A-root and returns to main

      _(a_finished).must_equal(true)
      _(b_finished).must_equal(true)

      recorded = exporter.finished_spans.map { |sd| [sd.name, sd] }.to_h

      _(recorded['A-child'].parent_span_id).must_equal(spans[:a_root].context.span_id)
      _(recorded['B-child'].parent_span_id).must_equal(spans[:b_root].context.span_id)

      # Roots have no parent span, and the two fibers stay on separate traces.
      _(recorded['A-root'].parent_span_id).must_equal(OpenTelemetry::Trace::INVALID_SPAN_ID)
      _(recorded['B-root'].parent_span_id).must_equal(OpenTelemetry::Trace::INVALID_SPAN_ID)
      _(recorded['A-child'].trace_id).must_equal(recorded['A-root'].trace_id)
      _(recorded['B-child'].trace_id).must_equal(recorded['B-root'].trace_id)
      _(recorded['A-root'].trace_id).wont_equal(recorded['B-root'].trace_id)
    end

    # Mirrors the API-level cross-fiber detach scenario: a task resumes on the
    # wrong fiber and attempts to detach a token it does not own. The failed
    # detach must report an error without disturbing either fiber's stack, so
    # the owning fiber can still close its span cleanly and the parent
    # relationship remains intact.
    it 'preserves span parents after a cross-fiber detach error' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        tokens = {}
        spans = {}
        main = Fiber.current
        fiber_a = nil
        fiber_b = nil
        a_finished = false
        b_finished = false

        fiber_a = Fiber.new do
          tracer.in_span('A-root') do |a_root|
            spans[:a_root] = a_root
            ctx = OpenTelemetry::Trace.context_with_span(a_root)
            tokens[:a_root] = OpenTelemetry::Context.attach(ctx)

            fiber_b.transfer

            # B's erroneous detach did not pop A's context.
            _(OpenTelemetry::Trace.current_span).must_equal(a_root)

            tracer.in_span('A-child') do |a_child|
              spans[:a_child] = a_child
            end

            OpenTelemetry::Context.detach(tokens[:a_root])
          end

          a_finished = true
          main.transfer
        end

        fiber_b = Fiber.new do
          tracer.in_span('B-root') do |b_root|
            spans[:b_root] = b_root

            # Wrong fiber detaches a token owned by A: ownership mismatch.
            _(OpenTelemetry::Context.detach(tokens[:a_root])).must_equal(false)

            # B's own span is still current; its stack was not modified.
            _(OpenTelemetry::Trace.current_span).must_equal(b_root)

            tracer.in_span('B-child') do |b_child|
              spans[:b_child] = b_child
            end
          end

          b_finished = true
          fiber_a.transfer
        end

        fiber_a.transfer # A opens A-root, attaches and transfers to B
        # B hits the ownership mismatch, finishes its spans, transfers to A
        # A verifies its stack and finishes its own spans

        _(a_finished).must_equal(true)
        _(b_finished).must_equal(true)

        _(log_stream.string).must_match(/different fiber/)

        recorded = exporter.finished_spans.map { |sd| [sd.name, sd] }.to_h
        _(recorded['A-child'].parent_span_id).must_equal(spans[:a_root].context.span_id)
        _(recorded['B-child'].parent_span_id).must_equal(spans[:b_root].context.span_id)
        _(recorded['A-root'].trace_id).wont_equal(recorded['B-root'].trace_id)
      end
    end
  end
end
