# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::Baggage::Propagation::Serializer do
  let(:serializer) { OpenTelemetry::Baggage::Propagation::Serializer.new }
  let(:entry) { OpenTelemetry::Baggage::Entry }

  describe '#decode' do
    it 'decodes members into key and entry pairs' do
      entries = serializer.decode('key1=val1,key2=val2;prop=1')

      _(entries.map(&:first)).must_equal(%w[key1 key2])
      _(entries[0][1].value).must_equal('val1')
      _(entries[1][1].metadata).must_equal('prop=1')
    end

    it 'keeps the last member for a repeated key' do
      entries = serializer.decode('key=first,key=second')

      _(entries.size).must_equal(2)
      _(entries.last[1].value).must_equal('second')
    end

    it 'drops members that are not key-value pairs' do
      entries = serializer.decode('key1=val1,novalue,=novalue,key2=val2')

      _(entries.map(&:first)).must_equal(%w[key1 key2])
    end

    it 'drops a member with malformed percent-encoding' do
      entries = serializer.decode('key1=val1,key2=%G1,key3=%')

      _(entries.map(&:first)).must_equal(%w[key1])
    end

    it 'accepts empty values' do
      entries = serializer.decode('key1=')

      _(entries.first[1].value).must_equal('')
    end

    it 'does not cut a member that straddles the total limit' do
      member = "key=#{'%E5%80%BC' * 1000}"
      entries = serializer.decode("ok=1,#{member},after=2")

      _(entries.map(&:first)).must_equal(%w[ok])
    end

    it 'normalizes the whitespace and escaping of properties' do
      entries = serializer.decode('key=val; p1 = 1 ; p2 ;;p3=a=b')

      _(entries.first[1].metadata).must_equal('p1=1;p2;p3=a=b')
    end
  end

  describe '#encode' do
    it 'encodes a single member without a trailing comma' do
      _(serializer.encode({ 'key' => entry.new('val') })).must_equal('key=val')
    end

    it 'escapes the delimiters of keys and values' do
      baggage = { 'a:b' => entry.new('v,1;s=2 %') }

      _(serializer.encode(baggage)).must_equal('a%3Ab=v%2C1%3Bs%3D2%20%25')
    end

    it 'escapes multibyte values' do
      _(serializer.encode({ 'k' => entry.new('值') })).must_equal('k=%E5%80%BC')
    end

    it 'emits one member per distinct encoded key' do
      baggage = { 'k' => entry.new('first'), :k => entry.new('second') }

      _(serializer.encode(baggage)).must_equal('k=second')
    end
  end
end
