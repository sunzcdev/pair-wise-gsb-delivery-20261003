# frozen_string_literal: true

RSpec.describe Shoryuken::Worker::InlineExecutor::FifoDeduplicator do
  let(:time) { [1_000.0] }
  let(:clock) { -> { time.first } }

  subject(:deduplicator) { described_class.new(window: 300, clock: clock) }

  def advance(seconds)
    time[0] += seconds
  end

  describe '#initialize' do
    it 'defaults to the SQS five-minute deduplication window' do
      expect(described_class.new.window).to eq(5 * 60)
    end

    it 'rejects a non-positive window' do
      expect { described_class.new(window: 0) }.to raise_error(ArgumentError, 'window must be positive')
      expect { described_class.new(window: -1) }.to raise_error(ArgumentError, 'window must be positive')
    end
  end

  describe '#duplicate?' do
    it 'runs the first submission of a scope and suppresses the repeat within the window' do
      expect(deduplicator.duplicate?('queue.fifo', 'group', 'dedup')).to be(false)
      expect(deduplicator.duplicate?('queue.fifo', 'group', 'dedup')).to be(true)
      expect(deduplicator.size).to eq(1)
    end

    it 'scopes by the queue actually submitted to' do
      expect(deduplicator.duplicate?('queue-a.fifo', 'group', 'dedup')).to be(false)
      expect(deduplicator.duplicate?('queue-b.fifo', 'group', 'dedup')).to be(false)
    end

    it 'scopes by the message group id' do
      expect(deduplicator.duplicate?('queue.fifo', 'group-a', 'dedup')).to be(false)
      expect(deduplicator.duplicate?('queue.fifo', 'group-b', 'dedup')).to be(false)
    end

    it 'scopes by the message deduplication id' do
      expect(deduplicator.duplicate?('queue.fifo', 'group', 'dedup-a')).to be(false)
      expect(deduplicator.duplicate?('queue.fifo', 'group', 'dedup-b')).to be(false)
    end

    it 'runs a scope again once the window elapses' do
      expect(deduplicator.duplicate?('queue.fifo', 'group', 'dedup')).to be(false)
      expect(deduplicator.duplicate?('queue.fifo', 'group', 'dedup')).to be(true)

      advance(300)

      expect(deduplicator.duplicate?('queue.fifo', 'group', 'dedup')).to be(false)
    end

    it 'keeps a scope suppressed right up to the window boundary' do
      deduplicator.duplicate?('queue.fifo', 'group', 'dedup')
      advance(299.9)

      expect(deduplicator.duplicate?('queue.fifo', 'group', 'dedup')).to be(true)
    end

    it 'prunes expired scopes instead of growing forever' do
      deduplicator.duplicate?('queue.fifo', 'group', 'old')
      advance(100)
      deduplicator.duplicate?('queue.fifo', 'group', 'fresh')
      advance(250) # 'old' is now 350s old, 'fresh' is 250s old

      expect(deduplicator.size).to eq(1)
      expect(deduplicator.keys).to eq([['queue.fifo', 'group', 'fresh']])
    end

    it 'is safe under concurrent submissions of the same scope' do
      # Without synchronization a read-then-write race lets every thread record
      # the scope and all submissions run, unlike SQS.
      threads = Array.new(8) do
        Thread.new { deduplicator.duplicate?('queue.fifo', 'group', 'dedup') }
      end

      results = threads.map(&:value)
      expect(results.count(false)).to eq(1)
      expect(results.count(true)).to eq(7)
    end
  end

  describe '#keys' do
    it 'exposes the observed scopes as queue/group/deduplication triples' do
      deduplicator.duplicate?('queue-a.fifo', 'group-a', 'dedup-a')
      deduplicator.duplicate?('queue-b.fifo', 'group-b', 'dedup-b')

      expect(deduplicator.keys).to contain_exactly(
        ['queue-a.fifo', 'group-a', 'dedup-a'],
        ['queue-b.fifo', 'group-b', 'dedup-b']
      )
    end

    it 'returns copies that cannot mutate the stored scopes' do
      deduplicator.duplicate?('queue.fifo', 'group', 'dedup')

      deduplicator.keys.first << 'tampered'

      expect(deduplicator.keys).to eq([['queue.fifo', 'group', 'dedup']])
    end
  end

  describe '#reset' do
    it 'forgets every observed scope so it executes again' do
      deduplicator.duplicate?('queue.fifo', 'group', 'dedup')
      expect(deduplicator.size).to eq(1)

      deduplicator.reset

      expect(deduplicator.size).to eq(0)
      expect(deduplicator.duplicate?('queue.fifo', 'group', 'dedup')).to be(false)
    end
  end
end
