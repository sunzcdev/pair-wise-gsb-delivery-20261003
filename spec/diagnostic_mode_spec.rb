# frozen_string_literal: true
require 'spec_helper'

describe 'Diagnostic mode' do
  let(:data) do
    {
      'service' => {
        'endpoint' => nil,
        'tags' => %w[a b],
        'limits' => [{ 'cpu' => 1 }, { 'mem' => 2 }]
      }
    }
  end

  def diagnostic(expression)
    JMESPath.diagnostic_search(expression, data)
  end

  describe 'missing vs null' do
    it 'returns nil for explicit null values' do
      expect(diagnostic('service.endpoint')).to be(nil)
    end

    it 'reports absent keys as serializable hashes' do
      expect(diagnostic('service.timeout')).to eq('$missing' => 'service.timeout')
    end

    it 'reports multi-level paths that go missing mid-path' do
      expect(diagnostic('service.timeout.retries')).to(
        eq('$missing' => 'service.timeout.retries')
      )
    end

    it 'reports out of range array indices' do
      expect(diagnostic('service.tags[5]')).to eq('$missing' => 'service.tags[5]')
    end

    it 'never leaks the sentinel object into results' do
      result = diagnostic('{ep: service.endpoint, to: service.timeout, list: [service.missing]}')
      expect(result).to eq(
        'ep' => nil,
        'to' => { '$missing' => 'service.timeout' },
        'list' => [{ '$missing' => 'service.missing' }]
      )
      expect(JSON.parse(JSON.generate(result))).to eq(result)
    end
  end

  describe 'comparisons' do
    it 'treats missing as unequal to null' do
      expect(diagnostic('service.missing == `null`')).to be(false)
      expect(diagnostic('service.missing != `null`')).to be(true)
    end

    it 'treats explicit null as equal to null' do
      expect(diagnostic('service.endpoint == `null`')).to be(true)
    end

    it 'treats two missing values as equal' do
      expect(diagnostic('service.missing == service.other')).to be(true)
      expect(diagnostic('service.missing != service.other')).to be(false)
    end

    it 'returns nil for ordering comparisons against missing' do
      expect(diagnostic('service.missing > `1`')).to be(nil)
    end

    it 'excludes elements filtered on missing fields' do
      expect(diagnostic('service.limits[?missing == `1`].cpu')).to eq([])
      expect(diagnostic('service.limits[?cpu].mem')).to eq([])
    end
  end

  describe 'logic' do
    it 'treats missing as falsey' do
      expect(diagnostic('service.missing || `1`')).to eq(1)
      expect(diagnostic('service.missing && `1`')).to eq('$missing' => 'service.missing')
      expect(diagnostic('service.limits[? !cpu].mem')).to eq([2])
    end
  end

  describe 'projections' do
    it 'drops missing elements from projections' do
      expect(diagnostic('service.limits[*].cpu')).to eq([1])
    end

    it 'propagates missing projection targets' do
      expect(diagnostic('service.missing[].x')).to eq('$missing' => 'service.missing')
    end
  end

  describe 'functions' do
    it 'propagates missing arguments instead of raising type errors' do
      expect(diagnostic('length(service.missing)')).to eq('$missing' => 'service.missing')
      expect(diagnostic('abs(service.missing)')).to eq('$missing' => 'service.missing')
    end

    it 'still evaluates functions over present values' do
      expect(diagnostic('length(service.tags)')).to eq(2)
    end
  end

  describe 'default search' do
    it 'keeps the existing semantics on the same input' do
      expect(JMESPath.search('service.timeout.retries', data)).to be(nil)
      expect(JMESPath.search('service.endpoint', data)).to be(nil)
      expect(JMESPath.search('service.missing == `null`', data)).to be(true)
      expect(JMESPath.search('service.limits[*].cpu', data)).to eq([1])
      expect { JMESPath.search('length(service.missing)', data) }.to(
        raise_error(JMESPath::Errors::InvalidTypeError)
      )
    end

    it 'is not affected by a previous diagnostic search' do
      JMESPath.diagnostic_search('service.timeout', data)
      expect(JMESPath.search('service.timeout', data)).to be(nil)
    end
  end
end
