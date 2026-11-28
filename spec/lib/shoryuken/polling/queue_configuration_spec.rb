# frozen_string_literal: true

RSpec.describe Shoryuken::Polling::QueueConfiguration do
  describe '#initialize' do
    it 'creates configuration with name and options' do
      config = described_class.new('test_queue', { priority: :high })

      expect(config.name).to eq('test_queue')
      expect(config.options).to eq({ priority: :high })
    end

    it 'creates configuration with empty options' do
      config = described_class.new('simple_queue', {})

      expect(config.name).to eq('simple_queue')
      expect(config.options).to eq({})
    end

    it 'accepts nil options' do
      config = described_class.new('queue', nil)

      expect(config.name).to eq('queue')
      expect(config.options).to be_nil
    end
  end

  describe '#hash' do
    it 'returns hash based on queue name' do
      config1 = described_class.new('queue', {})
      config2 = described_class.new('queue', { weight: 5 })

      expect(config1.hash).to eq(config2.hash)
      expect(config1.hash).to eq('queue'.hash)
    end

    it 'returns different hash for different queue names' do
      config1 = described_class.new('queue1', {})
      config2 = described_class.new('queue2', {})

      expect(config1.hash).not_to eq(config2.hash)
    end
  end

  describe '#==' do
    context 'when comparing with another QueueConfiguration' do
      it 'returns true for same name and options' do
        config1 = described_class.new('queue', { weight: 5 })
        config2 = described_class.new('queue', { weight: 5 })

        expect(config1).to eq(config2)
      end

      it 'returns false for same name but different options' do
        config1 = described_class.new('queue', { weight: 5 })
        config2 = described_class.new('queue', { weight: 10 })

        expect(config1).not_to eq(config2)
      end

      it 'returns false for different names' do
        config1 = described_class.new('queue1', {})
        config2 = described_class.new('queue2', {})

        expect(config1).not_to eq(config2)
      end
    end

    context 'when comparing with a string' do
      it 'returns true when options are empty and names match' do
        config = described_class.new('test_queue', {})

        expect(config).to eq('test_queue')
      end

      it 'returns false when options are not empty' do
        config = described_class.new('test_queue', { weight: 5 })

        expect(config).not_to eq('test_queue')
      end

      it 'returns false when names do not match' do
        config = described_class.new('queue1', {})

        expect(config).not_to eq('queue2')
      end
    end

    context 'when comparing with other objects' do
      it 'returns false for non-string, non-QueueConfiguration objects' do
        config = described_class.new('queue', {})

        expect(config).not_to eq(123)
        expect(config).not_to eq([])
        expect(config).not_to eq({ name: 'queue' })
      end
    end
  end

  describe '#eql?' do
    it 'behaves the same as ==' do
      config1 = described_class.new('queue', {})
      config2 = described_class.new('queue', {})

      expect(config1.eql?(config2)).to eq(config1 == config2)
      expect(config1.eql?('queue')).to eq(config1 == 'queue')
    end
  end

  describe '#to_s' do
    context 'when options are empty' do
      it 'returns just the queue name' do
        config = described_class.new('simple_queue', {})

        expect(config.to_s).to eq('simple_queue')
      end
    end

    context 'when options are present' do
      it 'returns detailed representation with options' do
        config = described_class.new('complex_queue', { priority: :high, weight: 5 })

        expect(config.to_s).to include('#<QueueConfiguration complex_queue options={')
      end

      it 'handles single option' do
        config = described_class.new('weighted_queue', { weight: 10 })

        expect(config.to_s).to include('#<QueueConfiguration weighted_queue options={')
      end
    end

    context 'when options are nil' do
      it 'returns detailed representation' do
        config = described_class.new('nil_options_queue', nil)

        expect(config.to_s).to eq('#<QueueConfiguration nil_options_queue options=nil>')
      end
    end
  end

  describe 'struct behavior' do
    it 'provides attribute accessors' do
      config = described_class.new('queue', { weight: 5 })

      expect(config.name).to eq('queue')
      expect(config.options).to eq({ weight: 5 })
    end

    it 'allows attribute modification' do
      config = described_class.new('queue', {})

      config.name = 'new_queue'
      config.options = { priority: :low }

      expect(config.name).to eq('new_queue')
      expect(config.options).to eq({ priority: :low })
    end

    it 'supports array-like access' do
      config = described_class.new('queue', { weight: 5 })

      expect(config[0]).to eq('queue')
      expect(config[1]).to eq({ weight: 5 })
    end

    it 'carries the poll id of the selection it describes' do
      config = described_class.new('queue', {}, 4)

      expect(config.poll_id).to eq(4)
    end

    it 'leaves the poll id unset when none is given' do
      expect(described_class.new('queue', {}).poll_id).to be_nil
    end
  end

  describe 'poll id and equality' do
    it 'ignores the poll id when comparing two configurations' do
      plain = described_class.new('queue', {})
      selected = described_class.new('queue', {}, 7)

      expect(selected).to eq(plain)
      expect(plain).to eq(selected)
      expect(selected.eql?(plain)).to be true
    end

    it 'still compares equal to the plain queue name' do
      expect(described_class.new('queue', {}, 7)).to eq('queue')
    end

    it 'hashes only on the queue name' do
      plain = described_class.new('queue', {})
      selected = described_class.new('queue', {}, 7)

      expect(selected.hash).to eq(plain.hash)
    end
  end

  describe 'usage as hash key' do
    it 'can be used as hash keys' do
      config1 = described_class.new('queue', {})
      config2 = described_class.new('queue', {})  # Same config

      hash = {}
      hash[config1] = 'value1'
      hash[config2] = 'value2'

      # Same queue name and options should use same hash key
      expect(hash[config1]).to eq('value2')
      expect(hash[config2]).to eq('value2')
      expect(hash.size).to eq(1)
    end

    it 'different queue names create different keys' do
      config1 = described_class.new('queue1', {})
      config2 = described_class.new('queue2', {})

      hash = {}
      hash[config1] = 'value1'
      hash[config2] = 'value2'

      expect(hash[config1]).to eq('value1')
      expect(hash[config2]).to eq('value2')
      expect(hash.size).to eq(2)
    end
  end
end