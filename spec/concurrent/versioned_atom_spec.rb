require 'concurrent/atomic/count_down_latch'
require 'concurrent/atomic/atomic_fixnum'
require 'concurrent/versioned_atom'

module Concurrent

  RSpec.describe VersionedAtom do

    describe '#snapshot' do

      it 'returns the initial value and version together' do
        atom = described_class.new(:a)

        expect(atom.snapshot).to eq [:a, 0]
      end

      it 'returns a frozen snapshot' do
        atom = described_class.new(:a)

        expect(atom.snapshot).to be_frozen
      end

      it 'supports value and version accessors' do
        atom = described_class.new(:a)

        expect(atom.value).to eq :a
        expect(atom.version).to eq 0
      end
    end

    describe '#compare_and_set' do

      it 'commits when the snapshot is current' do
        atom = described_class.new(:a)
        snapshot = atom.snapshot

        expect(atom.compare_and_set(snapshot, :b)).to be true
        expect(atom.snapshot).to eq [:b, 1]
      end

      it 'increments the version even when the committed value is equal' do
        atom = described_class.new(:a)

        expect(atom.compare_and_set(atom.snapshot, :a)).to be true
        expect(atom.version).to eq 1
      end

      it 'rejects a reconstructed snapshot with the same value and version' do
        atom = described_class.new(:a)

        expect(atom.compare_and_set([:a, 0], :b)).to be false
        expect(atom.snapshot).to eq [:a, 0]
      end

      it 'does not increment the version when validation fails' do
        atom = described_class.new(:a, validator: -> value { value != :b })

        expect(atom.compare_and_set(atom.snapshot, :b)).to be false
        expect(atom.snapshot).to eq [:a, 0]
      end

      it 'does not increment the version when a race is lost' do
        atom = described_class.new(:a)
        stale_snapshot = atom.snapshot
        atom.compare_and_set(atom.snapshot, :b)

        expect(atom.compare_and_set(stale_snapshot, :c)).to be false
        expect(atom.snapshot).to eq [:b, 1]
      end

      it 'matches a current value and version supplied separately' do
        atom = described_class.new(:a)

        expect(atom.compare_and_set(:a, 0, :b)).to be true
        expect(atom.snapshot).to eq [:b, 1]
      end

      it 'matches a current numeric value and version supplied separately' do
        atom = described_class.new(1)

        expect(atom.compare_and_set(1, 0, 2)).to be true
        expect(atom.snapshot).to eq [2, 1]
      end

      it 'rejects a stale value and version supplied separately' do
        atom = described_class.new(:a)
        atom.compare_and_set(atom.snapshot, :b)
        atom.compare_and_set(atom.snapshot, :a)

        expect(atom.compare_and_set(:a, 0, :c)).to be false
        expect(atom.snapshot).to eq [:a, 2]
      end
    end

    describe '#swap' do

      it 'retries with the latest snapshot and commits its version' do
        first_block_started = CountDownLatch.new
        release_first_block = CountDownLatch.new
        block_invocations = Concurrent::AtomicFixnum.new(0)
        atom = described_class.new(0)

        swapping = in_thread do
          atom.swap do |value|
            block_invocations.increment
            first_block_started.count_down
            release_first_block.wait(1)
            value + 1
          end
        end

        first_block_started.wait(1)
        expect(atom.swap { 10 }).to eq 10
        release_first_block.count_down
        join_with(swapping)

        expect(block_invocations.value).to eq 2
        expect(atom.snapshot).to eq [11, 2]
      end

      it 'returns the old value and does not increment when validation fails' do
        atom = described_class.new(:a, validator: -> value { value != :b })

        expect(atom.swap { :b }).to eq :a
        expect(atom.snapshot).to eq [:a, 0]
      end
    end

    describe '#reset' do

      it 'sets the value and increments the version' do
        atom = described_class.new(:a)

        expect(atom.reset(:b)).to eq :b
        expect(atom.snapshot).to eq [:b, 1]
      end

      it 'does not increment when validation fails' do
        atom = described_class.new(:a, validator: -> value { value != :b })

        expect(atom.reset(:b)).to eq :a
        expect(atom.snapshot).to eq [:a, 0]
      end
    end

    describe 'versioned observation' do

      it 'notifies with the actual versions before and after each commit' do
        events = []
        atom = described_class.new(0)
        atom.add_observer do |_time, old_value, new_value, old_version, new_version|
          events << [old_value, new_value, old_version, new_version]
        end

        first_block_started = CountDownLatch.new
        release_first_block = CountDownLatch.new
        swapping = in_thread do
          atom.swap do |value|
            first_block_started.count_down
            release_first_block.wait(1)
            value + 1
          end
        end

        first_block_started.wait(1)
        atom.reset(10)
        release_first_block.count_down
        join_with(swapping)

        expect(events).to eq [
          [0, 10, 0, 1],
          [10, 11, 1, 2]
        ]
      end

      it 'does not notify after a failed compare-and-set' do
        events = []
        atom = described_class.new(:a)
        atom.add_observer { events << :changed }
        stale_snapshot = atom.snapshot
        atom.compare_and_set(atom.snapshot, :b)

        atom.compare_and_set(stale_snapshot, :c)

        expect(events).to eq [:changed]
      end
    end

    it 'does not allow an old snapshot to overwrite an A to B to A sequence' do
      atom = described_class.new(:a)
      stale_snapshot_read = CountDownLatch.new
      aba_sequence_complete = CountDownLatch.new
      stale_snapshot = nil
      stale_update_result = nil
      first_update_result = nil
      second_update_result = nil

      stale_reader = in_thread do
        stale_snapshot = atom.snapshot
        stale_snapshot_read.count_down
        aba_sequence_complete.wait(1)
        stale_update_result = atom.compare_and_set(stale_snapshot, :c)
      end

      mutator = in_thread do
        expect(stale_snapshot_read.wait(1)).to be true
        first_update_result = atom.compare_and_set(atom.snapshot, :b)
        second_update_result = atom.compare_and_set(atom.snapshot, :a)
        aba_sequence_complete.count_down
      end

      join_with([stale_reader, mutator])

      expect(first_update_result).to be true
      expect(second_update_result).to be true
      expect(stale_snapshot).to eq [:a, 0]
      expect(stale_update_result).to be false
      expect(atom.snapshot).to eq [:a, 2]
    end
  end
end
