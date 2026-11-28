require 'flipper/adapters/dual_write'
require 'flipper/adapters/operation_logger'
require 'active_support/notifications'

RSpec.describe Flipper::Adapters::DualWrite do
  let(:local_adapter) do
    Flipper::Adapters::OperationLogger.new Flipper::Adapters::Memory.new
  end
  let(:remote_adapter) do
    Flipper::Adapters::OperationLogger.new Flipper::Adapters::Memory.new
  end
  let(:sync) { Flipper.new(subject) }

  subject do
    described_class.new(local_adapter, remote_adapter)
  end

  it_should_behave_like 'a flipper adapter'
  it_should_behave_like 'an adapter that stores its sync version'

  it 'keeps the sync version in the local adapter only' do
    subject.sync_version = Flipper::SyncVersion.new("scope", 10)
    expect(local_adapter.sync_version).to eq(subject.sync_version)
    expect(remote_adapter.sync_version).to be_nil
  end

  it 'only uses local for #features' do
    subject.features
  end

  it 'only uses local for #get' do
    subject.get sync[:search]
  end

  it 'only uses local for #get_multi' do
    subject.get_multi [sync[:search]]
  end

  it 'only uses local for #get_all' do
    subject.get_all
  end

  it 'updates remote and local for #add' do
    subject.add sync[:search]
    expect(remote_adapter.count(:add)).to be(1)
    expect(local_adapter.count(:add)).to be(1)
  end

  it 'updates remote and local for #remove' do
    subject.remove sync[:search]
    expect(remote_adapter.count(:remove)).to be(1)
    expect(local_adapter.count(:remove)).to be(1)
  end

  it 'updates remote and local for #clear' do
    subject.clear sync[:search]
    expect(remote_adapter.count(:clear)).to be(1)
    expect(local_adapter.count(:clear)).to be(1)
  end

  it 'updates remote and local for #enable' do
    feature = sync[:search]
    subject.enable feature, feature.gate(:boolean), Flipper::Types::Boolean.new(true)
    expect(remote_adapter.count(:enable)).to be(1)
    expect(local_adapter.count(:enable)).to be(1)
  end

  it 'updates remote and local for #disable' do
    feature = sync[:search]
    subject.disable feature, feature.gate(:boolean), Flipper::Types::Boolean.new(false)
    expect(remote_adapter.count(:disable)).to be(1)
    expect(local_adapter.count(:disable)).to be(1)
  end

  describe 'write receipts' do
    let(:local) { Flipper::Adapters::Memory.new }
    let(:remote) { Flipper::Adapters::Memory.new }
    let(:adapter) { described_class.new(local, remote) }
    let(:flipper) { Flipper.new(adapter, memoize: false) }

    def make_replica_fail(methods = [:enable, :disable])
      allow(remote).to receive(:enable).and_raise(IOError, 'replica unreachable') if methods.include?(:enable)
      allow(remote).to receive(:disable).and_raise(IOError, 'replica unreachable') if methods.include?(:disable)
    end

    it 'returns a receipt that reports the replica caught up' do
      receipt = flipper.enable(:flag)

      expect(receipt).to be_instance_of(described_class::Receipt)
      expect(receipt.caught_up?).to be(true)
      expect(receipt.pending?).to be(false)
      expect(receipt.error).to be_nil
      expect(receipt.feature_name).to eq('flag')
      expect(receipt.gate_name).to eq(:boolean)
      expect(adapter.pending_count).to be(0)
    end

    it 'keeps the receipt compatible with the boolean adapter contract' do
      expect(flipper.enable(:flag)).to eq(true)
      expect(flipper.enable(:flag)).to be(true)
      expect(flipper.disable(:flag)).to eq(true)
      expect(flipper.enable_actor(:flag, Flipper::Actor.new('u1'))).to eq(true)
    end

    it 'does not consider two different receipts equal just because each matches true' do
      first = flipper.enable(:one)
      second = flipper.enable(:two)

      expect(first).not_to eq(second)
      expect(first).not_to equal(second)
    end

    it 'commits to the primary and reports replica failures in the receipt' do
      make_replica_fail

      receipt = nil
      expect { receipt = flipper.enable(:flag) }.not_to raise_error

      expect(receipt.pending?).to be(true)
      expect(receipt.caught_up?).to be(false)
      expect(receipt.error).to be_a(IOError)
      expect(receipt.local_applied?).to be(true)
      expect(receipt.remote_applied?).to be(false)
      expect(flipper.enabled?(:flag)).to be(true)
      expect(remote.get(Flipper::Feature.new(:flag, remote))[:boolean]).to be(nil)
      expect(adapter.pending_count).to be(1)
    end

    it 'follows the primary current result when an old receipt is processed' do
      # The replica misses the enable, then the primary flips the same gate to
      # the opposite value while the replica is still behind.
      allow(remote).to receive(:enable).and_raise(IOError, 'replica unreachable')
      old_receipt = flipper.enable(:flag)
      allow(remote).to receive(:enable).and_call_original
      allow(remote).to receive(:disable).and_raise(IOError, 'replica unreachable')
      flipper.disable(:flag)

      expect(flipper.enabled?(:flag)).to be(false)

      result = old_receipt.retry

      expect(result.caught_up?).to be(true)
      expect(result.pending?).to be(false)
      # The replica ends on the primary's actual result, not the old enable.
      expect(remote.get(Flipper::Feature.new(:flag, remote))[:boolean]).to be(nil)
      expect(flipper.enabled?(:flag)).to be(false)
      expect(adapter.pending_count).to be(0)
    end

    it 'never replays a superseded value when the replica missed both writes' do
      # Replica is fully down, so both enable and disable are pending.
      make_replica_fail
      old_receipt = flipper.enable(:flag)
      flipper.disable(:flag)
      expect(remote.get(Flipper::Feature.new(:flag, remote))[:boolean]).to be(nil)
      allow(remote).to receive(:enable).and_call_original
      allow(remote).to receive(:disable).and_call_original

      old_receipt.retry

      expect(remote.get(Flipper::Feature.new(:flag, remote))[:boolean]).to be(nil)
      expect(adapter.pending_count).to be(0)
    end

    it 'reconciles only the real differences for set gates' do
      actor = Flipper::Actor.new('u1')
      flipper.enable_actor(:search, actor)
      flipper.disable_actor(:search, actor)

      allow(remote).to receive(:enable).and_raise(IOError, 'replica unreachable')
      receipt = flipper.enable_actor(:search, actor)
      expect(receipt.pending?).to be(true)
      expect(receipt.gate_name).to eq(:actors)

      # Primary removes the actor again before the receipt is processed.
      flipper.disable_actor(:search, actor)
      allow(remote).to receive(:enable).and_call_original

      receipt.retry

      expect(remote.get(Flipper::Feature.new(:search, remote))[:actors]).to eq(Set.new)
      expect(flipper[:search].actors_value).to eq(Set.new)
    end

    it 'is idempotent when a caught up receipt is processed again' do
      remote_adapter.reset
      receipt = flipper.enable(:flag)
      before = remote_adapter.count(:enable)

      receipt.retry
      receipt.retry

      expect(remote_adapter.count(:enable)).to be(before)
    end

    it 'reconciles every pending receipt with #reconcile_pending' do
      make_replica_fail
      flipper.enable(:one)
      flipper.enable(:two)
      expect(adapter.pending_count).to be(2)
      allow(remote).to receive(:enable).and_call_original

      results = adapter.reconcile_pending

      expect(results.size).to be(2)
      expect(results).to all(be_caught_up)
      expect(adapter.pending_count).to be(0)
      expect(remote.get(Flipper::Feature.new(:one, remote))[:boolean]).to eq('true')
      expect(remote.get(Flipper::Feature.new(:two, remote))[:boolean]).to eq('true')
    end

    it 'drops a pending receipt once a newer write for the same gate succeeds' do
      make_replica_fail
      flipper.enable(:flag)
      expect(adapter.pending_count).to be(1)

      allow(remote).to receive(:enable).and_call_original
      flipper.enable(:flag)

      expect(adapter.pending_count).to be(0)
    end

    it 'does not let replaying an old receipt drop a newer pending receipt' do
      # First write succeeds everywhere.
      old_receipt = flipper.enable(:flag)

      # Second write leaves a pending receipt because the replica fails.
      allow(remote).to receive(:enable).and_raise(IOError, 'replica unreachable')
      flipper.enable(:flag)
      expect(adapter.pending_count).to be(1)

      # Replaying the already caught up receipt must not clear that pending one.
      old_receipt.retry
      expect(adapter.pending_count).to be(1)
    end

    it 'exposes the last receipt' do
      receipt = flipper.enable(:flag)
      expect(adapter.last_receipt).to be(receipt)
    end

    it 'raises primary failures instead of returning a receipt' do
      allow(local).to receive(:enable).and_raise(ArgumentError, 'primary down')
      expect { flipper.enable(:flag) }.to raise_error(ArgumentError, 'primary down')
    end
  end

  describe '#import' do
    let(:local) { Flipper::Adapters::Memory.new }
    let(:remote) { Flipper::Adapters::Memory.new }
    let(:adapter) { described_class.new(local, remote) }
    let(:source) { Flipper::Adapters::Memory.new }

    it 'reports replica failures because the caller asked to push flags' do
      Flipper.new(source).enable(:search)
      allow(remote).to receive(:enable).and_raise(IOError, 'replica unreachable')

      expect { adapter.import(source) }.to raise_error(IOError, 'replica unreachable')
    end
  end

  describe '#adapter_stack' do
    it 'returns the tree representation' do
      expect(subject.adapter_stack).to eq("dual_write(local: operation_logger -> memory, remote: operation_logger -> memory)")
    end

    it 'shows nested adapters in the tree' do
      memory = Flipper::Adapters::Memory.new
      strict = Flipper::Adapters::Strict.new(Flipper::Adapters::Memory.new)
      adapter = described_class.new(memory, strict)
      expect(adapter.adapter_stack).to eq("dual_write(local: memory, remote: strict -> memory)")
    end
  end
end
