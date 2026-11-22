# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::SDK::Trace::Export::SpanExporter do
  export = OpenTelemetry::SDK::Trace::Export

  let(:span_data1) { OpenTelemetry::SDK::Trace::SpanData.new({ name: 'name1' }) }
  let(:span_data2) { OpenTelemetry::SDK::Trace::SpanData.new({ name: 'name2' }) }
  let(:spans)      { [span_data1, span_data2] }
  let(:exporter)   { export::SpanExporter.new }

  it 'accepts an Array of SpanData as arg to #export and succeeds' do
    _(exporter.export(spans)).must_equal export::SUCCESS
  end

  it 'accepts an Enumerable of SpanData as arg to #export and succeeds' do
    enumerable = Struct.new(:span0, :span1).new(spans[0], spans[1])

    _(exporter.export(enumerable)).must_equal export::SUCCESS
  end

  it 'accepts calls to #shutdown' do
    exporter.shutdown
  end

  it 'fails to export after shutdown' do
    exporter.shutdown
    _(exporter.export(spans)).must_equal export::FAILURE
  end
end

describe OpenTelemetry::SDK::Trace::Export::ExportResult do
  export = OpenTelemetry::SDK::Trace::Export

  it 'reports per-span dispositions with counts' do
    result = export::ExportResult.new(accepted: [0], rejected: [1], retryable: [2])

    _(result.accepted).must_equal [0]
    _(result.rejected).must_equal [1]
    _(result.retryable).must_equal [2]
    _(result.accepted_count).must_equal 1
    _(result.rejected_count).must_equal 1
    _(result.retryable_count).must_equal 1
    _(result.success?).must_equal false
  end

  it 'treats unlisted spans as accepted' do
    result = export::ExportResult.new(retryable: [1])

    _(result.accepted).must_be_empty
    _(result.retryable).must_equal [1]
    _(result.success?).must_equal false
  end

  it 'is successful when nothing is rejected or retryable' do
    result = export::ExportResult.new(accepted: [0, 1])

    _(result.success?).must_equal true
  end

  it 'defaults to an empty, successful receipt' do
    result = export::ExportResult.new

    _(result.accepted).must_be_empty
    _(result.rejected).must_be_empty
    _(result.retryable).must_be_empty
    _(result.success?).must_equal true
  end

  it 'resolves overlapping indexes with rejected over retryable over accepted' do
    result = export::ExportResult.new(accepted: [0, 1], retryable: [1, 2], rejected: [2])

    _(result.rejected).must_equal [2]
    _(result.retryable).must_equal [1]
    _(result.accepted).must_equal [0]
  end

  it 'de-duplicates indexes' do
    result = export::ExportResult.new(accepted: [0, 0], retryable: [1, 1])

    _(result.accepted).must_equal [0]
    _(result.retryable).must_equal [1]
  end

  it 'ignores indexes that are not usable batch positions' do
    result = export::ExportResult.new(accepted: [0, -1, 'x'], rejected: [-2], retryable: [nil])

    _(result.accepted).must_equal [0]
    _(result.rejected).must_be_empty
    _(result.retryable).must_be_empty
  end
end
