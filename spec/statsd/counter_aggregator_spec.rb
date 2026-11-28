require 'spec_helper'

describe Datadog::Statsd::CounterAggregator do
  subject do
    described_class.new
  end

  describe '#add' do
    it 'merges counters with the same name, tags and sample rate' do
      key_a = subject.add('mycounter:1|c')
      key_b = subject.add('mycounter:1|c')

      expect(key_a).to eq(key_b)
      expect(subject.render).to eq(['mycounter:2|c'])
    end

    it 'sums arbitrary (possibly negative) deltas' do
      subject.add('mycounter:2|c')
      subject.add('mycounter:5|c')
      subject.add('mycounter:-3|c')

      expect(subject.render).to eq(['mycounter:4|c'])
    end

    it 'keeps different metric names in separate groups' do
      subject.add('mycounter:1|c')
      subject.add('myothercounter:1|c')
      subject.add('mycounter:4|c')

      expect(subject.render).to eq(['mycounter:5|c', 'myothercounter:1|c'])
    end

    it 'keeps different tag sets in separate groups' do
      subject.add('mycounter:1|c|#env:prod')
      subject.add('mycounter:1|c|#env:staging')
      subject.add('mycounter:1|c|#env:prod')

      expect(subject.render).to eq([
        'mycounter:2|c|#env:prod',
        'mycounter:1|c|#env:staging',
      ])
    end

    it 'only treats tag sets as equivalent when they serialize identically' do
      subject.add('mycounter:1|c|#x:1,y:2')
      subject.add('mycounter:1|c|#y:2,x:1')

      expect(subject.render).to eq([
        'mycounter:1|c|#x:1,y:2',
        'mycounter:1|c|#y:2,x:1',
      ])
    end

    it 'keeps different sample rates in separate groups' do
      subject.add('mycounter:1|c|@0.5')
      subject.add('mycounter:1|c|@0.25')
      subject.add('mycounter:1|c|@0.5')

      expect(subject.render).to eq([
        'mycounter:2|c|@0.5',
        'mycounter:1|c|@0.25',
      ])
    end

    it 'does not merge counters without a sample rate with sampled ones' do
      subject.add('mycounter:1|c')
      subject.add('mycounter:1|c|@0.5')

      expect(subject.render).to eq(['mycounter:1|c', 'mycounter:1|c|@0.5'])
    end

    it 'preserves the rate, tags and extra fields of the group' do
      subject.add('mycounter:1|c|@0.5|#env:prod|c:container')
      subject.add('mycounter:2|c|@0.5|#env:prod|c:container')

      expect(subject.render).to eq(['mycounter:3|c|@0.5|#env:prod|c:container'])
    end

    it 'keeps the first-seen order of the groups when rendering' do
      subject.add('mycounter:1|c|#env:prod')
      subject.add('agauge:7|g')
      subject.add('mycounter:1|c|#env:staging')
      subject.add('mycounter:3|c|#env:prod')

      expect(subject.render).to eq([
        'mycounter:4|c|#env:prod',
        'mycounter:1|c|#env:staging',
      ])
    end

    it 'ignores non-counter messages' do
      expect(subject.add('mygauge:7|g')).to be_nil
      expect(subject.add('myhistogram:7|h')).to be_nil
      expect(subject.add('mytiming:7|ms')).to be_nil
      expect(subject.add('myset:7|s')).to be_nil
      expect(subject.add('mydistribution:7|d')).to be_nil

      expect(subject).to be_empty
      expect(subject.render).to eq([])
    end

    it 'does not mistake a counter-named metric of another type for a counter' do
      expect(subject.add('mycounter:1|h')).to be_nil
      expect(subject).to be_empty
    end

    it 'ignores messages whose delta cannot be parsed' do
      expect(subject.add('mycounter:not_a_delta|c')).to be_nil
      expect(subject).to be_empty
    end

    it 'tracks the exact bytesize of the rendered messages' do
      subject.add('mycounter:1|c')
      size_after_first = subject.messages_bytesize
      subject.add('mycounter:1|c')

      expect(size_after_first).to eq('mycounter:1|c'.bytesize)
      # the merged group keeps the same rendered size ("2" is one byte too)
      expect(subject.messages_bytesize).to eq('mycounter:2|c'.bytesize)
      expect(subject.size).to eq(1)

      subject.add('myothercounter:1|c')
      expect(subject.size).to eq(2)
      expect(subject.messages_bytesize).to eq(
        'mycounter:2|c'.bytesize + 'myothercounter:1|c'.bytesize
      )
    end
  end

  describe '#empty?' do
    it 'is empty initially and once reset' do
      expect(subject).to be_empty

      subject.add('mycounter:1|c')
      expect(subject).not_to be_empty

      subject.reset
      expect(subject).to be_empty
      expect(subject.render).to eq([])
      expect(subject.messages_bytesize).to eq(0)
    end
  end

  describe '#rollback' do
    it 'removes a brand new group entirely' do
      key = subject.add('mycounter:1|c|#env:prod')
      subject.add('other:1|c')

      subject.rollback(key, 1, true)

      expect(subject.render).to eq(['other:1|c'])
      expect(subject.key?(key)).to be(false)
      expect(subject.size).to eq(1)
    end

    it 'restores the previous delta and bytesize of an existing group' do
      key = subject.add('mycounter:2|c')
      size_before = subject.messages_bytesize
      subject.add('mycounter:5|c')

      subject.rollback(key, 5, false)

      expect(subject.render).to eq(['mycounter:2|c'])
      expect(subject.messages_bytesize).to eq(size_before)
    end
  end
end
