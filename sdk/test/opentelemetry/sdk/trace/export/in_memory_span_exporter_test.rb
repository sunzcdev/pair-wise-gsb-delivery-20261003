# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter do
  export = OpenTelemetry::SDK::Trace::Export

  let(:span_data1) { OpenTelemetry::SDK::Trace::SpanData.new({ name: 'name1' }) }
  let(:span_data2) { OpenTelemetry::SDK::Trace::SpanData.new({ name: 'name2' }) }

  let(:exporter) { OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new }

  it 'accepts an Array of SpanDatas as argument to #export' do
    exporter.export([span_data1, span_data2])

    finished_spans = exporter.finished_spans
    _(finished_spans[0]).must_equal span_data1
    _(finished_spans[1]).must_equal span_data2
  end

  it 'accepts an Enumerable of SpanDatas as argument to #export' do
    # An anonymous Struct serves as a handy implementer of Enumerable
    enumerable = Struct.new(:span_data1, :span_data2).new
    enumerable.span_data1 = span_data1
    enumerable.span_data2 = span_data2

    exporter.export(enumerable)

    finished_spans = exporter.finished_spans
    _(finished_spans[0]).must_equal span_data1
    _(finished_spans[1]).must_equal span_data2
  end

  it 'freezes the return of #finished_spans' do
    exporter.export([span_data1])
    _(exporter.finished_spans).must_be :frozen?
  end

  it 'allows additional calls to #export after #finished_spans' do
    exporter.export([span_data1])
    finished_spans1 = exporter.finished_spans

    exporter.export([span_data2])
    finished_spans2 = exporter.finished_spans

    _(finished_spans1.length).must_equal 1
    _(finished_spans2.length).must_equal 2

    _(finished_spans1[0]).must_equal finished_spans2[0]
  end

  it 'returns success from #export' do
    _(exporter.export([span_data1])).must_equal export::SUCCESS
  end

  it 'returns error from #export after #shutdown called' do
    exporter.export([span_data1])
    exporter.shutdown

    _(exporter.export([span_data2])).must_equal export::FAILURE
  end

  it 'returns an empty array from #export after #shutdown called' do
    exporter.export([span_data1])
    exporter.shutdown

    _(exporter.finished_spans.length).must_equal 0
  end

  it 'records nothing if #recording is false' do
    exporter.recording = false
    exporter.export([span_data1])
    _(exporter.finished_spans.length).must_equal 0

    # In this test, we're generally recording all the time - restore that state.
    exporter.recording = true
  end

  describe 'observing a recorded exception cause chain' do
    let(:provider) do
      OpenTelemetry::SDK::Trace::TracerProvider.new(span_limits: span_limits).tap do |provider|
        provider.add_span_processor(OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter))
      end
    end
    let(:tracer) { provider.tracer('cause-chain-test') }
    let(:span_limits) { OpenTelemetry::SDK::Trace::SpanLimits.new }

    def exported_exception_event
      tracer.in_span('request') do |span|
        begin
          request
        rescue StandardError => e
          span.record_exception(e, record_cause_chain: true)
        end
      end
      span_data = exporter.finished_spans.last
      span_data.events.find { |event| event.name == 'exception' }
    end

    it 'locates the first failure through a two-level wrapper' do
      attributes = exported_exception_event.attributes

      _(attributes['exception.type']).must_equal('RuntimeError')
      _(attributes['exception.message']).must_equal('request failed')
      _(attributes['exception.cause.count']).must_equal(2)
      _(attributes['exception.cause.1.type']).must_equal('ArgumentError')
      _(attributes['exception.cause.1.message']).must_equal('repository failed')
      _(attributes['exception.cause.2.type']).must_equal('IOError')
      _(attributes['exception.cause.2.message']).must_equal('connection timed out')
      _(attributes['exception.cause.2.depth']).must_equal(2)
      _(attributes.key?('exception.cause.truncated')).must_equal(false)
    end

    it 'reports the shared frame boundary instead of repeating frames' do
      attributes = exported_exception_event.attributes

      # Both wrappers were raised from within a rescue, so each shares the
      # trailing frames of the exception it wraps.
      _(attributes['exception.cause.1.shared_frames']).must_be :>, 0
      _(attributes['exception.cause.2.shared_frames']).must_be :>, 0

      # The frames below the boundary are only recorded once, on the wrapper.
      _(attributes['exception.cause.2.stacktrace']).must_include('connect')
      _(attributes['exception.cause.2.stacktrace']).wont_include('trace/tracer.rb')
      _(attributes['exception.stacktrace']).must_include('trace/tracer.rb')
    end

    it 'marks repeated exception references and stops looking' do
      exporter.reset
      tracer.in_span('request') do |span|
        span.record_exception(cyclic_error, record_cause_chain: true)
      end
      attributes = exporter.finished_spans.last.events.first.attributes

      _(attributes['exception.cause.truncated']).must_equal('alias')
      _(attributes['exception.cause.2.alias_of']).must_equal(0)
    end

    it 'names the limit that cut the chain under a low budget' do
      exporter.reset
      low_budget_provider = OpenTelemetry::SDK::Trace::TracerProvider.new(
        span_limits: OpenTelemetry::SDK::Trace::SpanLimits.new(exception_cause_count_limit: 1,
                                                               exception_cause_length_limit: 32)
      )
      low_budget_provider.add_span_processor(OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter))
      low_budget_tracer = low_budget_provider.tracer('cause-chain-test')

      low_budget_tracer.in_span('request') do |span|
        begin
          request('a message long enough that the cause length limit has to shorten it')
        rescue StandardError => e
          span.record_exception(e, record_cause_chain: true)
        end
      end
      attributes = exporter.finished_spans.last.events.first.attributes

      _(attributes['exception.cause.count']).must_equal(1)
      _(attributes['exception.cause.1.message'].length).must_equal(32)
      _(attributes['exception.cause.1.truncated']).must_include('message')
      _(attributes['exception.cause.truncated']).must_include('length_limit')
      _(attributes['exception.cause.truncated']).must_include('cause_count_limit')
    end

    def connect(message = nil)
      raise IOError, (message || 'connection timed out')
    end

    def repository(message = nil)
      connect(message)
    rescue IOError => e
      raise ArgumentError, (message || 'repository failed'), cause: e
    end

    def request(message = nil)
      repository(message)
    rescue ArgumentError => e
      raise RuntimeError, 'request failed', cause: e
    end

    def cyclic_error
      inner = RuntimeError.new('inner')
      inner.set_backtrace(['app/app.rb:30:in `call`'])
      outer = RuntimeError.new('outer')
      outer.set_backtrace(['app/app.rb:31:in `call`'])
      outer.define_singleton_method(:cause) { inner }
      inner.define_singleton_method(:cause) { outer }
      outer
    end
  end
end
