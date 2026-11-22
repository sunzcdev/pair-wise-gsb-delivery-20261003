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

  describe 'resources' do
    let(:resource) do
      OpenTelemetry::SDK::Resources::Resource.create(
        { 'service.name' => 'checkout', 'deployment.environment' => 'production' },
        schema_url: 'https://opentelemetry.io/schemas/1.21.0',
        source: :application
      )
    end

    it 'exposes the resource carried by each exported span' do
      span_data = OpenTelemetry::TestHelpers.create_span_data(name: 'with-resource', resource: resource)

      exporter.export([span_data])

      _(exporter.resources).must_equal([resource])
      _(exporter.resources.first.attribute_enumerator.to_h['service.name']).must_equal('checkout')
    end

    it 'exposes one resource per exported span in export order' do
      other_resource = OpenTelemetry::SDK::Resources::Resource.create({ 'service.name' => 'billing' }, source: :application)
      first = OpenTelemetry::TestHelpers.create_span_data(name: 'first', resource: resource)
      second = OpenTelemetry::TestHelpers.create_span_data(name: 'second', resource: other_resource)

      exporter.export([first, second])

      _(exporter.resources.map { |r| r.attribute_enumerator.to_h['service.name'] }).must_equal(%w[checkout billing])
    end

    it 'returns a frozen array of resources' do
      exporter.export([OpenTelemetry::TestHelpers.create_span_data(resource: resource)])

      _(exporter.resources).must_be(:frozen?)
    end

    it 'reports the merge provenance of the exported resource' do
      merged_resource = OpenTelemetry::SDK::Resources::Resource
                         .create({ 'service.name' => 'from-default' }, schema_url: 'https://opentelemetry.io/schemas/1.0.0', source: :default)
                         .merge(resource, source: :application)
      exporter.export([OpenTelemetry::TestHelpers.create_span_data(resource: merged_resource)])

      report = exporter.resource_reports.first

      _(report.sources['service.name']).must_equal(:application)
      _(report.attributes['service.name']).must_equal('checkout')
      _(report.attributes['deployment.environment']).must_equal('production')
      _(report.schema_url).must_equal('https://opentelemetry.io/schemas/1.21.0')
      _(report.schema_source).must_equal(:application)
      _(report.schema_conflict.replaced_schema_url).must_equal('https://opentelemetry.io/schemas/1.0.0')
      _(report.conflicts.map(&:key)).must_equal(['service.name'])
    end

    it 'deduplicates reports for identical resources' do
      exporter.export([
                        OpenTelemetry::TestHelpers.create_span_data(name: 'one', resource: resource),
                        OpenTelemetry::TestHelpers.create_span_data(name: 'two', resource: resource)
                      ])

      _(exporter.resources.length).must_equal(2)
      _(exporter.resource_reports.length).must_equal(1)
    end

    it 'clears resources on #reset' do
      exporter.export([OpenTelemetry::TestHelpers.create_span_data(resource: resource)])
      exporter.reset

      _(exporter.resources).must_be_empty
      _(exporter.resource_reports).must_be_empty
    end

    it 'clears resources on #shutdown' do
      exporter.export([OpenTelemetry::TestHelpers.create_span_data(resource: resource)])
      exporter.shutdown

      _(exporter.resources).must_be_empty
    end

    it 'records no resources when #recording is false' do
      exporter.recording = false
      exporter.export([OpenTelemetry::TestHelpers.create_span_data(resource: resource)])

      _(exporter.resources).must_be_empty

      exporter.recording = true
    end

    it 'skips span data that carries no resource' do
      exporter.export([span_data1])

      _(exporter.resources).must_be_empty
    end
  end

  describe 'three-layer resource' do
    before do
      OpenTelemetry::SDK::Resources::Resource.instance_variable_set(:@default, nil)
    end

    after do
      OpenTelemetry::SDK::Resources::Resource.instance_variable_set(:@default, nil)
    end

    it 'exports the resource the provider resolved and reports where each value came from' do
      OpenTelemetry::TestHelpers.with_env(
        'OTEL_RESOURCE_ATTRIBUTES' => 'deployment.environment=staging,service.version=1.9.0',
        'OTEL_SERVICE_NAME' => 'checkout-api'
      ) do
        configurator = OpenTelemetry::SDK::Configurator.new
        configurator.resource = OpenTelemetry::SDK::Resources::Resource.create(
          { 'service.version' => '2.0.0', 'deployment.environment' => '', 'team' => 'payments' },
          schema_url: 'https://opentelemetry.io/schemas/1.21.0',
          source: :application
        )

        provider = OpenTelemetry::SDK::Trace::TracerProvider.new(
          resource: configurator.resource
        )
        provider.add_span_processor(OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter))
        provider.tracer('three-layer-test').in_span('three-layer') {}

        exported = exporter.resources.first
        attributes = exported.attribute_enumerator.to_h
        report = exporter.resource_reports.first

        # The exporter sees the merged result, not just one layer.
        _(attributes['service.name']).must_equal('checkout-api')
        _(attributes['service.version']).must_equal('2.0.0')
        _(attributes['deployment.environment']).must_equal('')
        _(attributes['team']).must_equal('payments')
        _(attributes['telemetry.sdk.language']).must_equal('ruby')
        _(attributes['process.pid']).must_equal(Process.pid)
        _(exported.schema_url).must_equal('https://opentelemetry.io/schemas/1.21.0')

        # The report says which layer supplied each value and the schema URL.
        _(report.sources['telemetry.sdk.name']).must_equal(:telemetry_sdk)
        _(report.sources['process.pid']).must_equal(:process)
        _(report.sources['service.name']).must_equal(:environment)
        _(report.sources['service.version']).must_equal(:application)
        _(report.sources['deployment.environment']).must_equal(:application)
        _(report.sources['team']).must_equal(:application)
        _(report.schema_url).must_equal('https://opentelemetry.io/schemas/1.21.0')
        _(report.schema_source).must_equal(:application)

        # Collisions are visible instead of silently collapsing.
        version_conflict = report.conflicts.find { |conflict| conflict.key == 'service.version' }
        _(version_conflict.kept_value).must_equal('2.0.0')
        _(version_conflict.kept_source).must_equal(:application)
        _(version_conflict.replaced_value).must_equal('1.9.0')
        _(version_conflict.replaced_source).must_equal(:environment)

        environment_conflict = report.conflicts.find { |conflict| conflict.key == 'deployment.environment' }
        _(environment_conflict.kept_value).must_equal('')
        _(environment_conflict.replaced_value).must_equal('staging')

        service_conflict = report.conflicts.find { |conflict| conflict.key == 'service.name' }
        _(service_conflict.kept_value).must_equal('checkout-api')
        _(service_conflict.replaced_value).must_equal('unknown_service')
      end
    end

    it 'surfaces a schema_url decision when two layers disagree' do
      OpenTelemetry::TestHelpers.with_env('OTEL_RESOURCE_ATTRIBUTES' => 'deployment.environment=staging') do
        base = OpenTelemetry::SDK::Resources::Resource.create(
          { 'service.name' => 'checkout-api' },
          schema_url: 'https://opentelemetry.io/schemas/1.0.0',
          source: :application
        )
        # Second application value carries a different schema URL.
        merged = base.merge(
          OpenTelemetry::SDK::Resources::Resource.create(
            { 'service.version' => '3.1.0' },
            schema_url: 'https://opentelemetry.io/schemas/1.21.0'
          ),
          source: :application
        )

        provider = OpenTelemetry::SDK::Trace::TracerProvider.new(resource: merged)
        provider.add_span_processor(OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter))
        provider.tracer('schema-test').in_span('schema') {}

        exported = exporter.resources.first
        report = exporter.resource_reports.first

        _(exported.schema_url).must_equal('https://opentelemetry.io/schemas/1.21.0')
        _(report.schema_url).must_equal('https://opentelemetry.io/schemas/1.21.0')
        _(report.schema_source).must_equal(:application)
        _(report.schema_conflict).wont_be_nil
        _(report.schema_conflict.kept_schema_url).must_equal('https://opentelemetry.io/schemas/1.21.0')
        _(report.schema_conflict.replaced_schema_url).must_equal('https://opentelemetry.io/schemas/1.0.0')
      end
    end
  end
end
