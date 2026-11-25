require 'flipper/adapters/failover'
require 'flipper/adapters/instrumented'
require 'flipper/adapters/memoizable'

RSpec.describe Flipper::Adapters::Memoizable, 'read receipt' do
  let(:primary_store) { Flipper::Adapters::Memory.new }
  let(:secondary_store) { Flipper::Adapters::Memory.new }
  let(:failover) do
    Flipper::Adapters::Failover.new(primary_store, secondary_store, receipt: true)
  end
  subject(:memoizable) { described_class.new(failover) }
  let(:flipper) { Flipper.new(memoizable, memoize: false) }
  let(:feature) { flipper[:stats] }

  before { memoizable.memoize = true }

  it 'exposes the wrapped failover receipt' do
    expect(memoizable.read_receipt).to be(failover.read_receipt)
  end

  it 'returns nil when the wrapped adapter does not collect a receipt' do
    instance = described_class.new(Flipper::Adapters::Memory.new)
    expect(instance.read_receipt).to be_nil
  end

  it 'keeps primary provenance on the first read and restamps memo hits' do
    memoizable.get(feature)
    expect(memoizable.read_receipt.for(feature.key).source).to be(:primary)

    memoizable.get(feature)
    entry = memoizable.read_receipt.for(feature.key)
    expect(entry.source).to be(:memo)
    expect(entry.active_source).to be(:primary)
    expect(entry.active_status).to be(:missing)
  end

  it 'distinguishes a degraded memoized value from an active read' do
    allow(primary_store).to receive(:get).and_raise(RuntimeError, 'primary down')
    secondary_store.add(feature)
    secondary_store.enable(feature, feature.gate(:boolean), Flipper::Types::Boolean.new)

    memoizable.get(feature)
    expect(memoizable.read_receipt.for(feature.key).source).to be(:secondary)
    expect(memoizable.read_receipt.for(feature.key).status).to be(:degraded)

    memoizable.get(feature)
    entry = memoizable.read_receipt.for(feature.key)
    expect(entry.source).to be(:memo)
    expect(entry.active_source).to be(:secondary)
    expect(entry.active_status).to be(:degraded)
  end

  it 'restores primary provenance after an active reread' do
    memoizable.get(feature)
    memoizable.get(feature)
    expect(memoizable.read_receipt.for(feature.key).source).to be(:memo)

    memoizable.send(:expire_feature, feature)
    memoizable.get(feature)

    entry = memoizable.read_receipt.for(feature.key)
    expect(entry.source).to be(:primary)
    expect(entry.original_source).to be_nil
  end

  it 'only restamps keys reused from the cache in get_multi' do
    other = flipper[:search]
    memoizable.get(feature)

    memoizable.get_multi([feature, other])

    cached = memoizable.read_receipt.for(feature.key)
    expect(cached.source).to be(:memo)
    expect(cached.active_source).to be(:primary)

    fresh = memoizable.read_receipt.for(other.key)
    expect(fresh.source).to be(:primary)
    expect(fresh.status).to be(:missing)
    expect(fresh).not_to be_memo
  end

  it 'clears cached provenance when memoization is toggled' do
    memoizable.get(feature)
    memoizable.get(feature)
    expect(memoizable.read_receipt.for(feature.key).source).to be(:memo)

    memoizable.memoize = false
    memoizable.memoize = true
    memoizable.get(feature)

    expect(memoizable.read_receipt.for(feature.key).source).to be(:primary)
  end

  it 'stamps a preloaded-missing default as a memo value' do
    memoizable.get_all
    unknown = flipper[:never_added]

    memoizable.get(unknown)

    entry = memoizable.read_receipt.for(unknown.key)
    expect(entry.source).to be(:memo)
    expect(entry.status).to be(:missing)
  end

  it 'stays reachable through an instrumented wrapper' do
    instrumented = Flipper::Adapters::Instrumented.new(
      failover, instrumenter: Flipper::Instrumenters::Memory.new
    )
    wrapped = described_class.new(instrumented)
    wrapped.memoize = true

    expect(wrapped.read_receipt).to be(failover.read_receipt)

    wrapped.get(feature)
    wrapped.get(feature)
    entry = failover.read_receipt.for(feature.key)
    expect(entry.source).to be(:memo)
    expect(entry.active_source).to be(:primary)
  end
end
