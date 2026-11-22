# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::SDK::Trace::Event do
  Event = OpenTelemetry::SDK::Trace::Event

  let(:event) { Event.new('name', { 'key' => 'value' }, 123) }

  it 'holds a name, attributes, and timestamp' do
    _(event.name).must_equal('name')
    _(event.attributes).must_equal({ 'key' => 'value' })
    _(event.timestamp).must_equal(123)
  end

  it 'is not preferred by default' do
    assert_nil(event.preferred)
    _(event.preferred?).must_equal(false)
  end

  it 'reports the preferred marker' do
    _(Event.new('name', {}, 1, true).preferred?).must_equal(true)
    _(Event.new('name', {}, 1, false).preferred?).must_equal(false)
  end
end
