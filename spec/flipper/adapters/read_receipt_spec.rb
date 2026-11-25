require 'flipper/adapters/read_receipt'

RSpec.describe Flipper::Adapters::ReadReceipt do
  subject(:receipt) { described_class.new }

  it 'starts empty' do
    expect(receipt.entries).to be_empty
    expect(receipt.failures).to be_empty
    expect(receipt.for(:stats)).to be_nil
  end

  it 'records where a key was read from' do
    receipt.record(:stats, source: :primary, status: :ok, operation: :get,
                   values: { 'stats' => { boolean: 'true' } })

    entry = receipt.for(:stats)
    expect(entry.key).to eq('stats')
    expect(entry.source).to be(:primary)
    expect(entry.status).to be(:ok)
    expect(entry.operation).to be(:get)
    expect(entry.value).to eq(boolean: 'true')
    expect(entry).to be_primary
    expect(entry).not_to be_memo
  end

  it 'records a degraded secondary read together with the error' do
    error = RuntimeError.new('primary down')
    receipt.record(%i(stats search), source: :secondary, status: :degraded,
                   operation: :get_multi,
                   values: { 'stats' => {}, 'search' => {} }, error: error)

    expect(receipt.for(:stats).source).to be(:secondary)
    expect(receipt.for(:search).status).to be(:degraded)
    expect(receipt.for(:stats).error).to be(error)
    expect(receipt.for(:stats)).to be_degraded
  end

  it 'keeps reserved entries out of #entries' do
    receipt.record(:stats, source: :primary, status: :ok, operation: :get,
                   values: { 'stats' => {} })
    receipt.record(described_class.features_key,
                   source: :primary, status: :ok, operation: :features)

    expect(receipt.entries.keys).to eq(['stats'])
    expect(receipt.features_entry).not_to be_nil
  end

  it 'appends every failure point instead of overwriting them' do
    error_one = RuntimeError.new('one')
    error_two = RuntimeError.new('two')
    receipt.record_failure(:get, [:stats], error_one)
    receipt.record_failure(:get_multi, %i[stats search], error_two)

    expect(receipt.failures.map(&:operation)).to eq([:get, :get_multi])
    expect(receipt.failures.map(&:keys)).to eq([%w(stats), %w(stats search)])
    expect(receipt.failures.first.error).to be(error_one)
  end

  describe '#restamp and #restore' do
    it 'preserves the active provenance behind a memo hit' do
      error = RuntimeError.new('down')
      receipt.record(:stats, source: :secondary, status: :degraded,
                     operation: :get_multi, values: { 'stats' => {} }, error: error)

      receipt.restamp(:stats, :get)
      entry = receipt.for(:stats)
      expect(entry.source).to be(:memo)
      expect(entry.active_source).to be(:secondary)
      expect(entry.active_status).to be(:degraded)
      expect(entry).to be_memo

      receipt.restore(:stats)
      entry = receipt.for(:stats)
      expect(entry.source).to be(:secondary)
      expect(entry.status).to be(:degraded)
      expect(entry.original_source).to be_nil
      expect(entry.original_status).to be_nil
    end

    it 'ignores keys that were never read' do
      expect { receipt.restamp(:never_read, :get) }.not_to raise_error
      expect(receipt.for(:never_read)).to be_nil
    end
  end

  it '#clear empties both entries and failures' do
    receipt.record(:stats, source: :primary, status: :ok,
                   operation: :get, values: { 'stats' => {} })
    receipt.record_failure(:get, [:stats], RuntimeError.new)

    receipt.clear

    expect(receipt.entries).to be_empty
    expect(receipt.failures).to be_empty
  end
end
