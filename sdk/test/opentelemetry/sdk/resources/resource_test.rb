# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::SDK::Resources::Resource do
  Resource = OpenTelemetry::SDK::Resources::Resource

  describe '.new' do
    it 'is private' do
      _(proc { Resource.new('k1' => 'v1') }).must_raise(NoMethodError)
    end
  end

  describe '.create' do
    it 'can be initialized with attributes' do
      expected_attributes = { 'k1' => 'v1', 'k2' => 'v2' }
      resource = Resource.create(expected_attributes)
      _(resource.attribute_enumerator.to_h).must_equal(expected_attributes)
    end

    it 'can be empty' do
      resource = Resource.create
      _(resource.attribute_enumerator.to_h).must_be_empty
    end

    it 'enforces keys are strings' do
      _(proc { Resource.create(k1: 'v1') }).must_raise(ArgumentError)
    end

    it 'enforces values are strings, ints, floats, or booleans' do
      _(proc { Resource.create('k1' => :v1) }).must_raise(ArgumentError)
      values = ['v1', 123, 456.78, false, true]
      values.each do |value|
        resource = Resource.create('k1' => value)
        _(resource.attribute_enumerator.first.last).must_equal(value)
      end
    end

    it 'keeps an explicitly supplied empty string as a valid attribute' do
      resource = Resource.create('k1' => '')

      _(resource.attribute_enumerator.to_h).must_equal({ 'k1' => '' })
    end

    it 'records the schema_url of the resource' do
      resource = Resource.create({ 'k1' => 'v1' }, schema_url: 'https://opentelemetry.io/schemas/1.21.0')

      _(resource.schema_url).must_equal('https://opentelemetry.io/schemas/1.21.0')
    end

    it 'treats an empty schema_url as absent' do
      _(Resource.create({ 'k1' => 'v1' }, schema_url: '').schema_url).must_be_nil
    end

    it 'labels attributes with the supplied source' do
      resource = Resource.create({ 'k1' => 'v1' }, source: :application)

      _(resource.merge_report.sources).must_equal({ 'k1' => :application })
    end

    it 'reports no source for attributes created without one' do
      resource = Resource.create('k1' => 'v1')

      _(resource.merge_report.sources).must_be_empty
    end
  end

  describe '.default' do
    before do
      Resource.instance_variable_set(:@default, nil)
    end

    after do
      Resource.instance_variable_set(:@default, nil)
    end

    it 'contains telemetry sdk attributes' do
      resource_attributes = Resource.default.attribute_enumerator.to_h
      _(resource_attributes).must_include('telemetry.sdk.name')
      _(resource_attributes).must_include('telemetry.sdk.language')
      _(resource_attributes).must_include('telemetry.sdk.version')
    end

    it 'contains process attributes' do
      resource_attributes = Resource.default.attribute_enumerator.to_h
      _(resource_attributes).must_include('process.pid')
      _(resource_attributes).must_include('process.command')
      _(resource_attributes).must_include('process.runtime.name')
      _(resource_attributes).must_include('process.runtime.version')
      _(resource_attributes).must_include('process.runtime.description')
    end

    it 'contains a default value for service.name' do
      resource_attributes = Resource.default.attribute_enumerator.to_h
      _(resource_attributes).must_include('service.name')
      _(resource_attributes['service.name']).must_equal('unknown_service')
    end

    it 'allows overriding the default service.name with the OTEL_SERVICE_NAME environment variable' do
      OpenTelemetry::TestHelpers.with_env('OTEL_SERVICE_NAME' => 'svc') do
        resource_attributes = Resource.default.attribute_enumerator.to_h
        _(resource_attributes).must_include('service.name')
        _(resource_attributes['service.name']).must_equal('svc')
      end
    end

    it 'allows overriding the default service.name with the OTEL_RESOURCE_ATTRIBUTES environment variable' do
      OpenTelemetry::TestHelpers.with_env('OTEL_RESOURCE_ATTRIBUTES' => 'service.name=svc') do
        resource_attributes = Resource.default.attribute_enumerator.to_h
        _(resource_attributes).must_include('service.name')
        _(resource_attributes['service.name']).must_equal('svc')
      end
    end

    it 'lets the OTEL_SERVICE_NAME environment variable take precedence over the OTEL_RESOURCE_ATTRIBUTES environment variable' do
      OpenTelemetry::TestHelpers.with_env('OTEL_SERVICE_NAME' => 'svc-ok', 'OTEL_RESOURCE_ATTRIBUTES' => 'service.name=svc-bad') do
        resource_attributes = Resource.default.attribute_enumerator.to_h
        _(resource_attributes).must_include('service.name')
        _(resource_attributes['service.name']).must_equal('svc-ok')
      end
    end

    it 'reports the layer that supplied each value' do
      OpenTelemetry::TestHelpers.with_env(
        'OTEL_SERVICE_NAME' => 'svc',
        'OTEL_RESOURCE_ATTRIBUTES' => 'deployment.environment=staging'
      ) do
        report = Resource.default.merge_report

        _(report.sources['telemetry.sdk.name']).must_equal(:telemetry_sdk)
        _(report.sources['process.pid']).must_equal(:process)
        _(report.sources['service.name']).must_equal(:environment)
        _(report.sources['deployment.environment']).must_equal(:environment)
      end
    end

    it 'reports the service.name override as a conflict between default and environment' do
      OpenTelemetry::TestHelpers.with_env('OTEL_SERVICE_NAME' => 'svc') do
        conflict = Resource.default.merge_report.conflicts.find { |c| c.key == 'service.name' }

        _(conflict).wont_be_nil
        _(conflict.kept_value).must_equal('svc')
        _(conflict.kept_source).must_equal(:environment)
        _(conflict.replaced_value).must_equal('unknown_service')
        _(conflict.replaced_source).must_equal(:default)
      end
    end

    it 'keeps an empty OTEL_SERVICE_NAME value rather than treating it as absent' do
      OpenTelemetry::TestHelpers.with_env('OTEL_SERVICE_NAME' => '') do
        resource_attributes = Resource.default.attribute_enumerator.to_h

        _(resource_attributes['service.name']).must_equal('')
      end
    end
  end

  describe '.telemetry_sdk' do
    it 'returns a resource for the telemetry sdk' do
      resource_attributes = Resource.telemetry_sdk.attribute_enumerator.to_h
      _(resource_attributes['telemetry.sdk.name']).must_equal('opentelemetry')
      _(resource_attributes['telemetry.sdk.language']).must_equal('ruby')
      _(resource_attributes['telemetry.sdk.version']).must_match(/\b\d{1,3}\.\d{1,3}\.\d{1,3}/)
    end

    describe 'when the environment variable is present' do
      let(:expected_resource_attributes) do
        {
          'key1' => 'value1',
          'key2' => 'value2',
          'telemetry.sdk.name' => 'opentelemetry',
          'telemetry.sdk.language' => 'ruby',
          'telemetry.sdk.version' => OpenTelemetry::SDK::VERSION
        }
      end

      it 'includes environment resources' do
        OpenTelemetry::TestHelpers.with_env('OTEL_RESOURCE_ATTRIBUTES' => 'key1=value1,key2=value2') do
          resource_attributes = Resource.telemetry_sdk.attribute_enumerator.to_h
          _(resource_attributes).must_equal(expected_resource_attributes)
        end
      end

      it 'attributes built-in and environment values to their layers' do
        OpenTelemetry::TestHelpers.with_env('OTEL_RESOURCE_ATTRIBUTES' => 'key1=value1') do
          report = Resource.telemetry_sdk.merge_report

          _(report.sources['telemetry.sdk.name']).must_equal(:telemetry_sdk)
          _(report.sources['key1']).must_equal(:environment)
        end
      end

      it 'reports when the environment overrides a built-in value' do
        OpenTelemetry::TestHelpers.with_env('OTEL_RESOURCE_ATTRIBUTES' => 'telemetry.sdk.language=python') do
          report = Resource.telemetry_sdk.merge_report

          _(report.attributes['telemetry.sdk.language']).must_equal('python')
          _(report.sources['telemetry.sdk.language']).must_equal(:environment)

          conflict = report.conflicts.find { |c| c.key == 'telemetry.sdk.language' }
          _(conflict).wont_be_nil
          _(conflict.kept_value).must_equal('python')
          _(conflict.replaced_value).must_equal('ruby')
          _(conflict.replaced_source).must_equal(:telemetry_sdk)
        end
      end

      it 'drops a key whose environment value is empty' do
        OpenTelemetry::TestHelpers.with_env('OTEL_RESOURCE_ATTRIBUTES' => 'telemetry.sdk.language=') do
          resource_attributes = Resource.telemetry_sdk.attribute_enumerator.to_h

          _(resource_attributes).wont_include('telemetry.sdk.language')
        end
      end
    end
  end

  describe '.process' do
    let(:expected_resource_attributes) do
      {
        'process.pid' => Process.pid,
        'process.command' => $PROGRAM_NAME,
        'process.runtime.name' => RUBY_ENGINE,
        'process.runtime.version' => RUBY_VERSION,
        'process.runtime.description' => RUBY_DESCRIPTION
      }
    end

    it 'returns a resource for the process and runtime' do
      resource_attributes = Resource.process.attribute_enumerator.to_h
      _(resource_attributes).must_equal(expected_resource_attributes)
    end
  end

  describe '#merge' do
    it 'merges two resources into a third' do
      res1 = Resource.create('k1' => 'v1', 'k2' => 'v2')
      res2 = Resource.create('k3' => 'v3', 'k4' => 'v4')
      res3 = res1.merge(res2)

      _(res3.attribute_enumerator.to_h).must_equal({ 'k1' => 'v1', 'k2' => 'v2',
                                                     'k3' => 'v3', 'k4' => 'v4' })
      _(res1.attribute_enumerator.to_h).must_equal({ 'k1' => 'v1', 'k2' => 'v2' })
      _(res2.attribute_enumerator.to_h).must_equal({ 'k3' => 'v3', 'k4' => 'v4' })
    end

    it 'overwrites receiver\'s keys' do
      res1 = Resource.create('k1' => 'v1', 'k2' => 'v2')
      res2 = Resource.create('k2' => '2v2', 'k3' => '2v3')
      res3 = res1.merge(res2)

      _(res3.attribute_enumerator.to_h).must_equal({ 'k1' => 'v1',
                                                     'k2' => '2v2',
                                                     'k3' => '2v3' })
    end

    it 'still returns a Resource for the legacy call shape' do
      res = Resource.create('k1' => 'v1').merge(Resource.create('k2' => 'v2'))

      _(res).must_be_kind_of(Resource)
      _(res.attribute_enumerator.to_h).must_equal({ 'k1' => 'v1', 'k2' => 'v2' })
      _(res.merge_report.conflicts).must_be_empty
    end

    it 'records the source of each attribute in the merge report' do
      base = Resource.create({ 'k1' => 'v1' }, source: :default)
      overlay = Resource.create({ 'k2' => 'v2' }, source: :application)

      report = base.merge(overlay).merge_report

      _(report.sources).must_equal({ 'k1' => :default, 'k2' => :application })
      _(report.source_of('k2')).must_equal(:application)
      _(report.attributes).must_equal({ 'k1' => 'v1', 'k2' => 'v2' })
    end

    it 'labels unlabeled incoming attributes with the merge source argument' do
      base = Resource.create({ 'k1' => 'v1' }, source: :default)
      overlay = Resource.create('k2' => 'v2')

      report = base.merge(overlay, source: :environment).merge_report

      _(report.sources).must_equal({ 'k1' => :default, 'k2' => :environment })
    end

    it 'keeps more specific provenance already carried by the incoming resource' do
      base = Resource.create({ 'k1' => 'v1' }, source: :default)
      overlay = Resource.create({ 'k2' => 'v2' }, source: :telemetry_sdk)
                   .merge(Resource.create({ 'k3' => 'v3' }, source: :environment))

      report = base.merge(overlay, source: :application).merge_report

      _(report.sources).must_equal({ 'k1' => :default, 'k2' => :telemetry_sdk, 'k3' => :environment })
    end

    it 'records a conflict when the same key arrives with a different value' do
      base = Resource.create({ 'service.name' => 'from-default' }, source: :default)
      overlay = Resource.create({ 'service.name' => 'from-app' }, source: :application)

      report = base.merge(overlay).merge_report

      _(report.conflicts.length).must_equal(1)
      conflict = report.conflicts.first
      _(conflict.key).must_equal('service.name')
      _(conflict.kept_value).must_equal('from-app')
      _(conflict.kept_source).must_equal(:application)
      _(conflict.replaced_value).must_equal('from-default')
      _(conflict.replaced_source).must_equal(:default)
      _(report.attributes['service.name']).must_equal('from-app')
    end

    it 'does not record a conflict when both sides agree' do
      base = Resource.create({ 'k1' => 'same' }, source: :default)
      overlay = Resource.create({ 'k1' => 'same' }, source: :application)

      _(base.merge(overlay).merge_report.conflicts).must_be_empty
    end

    it 'treats an explicitly empty string as a real value in a conflict' do
      base = Resource.create({ 'deployment.environment' => 'production' }, source: :default)
      overlay = Resource.create({ 'deployment.environment' => '' }, source: :application)

      report = base.merge(overlay).merge_report

      _(report.attributes['deployment.environment']).must_equal('')
      _(report.conflicts.length).must_equal(1)
      _(report.conflicts.first.kept_value).must_equal('')
      _(report.conflicts.first.replaced_value).must_equal('production')
    end

    it 'does not record a conflict for a key supplied by only one side' do
      base = Resource.create({ 'k1' => 'v1' }, source: :default)
      overlay = Resource.create({ 'k2' => '' }, source: :application)

      _(base.merge(overlay).merge_report.conflicts).must_be_empty
    end

    it 'carries the schema_url of the incoming resource' do
      base = Resource.create({ 'k1' => 'v1' }, schema_url: 'https://opentelemetry.io/schemas/1.0.0', source: :default)
      overlay = Resource.create({ 'k2' => 'v2' }, schema_url: 'https://opentelemetry.io/schemas/1.21.0', source: :application)

      report = base.merge(overlay).merge_report

      _(report.schema_url).must_equal('https://opentelemetry.io/schemas/1.21.0')
      _(report.schema_source).must_equal(:application)
      _(report.schema_conflict).wont_be_nil
      _(report.schema_conflict.kept_schema_url).must_equal('https://opentelemetry.io/schemas/1.21.0')
      _(report.schema_conflict.replaced_schema_url).must_equal('https://opentelemetry.io/schemas/1.0.0')
      _(report.schema_conflict.replaced_source).must_equal(:default)
    end

    it 'adopts a schema_url without a conflict when the receiver has none' do
      base = Resource.create({ 'k1' => 'v1' }, source: :default)
      overlay = Resource.create({ 'k2' => 'v2' }, schema_url: 'https://opentelemetry.io/schemas/1.21.0', source: :application)

      report = base.merge(overlay).merge_report

      _(report.schema_url).must_equal('https://opentelemetry.io/schemas/1.21.0')
      _(report.schema_conflict).must_be_nil
    end

    it 'keeps the existing schema_url when the incoming resource has none' do
      base = Resource.create({ 'k1' => 'v1' }, schema_url: 'https://opentelemetry.io/schemas/1.21.0', source: :default)
      overlay = Resource.create({ 'k2' => 'v2' }, source: :application)

      report = base.merge(overlay).merge_report

      _(report.schema_url).must_equal('https://opentelemetry.io/schemas/1.21.0')
      _(report.schema_source).must_equal(:default)
      _(report.schema_conflict).must_be_nil
    end

    it 'does not record a schema conflict when both sides agree' do
      url = 'https://opentelemetry.io/schemas/1.21.0'
      base = Resource.create({ 'k1' => 'v1' }, schema_url: url, source: :default)
      overlay = Resource.create({ 'k2' => 'v2' }, schema_url: url, source: :application)

      report = base.merge(overlay).merge_report

      _(report.schema_url).must_equal(url)
      _(report.schema_conflict).must_be_nil
    end

    it 'returns a frozen merge report that outlives further merges' do
      base = Resource.create({ 'k1' => 'v1' }, source: :default)
      merged = base.merge(Resource.create({ 'k1' => 'v2' }, source: :application))
      report = merged.merge_report

      merged.merge(Resource.create({ 'k1' => 'v3' }, source: :later))

      _(report).must_be(:frozen?)
      _(report.attributes['k1']).must_equal('v2')
      _(report.sources['k1']).must_equal(:application)
      _(report.conflicts.length).must_equal(1)
    end

    it 'does not duplicate inherited conflicts when a merged resource is merged again' do
      merged = Resource.create({ 'k' => '1' }, source: :default)
                       .merge(Resource.create({ 'k' => '2' }, source: :application))

      _(merged.merge(merged).merge_report.conflicts.length).must_equal(1)
    end
  end

  describe '.default merge report' do
    before do
      Resource.instance_variable_set(:@default, nil)
    end

    after do
      Resource.instance_variable_set(:@default, nil)
    end

    it 'attributes SDK and process values to their layers' do
      report = Resource.default.merge_report

      _(report.sources['telemetry.sdk.name']).must_equal(:telemetry_sdk)
      _(report.sources['process.pid']).must_equal(:process)
      _(report.sources['service.name']).must_equal(:default)
    end

    it 'attributes environment overrides to the environment layer and records the conflict' do
      OpenTelemetry::TestHelpers.with_env('OTEL_RESOURCE_ATTRIBUTES' => 'service.name=from-env,deployment.environment=staging') do
        report = Resource.default.merge_report

        _(report.attributes['service.name']).must_equal('from-env')
        _(report.sources['service.name']).must_equal(:environment)
        _(report.sources['deployment.environment']).must_equal(:environment)

        conflict = report.conflicts.find { |c| c.key == 'service.name' }
        _(conflict).wont_be_nil
        _(conflict.kept_source).must_equal(:environment)
        _(conflict.replaced_value).must_equal('unknown_service')
        _(conflict.replaced_source).must_equal(:default)
      end
    end

    it 'lets OTEL_SERVICE_NAME override an OTEL_RESOURCE_ATTRIBUTES service.name' do
      OpenTelemetry::TestHelpers.with_env('OTEL_SERVICE_NAME' => 'svc-ok', 'OTEL_RESOURCE_ATTRIBUTES' => 'service.name=svc-bad') do
        report = Resource.default.merge_report

        _(report.attributes['service.name']).must_equal('svc-ok')
        _(report.sources['service.name']).must_equal(:environment)
      end
    end
  end
end
