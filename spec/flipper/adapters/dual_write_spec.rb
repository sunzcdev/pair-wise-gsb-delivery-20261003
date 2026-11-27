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

  describe '#transaction' do
    # The release flips the two flags that select which backend serves
    # requests. Exactly one backend may serve at any time, so the only legal
    # combinations are [old on, new off] and [old off, new on].
    let(:old_backend) { sync[:requests_use_old_backend] }
    let(:new_backend) { sync[:requests_use_new_backend] }
    let(:boolean_gate_old) { old_backend.gate(:boolean) }
    let(:boolean_gate_new) { new_backend.gate(:boolean) }

    def boolean_pair(adapter)
      all = adapter.get_all
      [
        all[old_backend.key]&.fetch(:boolean, nil) == 'true',
        all[new_backend.key]&.fetch(:boolean, nil) == 'true',
      ]
    end

    # Validates the combined final state of the two flags.
    let(:validate) do
      ->(batch) do
        old_on, new_on = boolean_pair(batch)
        unless old_on != new_on
          raise Flipper::InvalidBatch,
                "exactly one backend must serve requests, got old=#{old_on} new=#{new_on}"
        end
      end
    end

    before do
      # Committed starting state: only the old backend serves requests.
      subject.enable(old_backend, boolean_gate_old, Flipper::Types::Boolean.new(true))
      subject.disable(new_backend, boolean_gate_new, Flipper::Types::Boolean.new(false))
    end

    it 'flips the two flags together on both backends as one grouped change' do
      subject.transaction(validate: validate) do |batch|
        batch.disable(old_backend, boolean_gate_old, Flipper::Types::Boolean.new(false))
        batch.enable(new_backend, boolean_gate_new, Flipper::Types::Boolean.new(true))
      end

      expect(boolean_pair(local_adapter.adapter)).to eq([false, true])
      expect(boolean_pair(remote_adapter.adapter)).to eq([false, true])
    end

    it 'only ever exposes the two legal combined states during a normal cutover' do
      observed = []
      subject.transaction(validate: validate) do |batch|
        batch.disable(old_backend, boolean_gate_old, Flipper::Types::Boolean.new(false))

        # Read barrier: the original entry still serves the committed state
        # between the two buffered modifications.
        observed << boolean_pair(subject)

        batch.enable(new_backend, boolean_gate_new, Flipper::Types::Boolean.new(true))
      end

      expect(observed).to eq([[true, false]])
      expect(boolean_pair(subject)).to eq([false, true])
    end

    it 'lets the original entry keep reading committed state inside the batch' do
      subject.transaction do |batch|
        batch.enable(new_backend, boolean_gate_new, Flipper::Types::Boolean.new(true))

        expect(subject.get(new_backend)[:boolean]).to be_nil
        expect(batch.get(new_backend)[:boolean]).to eq('true')
      end
    end

    it 'rejects an illegal combination and leaves the existing gating in place' do
      before_local = boolean_pair(local_adapter.adapter)
      before_remote = boolean_pair(remote_adapter.adapter)

      expect do
        subject.transaction(validate: validate) do |batch|
          # new backend is already on in committed state; turning the old one
          # on too would route requests to both backends at once.
          batch.enable(old_backend, boolean_gate_old, Flipper::Types::Boolean.new(true))
        end
      end.to raise_error(Flipper::InvalidBatch, /exactly one backend/)

      expect(boolean_pair(local_adapter.adapter)).to eq(before_local)
      expect(boolean_pair(remote_adapter.adapter)).to eq(before_remote)
    end

    it 'rejects a zero-backend combination without changing either backend' do
      expect do
        subject.transaction(validate: validate) do |batch|
          batch.disable(new_backend, boolean_gate_new, Flipper::Types::Boolean.new(false))
        end
      end.to raise_error(Flipper::InvalidBatch)

      expect(boolean_pair(local_adapter.adapter)).to eq([true, false])
      expect(boolean_pair(remote_adapter.adapter)).to eq([true, false])
    end

    it 'writes nothing to either backend when the validator raises' do
      local_adapter.reset
      remote_adapter.reset

      expect do
        subject.transaction(validate: validate) do |batch|
          batch.enable(old_backend, boolean_gate_old, Flipper::Types::Boolean.new(true))
        end
      end.to raise_error(Flipper::InvalidBatch)

      expect(local_adapter.count).to be(0)
      expect(remote_adapter.count).to be(0)
    end

    it 'returns the block return value on a successful commit' do
      result = subject.transaction(validate: validate) do |batch|
        batch.disable(old_backend, boolean_gate_old, Flipper::Types::Boolean.new(false))
        batch.enable(new_backend, boolean_gate_new, Flipper::Types::Boolean.new(true))
        :cutover_complete
      end

      expect(result).to eq(:cutover_complete)
    end

    it 'commits without a validator when none is given' do
      subject.transaction do |batch|
        batch.enable(new_backend, boolean_gate_new, Flipper::Types::Boolean.new(true))
      end

      expect(boolean_pair(local_adapter.adapter)).to eq([true, true])
      expect(boolean_pair(remote_adapter.adapter)).to eq([true, true])
    end
  end
end
