require 'concurrent/map'

module Concurrent
  RSpec.describe Map::Snapshot do
    let(:map) { Concurrent::Map.new }

    def build(entries = {})
      entries.each { |k, v| map[k] = v }
      map.snapshot
    end

    describe 'returned by Concurrent::Map#snapshot' do
      it 'is a Map::Snapshot' do
        expect(map.snapshot).to be_an_instance_of(described_class)
      end

      it 'captures an empty map as an empty snapshot' do
        snapshot = map.snapshot
        expect(snapshot).to be_empty
        expect(snapshot.size).to eq 0
        expect(snapshot.to_a).to eq []
        expect(snapshot.to_h).to eq({})
        expect(snapshot.keys).to eq []
        expect(snapshot.values).to eq []
      end

      it 'fixes the entries present at capture time' do
        map[:a] = 1
        map[:b] = 2
        snapshot = build

        expect(snapshot.size).to eq 2
        expect(snapshot.to_h).to eq(a: 1, b: 2)
        expect(snapshot.keys.sort).to eq [:a, :b]
        expect(snapshot.values.sort).to eq [1, 2]
        expect(snapshot.to_a.sort).to eq [[:a, 1], [:b, 2]]
      end

      it 'keeps nil values distinct from missing keys' do
        map[:n] = nil
        snapshot = map.snapshot

        expect(snapshot[:n]).to be_nil
        expect(snapshot.key?(:n)).to be true
        expect(snapshot.fetch(:n)).to be_nil
        expect(snapshot.key?(:missing)).to be false
        expect(snapshot.fetch(:missing, :default)).to eq :default
      end
    end

    describe 'independence from later map changes' do
      let!(:snapshot) { build(a: 1, b: 2) }

      it 'is unaffected by later insertions' do
        map[:c] = 3
        expect(snapshot.to_h).to eq(a: 1, b: 2)
        expect(snapshot.key?(:c)).to be false
        expect(snapshot.size).to eq 2
      end

      it 'is unaffected by later deletions' do
        map.delete(:a)
        expect(snapshot.to_h).to eq(a: 1, b: 2)
        expect(snapshot.key?(:a)).to be true
        expect(snapshot[:a]).to eq 1
      end

      it 'is unaffected by later value replacement' do
        map[:a] = 999
        expect(snapshot[:a]).to eq 1
        expect(snapshot.to_h).to eq(a: 1, b: 2)
      end

      it 'is unaffected by clearing the map' do
        map.clear
        expect(snapshot.to_h).to eq(a: 1, b: 2)
        expect(snapshot).not_to be_empty
      end

      it 'keeps its own identity when the same key is deleted and reinserted' do
        map.delete(:a)
        map[:a] = 'reinserted'
        later = map.snapshot

        expect(snapshot[:a]).to eq 1
        expect(later[:a]).to eq 'reinserted'
        expect(snapshot).not_to eq later
      end
    end

    describe 'shallow reference semantics' do
      it 'shares the exact value objects stored in the map (no deep copy)' do
        value = Object.new
        map[:obj] = value
        snapshot = map.snapshot

        expect(snapshot[:obj]).to be value
        expect(snapshot.to_h[:obj]).to be value
      end

      it 'does not pretend to protect internal state of the values' do
        value = []
        map[:items] = value
        snapshot = map.snapshot

        value << :mutated_elsewhere
        expect(snapshot[:items]).to equal(value)
        expect(snapshot[:items]).to eq [:mutated_elsewhere]
      end
    end

    describe 'repeated iteration' do
      let(:snapshot) { build(a: 1, b: 2, c: 3) }

      it 'yields the same pairs on every traversal' do
        first  = []
        second = []
        snapshot.each_pair { |k, v| first << [k, v] }
        snapshot.each_pair { |k, v| second << [k, v] }

        expect(first).to eq second
        expect(first.sort).to eq [[:a, 1], [:b, 2], [:c, 3]]
      end

      it 'yields identical keys and values on repeated traversal' do
        expect(snapshot.keys).to eq snapshot.keys
        expect(snapshot.values).to eq snapshot.values
      end

      it 'returns an enumerator when no block is given' do
        expect(snapshot.each_pair).to be_an(Enumerator)
        expect(snapshot.each_key).to be_an(Enumerator)
        expect(snapshot.each_value).to be_an(Enumerator)
        expect(snapshot.each_pair.to_a.sort).to eq [[:a, 1], [:b, 2], [:c, 3]]
      end

      it 'supports break out of iteration' do
        result = snapshot.each_pair { break :breaked }
        expect(result).to eq :breaked
      end

      it 'is Enumerable' do
        expect(snapshot).to be_an(Enumerable)
        expect(snapshot.map { |k, v| [k, v] }.sort).to eq [[:a, 1], [:b, 2], [:c, 3]]
        expect(snapshot.select { |_k, v| v > 1 }.sort).to eq [[:b, 2], [:c, 3]]
        expect(snapshot.to_enum).to be_an(Enumerator)
      end
    end

    describe 'iteration does not hold the map lock' do
      it 'allows the iteration block to access the original map' do
        map[:a] = 1
        snapshot = map.snapshot
        accessed = false

        snapshot.each_pair do |key, _value|
          # Reading and writing the originating map from inside a view
          # iteration must not deadlock.
          map[:b] = 2
          expect(map[key]).to eq 1
          accessed = true
        end

        expect(accessed).to be true
        expect(map[:b]).to eq 2
      end
    end

    describe '#fetch' do
      let(:snapshot) { build(a: 1) }

      it 'returns the stored value' do
        expect(snapshot.fetch(:a)).to eq 1
      end

      it 'returns the default argument for a missing key' do
        expect(snapshot.fetch(:missing, 42)).to eq 42
      end

      it 'yields the key to the block for a missing key' do
        expect(snapshot.fetch(:missing) { |k| "got #{k}" }).to eq 'got missing'
      end

      it 'raises KeyError without a default or block' do
        expect { snapshot.fetch(:missing) }.to raise_error(KeyError, 'key not found')
      end
    end

    describe '#key? and #value?' do
      let(:snapshot) { build(a: 1, b: 2) }

      it 'reports key presence' do
        expect(snapshot.key?(:a)).to be true
        expect(snapshot.has_key?(:a)).to be true
        expect(snapshot.key?(:z)).to be false
      end

      it 'finds values by equality' do
        expect(snapshot.value?(2)).to be true
        expect(snapshot.has_value?(2)).to be true
        expect(snapshot.value?(99)).to be false
        expect(snapshot.key(2)).to eq :b
        expect(snapshot.key(99)).to be_nil
      end
    end

    describe '#to_h and #to_a return owned copies' do
      let(:snapshot) { build(a: 1) }

      it 'returns hashes that the caller can mutate without touching the view' do
        h = snapshot.to_h
        h[:injected] = true

        expect(snapshot.key?(:injected)).to be false
        expect(snapshot.to_h).to eq(a: 1)
      end

      it 'returns arrays that the caller can mutate without touching the view' do
        pairs = snapshot.to_a
        pairs << [:injected, true]

        expect(snapshot.to_a).to eq [[:a, 1]]
      end
    end

    describe '#==' do
      it 'compares two snapshots on their captured pairs' do
        map[:a] = 1
        first  = map.snapshot
        second = map.snapshot

        expect(first).to eq second
      end

      it 'compares against a plain Hash' do
        snapshot = build(a: 1, b: 2)
        expect(snapshot).to eq(a: 1, b: 2)
        expect(snapshot).not_to eq(a: 1)
      end

      it 'is not equal to unrelated objects' do
        expect(build(a: 1)).not_to eq 42
        expect(build(a: 1)).not_to eq [[:a, 1]]
      end
    end

    describe 'immutability of the view' do
      let(:snapshot) { build(a: 1) }

      it 'does not expose mutating map methods' do
        expect(snapshot).not_to respond_to(:[]=)
        expect(snapshot).not_to respond_to(:delete)
        expect(snapshot).not_to respond_to(:clear)
      end

      it 'cannot be frozen, like Concurrent::Map' do
        expect { snapshot.freeze }.to raise_error(NoMethodError)
      end
    end

    describe 'under concurrent mutation of the source map' do
      it 'stays internally consistent while writers run' do
        10.times { |i| map[i] = i }
        snapshot = nil

        writers = 4.times.map do
          in_thread do
            500.times do |i|
              map[100 + i] = i
              map.delete(i)
              map[i] = i
            end
          end
        end

        snapshot = map.snapshot
        writers.each(&:join)

        # Every traversal of the one snapshot must yield the exact same pairs.
        pairs = snapshot.to_a
        3.times do
          expect(snapshot.to_a).to eq pairs
          collected = []
          snapshot.each_pair { |k, v| collected << [k, v] }
          expect(collected).to eq pairs
        end
        expect(snapshot.size).to eq pairs.size
      end
    end
  end
end
