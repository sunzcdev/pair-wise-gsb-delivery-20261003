require 'flipper/adapters/failover'

# Internal: Primary adapter whose failures can be scripted per operation while
# gate values remain in a real memory adapter.
class ScriptedFailoverPrimary
  def initialize(store)
    @store = store
    @failures = Hash.new { |hash, key| hash[key] = [] }
  end

  def fail(operation, error, count: 1)
    @failures[operation].concat(Array.new(count) { error })
  end

  def get(feature)
    maybe_raise(:get)
    @store.get(feature)
  end

  def get_multi(features)
    maybe_raise(:get_multi)
    @store.get_multi(features)
  end

  def get_all(**kwargs)
    maybe_raise(:get_all)
    @store.get_all(**kwargs)
  end

  def features
    maybe_raise(:features)
    @store.features
  end

  def add(feature); @store.add(feature); end
  def remove(feature); @store.remove(feature); end
  def clear(feature); @store.clear(feature); end
  def enable(feature, gate, thing); @store.enable(feature, gate, thing); end
  def disable(feature, gate, thing); @store.disable(feature, gate, thing); end
  def read_only?; false; end
  def sync_version; @store.sync_version; end
  def sync_version=(version); @store.sync_version = version; end
  def adapter_stack; "scripted_primary -> #{@store.adapter_stack}"; end
  def name; :scripted_primary; end

  private

  def maybe_raise(operation)
    error = @failures[operation].shift
    raise error if error
  end
end

RSpec.describe Flipper::Adapters::Failover, 'read receipt' do
  let(:primary_store) { Flipper::Adapters::Memory.new }
  let(:secondary_store) { Flipper::Adapters::Memory.new }
  let(:primary) { ScriptedFailoverPrimary.new(primary_store) }
  let(:secondary) { secondary_store }
  let(:adapter) { described_class.new(primary, secondary, receipt: true) }
  let(:flipper) { Flipper.new(adapter) }
  let(:healthy) { flipper[:healthy] }
  let(:flaky) { flipper[:flaky] }

  def enable_boolean(store, feature)
    store.add(feature)
    store.enable(feature, feature.gate(:boolean), Flipper::Types::Boolean.new)
  end

  it 'exposes no receipt unless receipt: true is passed' do
    plain = described_class.new(Flipper::Adapters::Memory.new, Flipper::Adapters::Memory.new)
    expect(plain.read_receipt).to be_nil
  end

  it 'records an explicit healthy primary missing without filling the stale secondary' do
    enable_boolean(secondary_store, flaky)

    value = adapter.get(flaky)

    expect(value).to eq(adapter.default_config)
    entry = adapter.read_receipt.for(flaky.key)
    expect(entry.source).to be(:primary)
    expect(entry.status).to be(:missing)
    expect(entry.value[:boolean]).to be_nil
    expect(entry.error).to be_nil
  end

  it 'records a stored healthy primary value as ok' do
    enable_boolean(primary_store, flaky)

    adapter.get(flaky)
    entry = adapter.read_receipt.for(flaky.key)

    expect(entry.source).to be(:primary)
    expect(entry.status).to be(:ok)
    expect(entry.value[:boolean]).to eq('true')
  end

  it 'returns secondary values and audits the failure point when a batch read raises' do
    enable_boolean(secondary_store, flaky)
    error = RuntimeError.new('primary unavailable')
    primary.fail(:get_multi, error)

    result = adapter.get_multi([healthy, flaky])

    expect(result[healthy.key][:boolean]).to be_nil
    expect(result[flaky.key][:boolean]).to eq('true')
    [healthy, flaky].each do |feature|
      entry = adapter.read_receipt.for(feature.key)
      expect(entry.source).to be(:secondary)
      expect(entry.status).to be(:degraded)
      expect(entry.error).to be(error)
      expect(entry.operation).to be(:get_multi)
    end
    failure = adapter.read_receipt.failures.last
    expect(failure.operation).to be(:get_multi)
    expect(failure.keys).to eq([healthy.key, flaky.key])
    expect(failure.error).to be(error)
  end

  it 'marks every key degraded when get_all raises' do
    enable_boolean(secondary_store, flaky)
    error = RuntimeError.new('primary down')
    primary.fail(:get_all, error)

    adapter.get_all

    adapter.read_receipt.entries.each_value do |entry|
      expect(entry.source).to be(:secondary)
      expect(entry.status).to be(:degraded)
      expect(entry.error).to be(error)
    end
    expect(adapter.read_receipt.failures.last.operation).to be(:get_all)
  end

  it 'keeps the adapter contract return values while collecting a receipt' do
    enable_boolean(secondary_store, flaky)
    primary.fail(:get, RuntimeError.new('down'))
    plain = described_class.new(primary, secondary)
    primary.fail(:get, RuntimeError.new('down'))

    expect(adapter.get(flaky)).to eq(plain.get(flaky))
  end

  it 'walks the missing -> failed -> recovered lifecycle with interleaved memoization' do
    enable_boolean(secondary_store, flaky)
    receipt = adapter.read_receipt

    # Request 1: healthy primary explicitly missing; stale secondary ignored.
    flipper.memoize = true
    expect(flipper[:flaky].enabled?).to be(false)
    entry = receipt.for(flaky.key)
    expect(entry.source).to be(:primary)
    expect(entry.status).to be(:missing)
    expect(entry.value[:boolean]).to be_nil

    # Interleave the healthy key in the same request cache.
    expect(flipper[:healthy].enabled?).to be(false)
    entry = receipt.for(healthy.key)
    expect(entry.source).to be(:primary)
    expect(entry.status).to be(:missing)

    # Re-reading in the same request is a memo hit.
    flipper[:healthy].enabled?
    entry = receipt.for(healthy.key)
    expect(entry.source).to be(:memo)
    expect(entry.active_source).to be(:primary)
    flipper.memoize = false

    # Request 2: the primary fails during the second-round batch read.
    batch_error = RuntimeError.new('primary failed mid batch')
    primary.fail(:get_multi, batch_error)
    flipper.memoize = true

    batch = flipper.adapter.get_multi([healthy, flaky])
    expect(batch[healthy.key][:boolean]).to be_nil
    expect(batch[flaky.key][:boolean]).to eq('true')
    [healthy, flaky].each do |feature|
      entry = receipt.for(feature.key)
      expect(entry.source).to be(:secondary)
      expect(entry.status).to be(:degraded)
      expect(entry.error).to be(batch_error)
    end

    # enabled? inside the same request reuses the memoized degraded values.
    expect(flipper[:flaky].enabled?).to be(true)
    entry = receipt.for(flaky.key)
    expect(entry.source).to be(:memo)
    expect(entry.active_source).to be(:secondary)
    expect(entry.active_status).to be(:degraded)

    expect(flipper[:healthy].enabled?).to be(false)
    entry = receipt.for(healthy.key)
    expect(entry.source).to be(:memo)
    expect(entry.active_source).to be(:secondary)
    flipper.memoize = false

    # Request 3: primary recovers and returns an explicit disabled value.
    primary_store.add(flaky)
    primary_store.disable(flaky, flaky.gate(:boolean),
                          Flipper::Types::Boolean.new(false))
    flipper.memoize = true

    expect(flipper[:flaky].enabled?).to be(false)
    entry = receipt.for(flaky.key)
    expect(entry.source).to be(:primary)
    expect(entry.status).to be(:missing)
    expect(entry.value[:boolean]).to be_nil
    expect(entry.original_source).to be_nil

    expect(flipper[:healthy].enabled?).to be(false)
    entry = receipt.for(healthy.key)
    expect(entry.source).to be(:primary)
    expect(entry.status).to be(:missing)
    flipper.memoize = false

    # Failure points remain auditable after recovery.
    failure = receipt.failures.find { |candidate| candidate.operation == :get_multi }
    expect(failure.keys).to eq([healthy.key, flaky.key])
    expect(failure.error).to be(batch_error)
  end
end
