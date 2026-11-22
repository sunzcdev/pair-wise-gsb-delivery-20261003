# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::Trace::Link do
  Link = OpenTelemetry::Trace::Link
  let(:span_context) { OpenTelemetry::Trace::SpanContext.new }
  describe '.new' do
    it 'accepts a span_context' do
      link = Link.new(span_context)
      _(link.span_context).must_equal(span_context)
    end

    it 'returns a link with the given span context and attributes' do
      link = Link.new(span_context, '1' => 1)
      _(link.attributes).must_equal({ '1' => 1 })
      _(link.span_context).must_equal(span_context)
    end

    it 'returns a link with no attributes by default' do
      link = Link.new(span_context)
      _(link.attributes).must_equal({})
    end

    it 'allows array-valued attributes' do
      attributes = { 'foo' => [1, 2, 3] }
      link = Link.new(span_context, attributes)
      _(link.attributes).must_equal(attributes)
    end

    it 'is not preferred by default' do
      _(Link.new(span_context).preferred?).must_equal(false)
    end

    it 'accepts a preferred flag' do
      _(Link.new(span_context, nil, preferred: true).preferred?).must_equal(true)
    end

    it 'accepts a preferred flag alongside a trailing attributes hash' do
      link = Link.new(span_context, '1' => 1, preferred: true)
      _(link.attributes).must_equal({ '1' => 1 })
      _(link.preferred?).must_equal(true)
    end
  end

  describe '.attributes' do
    it 'returns and freezes attributes passed in' do
      attributes = { 'foo' => 'bar', 'bar' => 'baz' }
      link = Link.new(span_context, attributes)
      _(link.attributes).must_equal(attributes)
      _(link.attributes).must_be(:frozen?)
    end
  end

  describe '#==' do
    it 'is true for links with the same context and attributes' do
      _(Link.new(span_context, '1' => 1)).must_equal(Link.new(span_context, '1' => 1))
    end

    it 'is true regardless of the preferred marker' do
      _(Link.new(span_context, '1' => 1)).must_equal(Link.new(span_context, '1' => 1, preferred: true))
    end
  end
end
