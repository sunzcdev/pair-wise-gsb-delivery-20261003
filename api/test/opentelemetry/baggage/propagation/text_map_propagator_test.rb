# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::Baggage::Propagation::TextMapPropagator do
  let(:propagator) do
    OpenTelemetry::Baggage::Propagation::TextMapPropagator.new
  end
  let(:header_key) do
    'baggage'
  end
  let(:context_key) do
    OpenTelemetry::Baggage::Propagation::ContextKeys.baggage_key
  end

  describe '#extract' do
    describe 'valid headers' do
      it 'extracts key-value pairs' do
        carrier = { header_key => 'key1=val1,key2=val2' }
        context = propagator.extract(carrier, context: Context.empty)
        assert_value(context, 'key1', 'val1')
        assert_value(context, 'key2', 'val2')
      end

      it 'extracts entries with spaces' do
        carrier = { header_key => ' key1  =  val1,  key2=val2 ' }
        context = propagator.extract(carrier, context: Context.empty)
        assert_value(context, 'key1', 'val1')
        assert_value(context, 'key2', 'val2')
      end

      it 'preserves properties' do
        carrier = { header_key => 'key1=val1,key2=val2;prop1=propval1;prop2=propval2' }
        context = propagator.extract(carrier, context: Context.empty)
        assert_value(context, 'key1', 'val1')
        assert_value(context, 'key2', 'val2', 'prop1=propval1;prop2=propval2')
      end

      it 'extracts urlencoded entries' do
        carrier = { header_key => 'key%3A1=val1%2C1,key%3A2=val2%2C2' }
        context = propagator.extract(carrier, context: Context.empty)
        assert_value(context, 'key:1', 'val1,1')
        assert_value(context, 'key:2', 'val2,2')
      end

      it 'extracts values containing equal signs' do
        carrier = { header_key => 'key1=a=b=c,key2=d' }
        context = propagator.extract(carrier, context: Context.empty)
        assert_value(context, 'key1', 'a=b=c')
        assert_value(context, 'key2', 'd')
      end

      it 'keeps the last entry when a key is repeated' do
        carrier = { header_key => 'key1=first,key1=second,key2=other' }
        context = propagator.extract(carrier, context: Context.empty)
        assert_value(context, 'key1', 'second')
        assert_value(context, 'key2', 'other')
      end

      it 'replaces invalid utf-8 sequences in percent-encoded values' do
        carrier = { header_key => 'key1=%FF%FE' }
        context = propagator.extract(carrier, context: Context.empty)
        assert_value(context, 'key1', "\u{FFFD}\u{FFFD}")
      end

      it 'skips a member with malformed percent-encoding but keeps the rest' do
        carrier = { header_key => 'key1=val1,key2=%G1,key3=val3' }
        context = propagator.extract(carrier, context: Context.empty)
        assert_value(context, 'key1', 'val1')
        _(OpenTelemetry::Baggage.value('key2', context: context)).must_be_nil
        assert_value(context, 'key3', 'val3')
      end

      it 'skips members without a value' do
        carrier = { header_key => 'key1=val1,key2,key3=val3' }
        context = propagator.extract(carrier, context: Context.empty)
        assert_value(context, 'key1', 'val1')
        _(OpenTelemetry::Baggage.value('key2', context: context)).must_be_nil
        assert_value(context, 'key3', 'val3')
      end

      it 'combines all baggage header fields into one list' do
        carrier = { header_key => ['key1=val1;prop1=propval1', 'key2=val2', 'key1=val1up'] }
        context = propagator.extract(carrier, context: Context.empty, getter: list_getter)
        assert_value(context, 'key1', 'val1up')
        assert_value(context, 'key2', 'val2')
      end

      it 'applies the entry limit to the combination of all header fields' do
        carrier = { header_key => (0..180).map { |i| "k#{i}=v#{i}" }.each_slice(90).map { |slice| slice.join(',') } }
        context = propagator.extract(carrier, context: Context.empty, getter: list_getter)

        _(OpenTelemetry::Baggage.values(context: context).size).must_equal(180)
        _(OpenTelemetry::Baggage.value('k180', context: context)).must_be_nil
      end

      it 'applies the total length limit to the combination of all header fields' do
        keys = (0..81).map { |i| "k#{i.to_s.rjust(48, '0')}" }
        values = (0..81).map { |i| "v#{i.to_s.rjust(48, '0')}" }
        members = keys.zip(values).map { |k, v| "#{k}=#{v}" }

        carrier = { header_key => members.each_slice(41).map { |slice| slice.join(',') } }
        context = propagator.extract(carrier, context: Context.empty, getter: list_getter)

        _(OpenTelemetry::Baggage.values(context: context).size).must_equal(81)
      end

      it 'drops an oversized member whole instead of decoding part of it' do
        # The oversized member holds a percent-encoded multibyte value that the
        # truncation window falls inside. It must not be decoded partially, so
        # no replacement character may show up anywhere in the result.
        oversized = "big=#{'%E5%80%BC' * 1000}"
        header = "key1=val1,#{oversized},key3=val3"

        _(header.bytesize).must_be(:>, 8192)

        context = propagator.extract({ header_key => header }, context: Context.empty)
        values = OpenTelemetry::Baggage.values(context: context)

        assert_value(context, 'key1', 'val1')
        _(values).wont_include('big')
        _(values.values).wont_include("\u{FFFD}")
      end

      it 'keeps entries already in the context that the header does not mention' do
        existing = OpenTelemetry::Baggage.build(context: Context.empty) do |b|
          b.set_value('local', 'kept')
          b.set_value('shared', 'old')
        end

        context = propagator.extract({ header_key => 'shared=new,remote=val' }, context: existing)

        assert_value(context, 'local', 'kept')
        assert_value(context, 'shared', 'new')
        assert_value(context, 'remote', 'val')
      end

      it 'reads a single field from a getter without get_all' do
        carrier = { header_key => 'key1=val1,key2=val2' }
        context = propagator.extract(carrier, context: Context.empty, getter: get_only_getter)

        assert_value(context, 'key1', 'val1')
        assert_value(context, 'key2', 'val2')
      end

      it 'reads every value from a carrier that stores the field as an array' do
        carrier = { header_key => %w[key1=val1 key2=val2] }
        context = propagator.extract(carrier, context: Context.empty)

        assert_value(context, 'key1', 'val1')
        assert_value(context, 'key2', 'val2')
      end
    end

    describe 'invalid or no-op headers' do
      it 'returns the same context object when the headers are not present' do
        carrier = {}
        empty_context = Context.empty
        context = propagator.extract(carrier, context: empty_context)
        _(context.object_id).must_equal(empty_context.object_id)
      end

      it 'returns the same context object when the baggage value is a 0-length string' do
        carrier = { header_key => '' }
        empty_context = Context.empty
        context = propagator.extract(carrier, context: empty_context)
        _(context.object_id).must_equal(empty_context.object_id)
      end

      it 'does not test for an "empty" string and still replaces the context' do
        carrier = { header_key => '   ' }
        empty_context = Context.empty
        context = propagator.extract(carrier, context: empty_context)
        _(context.object_id).wont_equal(empty_context.object_id)
      end

      it 'returns the original context when the getter raises' do
        carrier = { header_key => 'key1=val1' }
        existing = OpenTelemetry::Baggage.build(context: Context.empty) { |b| b.set_value('local', 'kept') }

        context = propagator.extract(carrier, context: existing, getter: raising_getter)

        _(context.object_id).must_equal(existing.object_id)
        assert_value(context, 'local', 'kept')
      end
    end

    describe 'enforced limits' do
      it 'enforces max of 180 name-value pairs' do
        header = (0..180).map { |i| "k#{i}=v#{i}" }.join(',')
        context = propagator.extract({ header_key => header }, context: Context.empty)

        180.times { |i| _(OpenTelemetry::Baggage.value("k#{i}", context: context)).must_equal("v#{i}") }
        _(OpenTelemetry::Baggage.value('k180', context: context)).must_be_nil
      end

      it 'counts the entries it keeps, not the candidates it skips' do
        header = (',' * 200) + (0...200).map { |i| "k#{i}=v#{i}" }.join(',')
        context = propagator.extract({ header_key => header }, context: Context.empty)

        _(OpenTelemetry::Baggage.values(context: context).size).must_equal(180)
      end

      it 'enforces max entry length of 4096' do
        oversized = "key1=#{'x' * 4092}" # 4097 chars including 'key1='
        header = "#{oversized},key2=val2"
        context = propagator.extract({ header_key => header }, context: Context.empty)

        _(OpenTelemetry::Baggage.value('key1', context: context)).must_be_nil
        _(OpenTelemetry::Baggage.value('key2', context: context)).must_equal('val2')
      end

      it 'enforces total length of 8192 chars' do
        keys = (0..81).map { |i| "k#{i.to_s.rjust(48, '0')}" }
        values = (0..81).map { |i| "v#{i.to_s.rjust(48, '0')}" }
        header = keys.zip(values).map { |k, v| "#{k}=#{v}" }.join(',')

        context = propagator.extract({ header_key => header }, context: Context.empty)

        81.times { |i| _(OpenTelemetry::Baggage.value(keys[i], context: context)).wont_be_nil }
        _(OpenTelemetry::Baggage.value(keys.last, context: context)).must_be_nil
      end
    end
  end

  describe '#inject' do
    it 'injects baggage' do
      context = OpenTelemetry::Baggage.build(context: OpenTelemetry::Context.empty) do |b|
        b.set_value('key1', 'val1')
        b.set_value('key2', 'val2')
      end

      carrier = {}
      propagator.inject(carrier, context: context)

      _(carrier[header_key]).must_equal('key1=val1,key2=val2')
    end

    it 'injects numeric baggage' do
      context = OpenTelemetry::Baggage.build(context: OpenTelemetry::Context.empty) do |b|
        b.set_value('key1', 1)
        b.set_value('key2', 3.14)
      end

      carrier = {}
      propagator.inject(carrier, context: context)

      _(carrier[header_key]).must_equal('key1=1,key2=3.14')
    end

    it 'injects boolean baggage' do
      context = OpenTelemetry::Baggage.build(context: OpenTelemetry::Context.empty) do |b|
        b.set_value('key1', true)
        b.set_value('key2', false)
      end

      carrier = {}
      propagator.inject(carrier, context: context)

      _(carrier[header_key]).must_equal('key1=true,key2=false')
    end

    it 'does not inject baggage key is not present' do
      carrier = {}
      propagator.inject(carrier, context: Context.empty)
      _(carrier).must_be(:empty?)
    end

    it 'injects properties' do
      context = OpenTelemetry::Baggage.build(context: OpenTelemetry::Context.empty) do |b|
        b.set_value('key1', 'val1')
        b.set_value('key2', 'val2', metadata: 'prop1=propval1;prop2=propval2')
      end

      carrier = {}
      propagator.inject(carrier, context: context)
      _(carrier[header_key]).must_equal('key1=val1,key2=val2;prop1=propval1;prop2=propval2')
    end

    it 'percent-encodes keys and values that contain delimiters' do
      context = OpenTelemetry::Baggage.build(context: OpenTelemetry::Context.empty) do |b|
        b.set_value('key:1', 'val,1;x=y z')
      end

      carrier = {}
      propagator.inject(carrier, context: context)
      _(carrier[header_key]).must_equal('key%3A1=val%2C1%3Bx%3Dy%20z')
    end

    it 'encodes the percent sign so that values round trip exactly' do
      context = OpenTelemetry::Baggage.build(context: OpenTelemetry::Context.empty) do |b|
        b.set_value('key1', '%2C')
      end

      carrier = {}
      propagator.inject(carrier, context: context)
      _(carrier[header_key]).must_equal('key1=%252C')

      round_tripped = propagator.extract(carrier, context: Context.empty)
      assert_value(round_tripped, 'key1', '%2C')
    end

    it 'round trips multibyte keys and values through a carrier' do
      context = OpenTelemetry::Baggage.build(context: OpenTelemetry::Context.empty) do |b|
        b.set_value('用户', '值 with space,comma;semis=eq 💼')
      end

      carrier = {}
      propagator.inject(carrier, context: context)
      _(carrier[header_key]).must_equal('%E7%94%A8%E6%88%B7=%E5%80%BC%20with%20space%2Ccomma%3Bsemis%3Deq%20%F0%9F%92%BC')

      round_tripped = propagator.extract(carrier, context: Context.empty)
      assert_value(round_tripped, '用户', '值 with space,comma;semis=eq 💼')
    end

    it 'drops an entry that does not fit instead of splitting it' do
      # Each entry is 1505 bytes once percent-encoded, so five of them fit in
      # the 8192 byte budget. The sixth entry is dropped whole and the smaller
      # seventh entry still fits.
      context = OpenTelemetry::Baggage.build(context: OpenTelemetry::Context.empty) do |b|
        b.set_value('key1', 'é' * 500)
        b.set_value('key2', 'é' * 500)
        b.set_value('key3', 'é' * 500)
        b.set_value('key4', 'é' * 500)
        b.set_value('key5', 'é' * 500)
        b.set_value('key6', 'é' * 500)
        b.set_value('key7', 'val7')
      end

      carrier = {}
      propagator.inject(carrier, context: context)
      result = carrier[header_key]

      _(result).wont_include('key6')
      _(result).must_include('key7=val7')
      _(result.bytesize).must_be(:<=, 8192)
    end

    it 'counts encoded bytes, not characters, against the total length limit' do
      # Each value is 681 characters but 4091 bytes once percent-encoded. Only
      # two of the three entries fit in the 8192 byte budget; counting
      # characters instead would have kept all three.
      context = OpenTelemetry::Baggage.build(context: OpenTelemetry::Context.empty) do |b|
        b.set_value('key1', 'é' * 681)
        b.set_value('key2', 'é' * 681)
        b.set_value('key3', 'é' * 681)
      end

      carrier = {}
      propagator.inject(carrier, context: context)
      result = carrier[header_key]

      _(result).must_equal("key1=#{'%C3%A9' * 681},key2=#{'%C3%A9' * 681}")
      _(result.bytesize).must_equal(8183)
    end

    it 'enforces max of 180 name-value pairs' do
      context = OpenTelemetry::Baggage.build(context: OpenTelemetry::Context.empty) do |b|
        181.times do |i|
          b.set_value("k#{i}", "v#{i}")
        end
      end

      carrier = {}
      propagator.inject(carrier, context: context)
      result = carrier[header_key]

      # expect keys indexed from 0 to 180 to be in baggage, but only 0 to 179 in the result
      _(OpenTelemetry::Baggage.value('k180', context: context)).wont_be_nil
      180.times do |i|
        _(result).must_include("k#{i}")
      end
      _(result).wont_include('k180')
    end

    it 'enforces max entry length of 4096' do
      context = OpenTelemetry::Baggage.build(context: OpenTelemetry::Context.empty) do |b|
        b.set_value('key1', 'x' * 4092)
        b.set_value('key2', 'val2')
      end

      carrier = {}
      propagator.inject(carrier, context: context)
      result = carrier[header_key]

      _(result).wont_include('key1')
      _(result).must_include('key2')
    end

    it 'enforces total length of 8192 chars' do
      # each entry will be 100 chars long including '=' and ','
      # the last entry will be 99 chars long (it doesn't have a trailing ',')
      keys = (0..81).map { |i| "k#{i.to_s.rjust(48, '0')}" }
      values = (0..81).map { |i| "v#{i.to_s.rjust(48, '0')}" }

      context = OpenTelemetry::Baggage.build(context: OpenTelemetry::Context.empty) do |b|
        keys.zip(values).each { |k, v| b.set_value(k, v) }
      end

      carrier = {}
      propagator.inject(carrier, context: context)
      result = carrier[header_key]

      keys.take(81).each { |k| _(result).must_include(k) }
      _(result).wont_include(keys.last)
      _(result.size).must_equal(8099)
    end
  end
end

def assert_value(context, key, value, metadata = nil)
  entry = OpenTelemetry::Baggage.raw_entries(context: context)[key]
  _(entry).wont_be_nil
  _(entry.value).must_equal(value)
  if metadata.nil?
    _(entry.metadata).must_be_nil
  else
    _(entry.metadata).must_equal(metadata)
  end
end

def list_getter
  @list_getter ||= ListGetter.new
end

def get_only_getter
  @get_only_getter ||= GetOnlyGetter.new
end

def raising_getter
  @raising_getter ||= RaisingGetter.new
end

# A getter that exposes every value of a field, the way a carrier with repeated
# headers does.
class ListGetter
  def get(carrier, key)
    Array(carrier[key]).first
  end

  def get_all(carrier, key)
    Array(carrier[key])
  end

  def keys(carrier)
    carrier.keys
  end
end

# A getter that predates the get_all operation.
class GetOnlyGetter
  def get(carrier, key)
    carrier[key]
  end

  def keys(carrier)
    carrier.keys
  end
end

# A getter whose carrier raises on access.
class RaisingGetter
  def get(_carrier, _key)
    raise 'broken carrier'
  end

  def keys(carrier)
    carrier.keys
  end
end
