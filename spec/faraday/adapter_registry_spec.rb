# frozen_string_literal: true

RSpec.describe Faraday::AdapterRegistry do
  describe '#initialize' do
    subject(:registry) { described_class.new }

    it { expect { registry.get(:FinFangFoom) }.to raise_error(NameError) }
    it { expect { registry.get('FinFangFoom') }.to raise_error(NameError) }

    it 'looks up class by string name' do
      expect(registry.get('Faraday::Connection')).to eq(Faraday::Connection)
    end

    it 'looks up class by symbol name' do
      expect(registry.get(:Faraday)).to eq(Faraday)
    end

    it 'caches lookups with implicit name' do
      registry.set :symbol
      expect(registry.get('symbol')).to eq(:symbol)
    end

    it 'caches lookups with explicit name' do
      registry.set 'string', :name
      expect(registry.get(:name)).to eq('string')
    end

    describe '#fetch' do
      it 'returns the registered constant without touching Object constants' do
        registry.set(Faraday::Connection, :custom)
        expect(registry.fetch(:custom)).to eq(Faraday::Connection)
      end

      it 'returns nil for an unregistered name instead of resolving a constant' do
        expect(registry.fetch(:FinFangFoom)).to be_nil
      end

      it 'does not cache the name as a side effect' do
        registry.fetch(:FinFangFoom)
        expect(registry.fetch(:FinFangFoom)).to be_nil
      end
    end
  end
end
