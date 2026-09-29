# frozen_string_literal: true
require 'spec_helper'
require 'json'

module JMESPath
  describe '.diagnose' do
    let(:data) do
      {
        'service' => {
          'endpoint' => 'https://example.com',
          'retries' => nil,
          'tags' => [{ 'name' => 'a' }, { 'name' => 'b', 'weight' => 2 }]
        }
      }
    end

    def diagnose(expression)
      JMESPath.diagnose(expression, data)
    end

    it 'returns a JSON serializable report' do
      report = diagnose('service.timeout')
      expect(JSON.parse(JSON.generate(report))).to eq(report)
    end

    it 'does not leak the MISSING sentinel into the report' do
      report = diagnose('{a: service.timeout, b: service.tags[*].weight}')
      expect(JSON.generate(report)).not_to include('MISSING')
      expect(report['value']).to eq('a' => nil, 'b' => [2])
    end

    it 'reports an explicit null without diagnostics' do
      expect(diagnose('service.retries')).to eq(
        'value' => nil, 'diagnostics' => []
      )
    end

    it 'distinguishes a missing key from an explicit null' do
      expect(diagnose('service.timeout')).to eq(
        'value' => nil,
        'diagnostics' => [
          { 'path' => 'service.timeout', 'reason' => 'missing_key' }
        ]
      )
    end

    it 'records the path where a deep path goes missing in the middle' do
      report = diagnose('service.timeout.seconds')
      expect(report['value']).to be(nil)
      expect(report['diagnostics']).to eq(
        [{ 'path' => 'service.timeout', 'reason' => 'missing_key' }]
      )
    end

    it 'reports out of bounds indices' do
      report = diagnose('service.tags[9]')
      expect(report['value']).to be(nil)
      expect(report['diagnostics']).to eq(
        [{ 'path' => 'service.tags[9]', 'reason' => 'index_out_of_bounds' }]
      )
    end

    describe 'comparisons' do
      it 'treats missing as different from null' do
        expect(diagnose('service.timeout == `null`')['value']).to be(false)
        expect(diagnose('service.timeout != `null`')['value']).to be(true)
        expect(diagnose('service.retries == `null`')['value']).to be(true)
      end

      it 'keeps ordering comparisons nil for missing values' do
        expect(diagnose('service.timeout > `1`')['value']).to be(nil)
      end
    end

    describe 'logic operators' do
      it 'treats missing as falsey' do
        expect(diagnose('!(service.timeout)')['value']).to be(true)
        expect(diagnose('service.timeout || `true`')['value']).to be(true)
        expect(diagnose('service.endpoint && service.timeout')['value']).to be(nil)
      end
    end

    describe 'projections' do
      it 'drops missing elements and records them' do
        report = diagnose('service.tags[*].weight')
        expect(report['value']).to eq([2])
        expect(report['diagnostics']).to eq(
          [{ 'path' => 'weight', 'reason' => 'missing_key' }]
        )
      end
    end

    describe 'function arguments' do
      it 'coerces missing arguments to null and records it' do
        report = diagnose('not_null(service.timeout, `3`)')
        expect(report['value']).to eq(3)
        expect(report['diagnostics']).to include(
          'path' => nil,
          'reason' => 'function_argument',
          'detail' => 'missing value coerced to null in not_null() argument'
        )
      end
    end

    describe 'default search semantics' do
      it 'is unaffected by the diagnostic mode' do
        expect(JMESPath.search('service.timeout', data)).to be(nil)
        expect(JMESPath.search('service.timeout == `null`', data)).to be(true)
        expect(JMESPath.search('service.tags[*].weight', data)).to eq([2])
        expect(JMESPath.search('not_null(service.timeout, `3`)', data)).to eq(3)
      end
    end
  end
end
