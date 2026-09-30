require_relative 'concern/observable_shared'
require 'concurrent/versioned_atom'
require 'concurrent/atomic/count_down_latch'
require 'concurrent/atomic/atomic_fixnum'

module Concurrent

  RSpec.describe VersionedAtom do

    context 'construction' do

      it 'sets the initial value to the given value' do
        atom = VersionedAtom.new(42)
        expect(atom.value).to eq 42
      end

      it 'starts the version at zero' do
        atom = VersionedAtom.new(42)
        expect(atom.version).to eq 0
      end

      it 'returns the initial value and version as one snapshot' do
        atom = VersionedAtom.new(42)
        expect(atom.snapshot).to eq [42, 0]
      end
    end

    context '#snapshot' do

      it 'returns the value and version current at the same instant' do
        atom = VersionedAtom.new(:a)
        atom.compare_and_set(:a, 0, :b)

        value, version = atom.snapshot
        expect(value).to eq :b
        expect(version).to eq 1
      end

      it 'does not expose a writable version' do
        atom = VersionedAtom.new(0)
        expect(atom).not_to respond_to(:version=)
      end
    end

    context '#compare_and_set' do

      it 'sets the new value when value and version both match' do
        atom = VersionedAtom.new(42)
        expect(atom.compare_and_set(42, 0, :foo)).to be true
        expect(atom.value).to eq :foo
      end

      it 'increments the version by one on success' do
        atom = VersionedAtom.new(42)
        atom.compare_and_set(42, 0, :foo)
        expect(atom.version).to eq 1
      end

      it 'rejects when the value matches but the version does not' do
        atom = VersionedAtom.new(42)
        atom.compare_and_set(42, 0, :foo)
        atom.compare_and_set(:foo, 1, 42)

        expect(atom.compare_and_set(42, 0, :bar)).to be false
        expect(atom.value).to eq 42
      end

      it 'rejects when the version matches but the value does not' do
        atom = VersionedAtom.new(42)
        expect(atom.compare_and_set(:foo, 0, :bar)).to be false
        expect(atom.value).to eq 42
      end

      it 'does not advance the version on a lost race' do
        atom = VersionedAtom.new(42)
        atom.compare_and_set(:wrong, 0, :bar)
        atom.compare_and_set(42, 7, :bar)
        expect(atom.version).to eq 0
      end

      it 'does not advance the version when the validator returns false' do
        validator = ->(value) { false }
        atom = VersionedAtom.new(42, validator: validator)
        expect(atom.compare_and_set(42, 0, :foo)).to be false
        expect(atom.version).to eq 0
      end

      it 'does not advance the version when the validator raises' do
        validator = ->(value) { raise StandardError }
        atom = VersionedAtom.new(42, validator: validator)
        expect(atom.compare_and_set(42, 0, :foo)).to be false
        expect(atom.version).to eq 0
      end

      it 'advances the version even when the new value equals the old value' do
        atom = VersionedAtom.new(42)
        expect(atom.compare_and_set(42, 0, 42)).to be true
        expect(atom.value).to eq 42
        expect(atom.version).to eq 1
      end

      it 'cannot be tricked into rolling the version backwards' do
        atom = VersionedAtom.new(42)
        atom.compare_and_set(42, 0, :foo)
        atom.compare_and_set(:foo, 1, :bar)

        expect(atom.compare_and_set(:bar, 0, :baz)).to be false
        expect(atom.snapshot).to eq [:bar, 2]
      end
    end

    context 'ABA visibility' do

      it 'refuses a stale value-only snapshot after value changes A -> B -> A' do
        atom = VersionedAtom.new(:a)

        value, version = atom.snapshot
        expect([value, version]).to eq [:a, 0]

        # The observed value is changed away and back again by another actor.
        atom.compare_and_set(:a, 0, :b)
        atom.compare_and_set(:b, 1, :a)

        # The value reads identically to the stale snapshot ...
        expect(atom.value).to eq :a
        # ... but the version exposes the intervening changes ...
        expect(atom.version).to eq 2
        # ... so the old (value, version) pair cannot overwrite the state.
        expect(atom.compare_and_set(value, version, :stale)).to be false
        expect(atom.snapshot).to eq [:a, 2]
      end

      it 'detects A -> B -> A driven concurrently by two threads' do
        atom = VersionedAtom.new(:a)
        attempts = AtomicFixnum.new(0)

        # Thread 1 takes a snapshot of the initial A, then waits while the
        # other thread mutates the value B and back to A.
        snapshot_taken = CountDownLatch.new(1)
        finish_read    = CountDownLatch.new(1)

        stale_cas = nil
        reader = in_thread do
          value, version = atom.snapshot
          snapshot_taken.count_down
          finish_read.wait(2)
          attempts.increment
          stale_cas = atom.compare_and_set(value, version, :stale)
        end

        writer = in_thread do
          snapshot_taken.wait(2)
          atom.compare_and_set(:a, 0, :b)
          atom.compare_and_set(:b, 1, :a)
          finish_read.count_down
        end

        writer.join(2)
        reader.join(2)

        expect(writer).not_to be_alive
        expect(reader).not_to be_alive
        expect(atom.value).to eq :a
        expect(atom.version).to eq 2
        expect(stale_cas).to be false
        expect(attempts.value).to eq 1
      end

      it 'commits strictly increasing versions under concurrent contention', :stress do
        atom = VersionedAtom.new(0)
        thread_count = 4
        commits_per_thread = 500
        ready = CountDownLatch.new(thread_count)
        start = CountDownLatch.new(1)

        threads = Array.new(thread_count) do
          in_thread do
            ready.count_down
            start.wait(5)
            commits_per_thread.times do
              loop do
                old_value, old_version = atom.snapshot
                break if atom.compare_and_set(
                  old_value, old_version, old_value + 1
                )
              end
            end
          end
        end

        ready.wait(5)
        start.count_down
        threads.each { |thread| thread.join(10) }

        expect(atom.version).to eq thread_count * commits_per_thread
        expect(atom.value).to eq thread_count * commits_per_thread
      end
    end

    context '#swap' do

      it 'raises an exception when no block is given' do
        atom = VersionedAtom.new(42)
        expect { atom.swap }.to raise_error(ArgumentError)
      end

      it 'passes the current value to the block' do
        actual = nil
        atom = VersionedAtom.new(42)
        atom.swap { |value| actual = value; value }
        expect(actual).to eq 42
      end

      it 'sets the new value to the result of the block' do
        atom = VersionedAtom.new(42)
        atom.swap { :foo }
        expect(atom.value).to eq :foo
        expect(atom.version).to eq 1
      end

      it 'keeps the value and version when the validator returns false' do
        validator = ->(value) { false }
        atom = VersionedAtom.new(42, validator: validator)
        expect(atom.swap { 100 }).to eq 42
        expect(atom.snapshot).to eq [42, 0]
      end

      it 'keeps the value and version when the validator raises' do
        validator = ->(value) { raise StandardError }
        atom = VersionedAtom.new(42, validator: validator)
        expect(atom.swap { 100 }).to eq 42
        expect(atom.version).to eq 0
      end

      it 'reraises the exception from the block' do
        atom = VersionedAtom.new(0)
        expect {
          atom.swap { raise 'something went wrong' }
        }.to raise_error('something went wrong')
      end

      it 'retries with a fresh value and version after losing a race' do
        observed = []
        seen_versions = []
        latch1 = CountDownLatch.new(1)
        latch2 = CountDownLatch.new(1)
        atom = VersionedAtom.new(0)

        t = in_thread do
          atom.swap do |value|
            seen_versions << atom.version
            observed << value
            latch1.count_down
            latch2.wait(2)
            42
          end
        end

        latch1.wait(2)
        # Concurrent A -> B change: value becomes 100 at version 1 while the
        # swapping thread is parked on its version-0 snapshot.
        atom.compare_and_set(0, 0, 100)
        latch2.count_down
        t.join(2)

        expect(t).not_to be_alive
        expect(observed).to eq [0, 100]
        expect(seen_versions).to eq [0, 1]
        expect(atom.snapshot).to eq [42, 2]
      end

      it 'notifies with the real pre- and post-commit versions after a retry' do
        events = Queue.new
        atom = VersionedAtom.new(0)
        atom.add_observer do |_time, old_value, new_value, old_version, new_version|
          events << [old_value, new_value, old_version, new_version]
        end

        latch1 = CountDownLatch.new(1)
        latch2 = CountDownLatch.new(1)

        t = in_thread do
          atom.swap do |value|
            latch1.count_down
            latch2.wait(2)
            42
          end
        end

        latch1.wait(2)
        atom.compare_and_set(0, 0, 100)
        latch2.count_down
        t.join(2)

        committed = events.size.times.map { events.pop }
        # The intervening commit first, then the retrying swap's actual commit
        # based on version 1, never the stale version 0 it started with.
        expect(committed).to include([0, 100, 0, 1])
        expect(committed).to include([100, 42, 1, 2])
        expect(committed.any? { |e| e[2] == 0 && e[1] == 42 }).to be false
      end
    end

    context '#reset' do

      it 'sets the new value and advances the version' do
        atom = VersionedAtom.new(42)
        expect(atom.reset(:foo)).to eq :foo
        expect(atom.snapshot).to eq [:foo, 1]
      end

      it 'advances the version even when reset to the same value' do
        atom = VersionedAtom.new(42)
        atom.reset(42)
        expect(atom.snapshot).to eq [42, 1]
      end

      it 'returns the old value without a version bump when invalid' do
        validator = ->(value) { false }
        atom = VersionedAtom.new(42, validator: validator)
        expect(atom.reset(:foo)).to eq 42
        expect(atom.version).to eq 0
      end
    end

    context :observable do

      subject { VersionedAtom.new(0) }

      def trigger_observable(observable)
        observable.reset(42)
      end

      it_behaves_like :observable

      it 'notifies observers with both values and both versions' do
        events = Queue.new
        subject.add_observer do |time, old_value, new_value, old_version, new_version|
          events << [old_value, new_value, old_version, new_version, time]
        end

        subject.compare_and_set(0, 0, 1)

        old_value, new_value, old_version, new_version, time = events.pop
        expect([old_value, new_value, old_version, new_version]).to eq [0, 1, 0, 1]
        expect(time).to be_a(Time)
      end

      it 'does not notify when the conditional update loses the race' do
        notifications = AtomicFixnum.new(0)
        subject.add_observer { notifications.increment }

        subject.compare_and_set(:wrong, 0, 1)
        expect(notifications.value).to eq 0
      end
    end
  end
end
