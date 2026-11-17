require_relative 'concern/observable_shared'
require 'stringio'
require 'concurrent/atom'
require 'concurrent/array'
require 'concurrent/atomic/atomic_fixnum'
require 'concurrent/atomic/count_down_latch'

module Concurrent

  RSpec.describe Atom do

    context 'construction' do

      it 'sets the initial value to the given value' do
        atom = Atom.new(42)
        expect(atom.value).to eq 42
      end
    end

    context '#compare_and_set' do

      it 'sets the new value if the current value matches' do
        atom = Atom.new(42)
        atom.compare_and_set(42, :foo)
        expect(atom.value).to eq :foo
      end

      it 'returns true if the current value matches' do
        atom = Atom.new(42)
        expect(atom.compare_and_set(42, :foo)).to be true
      end

      it 'rejects the new value if the current value does not match' do
        atom = Atom.new(42)
        atom.compare_and_set(:foo, 'bar')
        expect(atom.value).to eq 42
      end

      it 'returns false if the current value does not match' do
        atom = Atom.new(42)
        expect(atom.compare_and_set(:foo, 'bar')).to be false
      end

      it 'rejects the new value if the validator returns false' do
        validator = ->(value){ false }
        atom = Atom.new(42, validator: validator)
        atom.compare_and_set(42, :foo)
        expect(atom.value).to eq 42
      end

      it 'rejects the new value if the validator raises an exception' do
        validator = ->(value){ raise StandardError }
        atom = Atom.new(42, validator: validator)
        atom.compare_and_set(42, :foo)
        expect(atom.value).to eq 42
      end

      it 'returns false if the validator returns false' do
        validator = ->(value){ false }
        atom = Atom.new(42, validator: validator)
        expect(atom.compare_and_set(42, :foo)).to be false
      end

      it 'returns false if the validator raises an exception' do
        validator = ->(value){ raise StandardError }
        atom = Atom.new(42, validator: validator)
        expect(atom.compare_and_set(42, :foo)).to be false
      end
    end

    context '#swap' do

      it 'raises an exception when no block is given' do
        atom = Atom.new(42)
        expect {
          atom.swap
        }.to raise_error(ArgumentError)
      end

      it 'passes the current value to the block' do
        actual = nil
        expected = 42
        atom = Atom.new(expected)
        atom.swap do |value|
          actual = value
        end
        expect(actual).to eq expected
      end

      it 'passes all arguments to the block' do
        actual = nil
        expected = [1, 2, 3]
        atom = Atom.new(42)
        atom.swap(*expected) do |value, *args|
          actual = args
        end
        expect(actual).to eq expected
      end

      it 'sets the new value to the result of the block' do
        atom = Atom.new(42)
        atom.swap{ :foo }
        expect(atom.value).to eq :foo
      end

      it 'rejects the new value if the validator returns false' do
        validator = ->(value){ false }
        atom = Atom.new(42, validator: validator)
        atom.swap{ 100 }
        expect(atom.value).to eq 42
      end

      it 'rejects the new value if the validator raises an exception' do
        validator = ->(value){ raise StandardError }
        atom = Atom.new(42, validator: validator)
        atom.swap{ 100 }
        expect(atom.value).to eq 42
      end

      it 'returns the new value on success' do
        atom = Atom.new(42)
        expect(atom.swap{ :foo }).to eq :foo
      end

      it 'returns the old value if the validator returns false' do
        validator = ->(value){ false }
        atom = Atom.new(42, validator: validator)
        expect(atom.swap{ 100 }).to eq 42
      end

      it 'returns the old value if the validator raises an exception' do
        validator = ->(value){ raise StandardError }
        atom = Atom.new(42, validator: validator)
        expect(atom.swap{ 100 }).to eq 42
      end

      it 'calls the block more than once if the value changes underneath' do
        latch1 = Concurrent::CountDownLatch.new
        latch2 = Concurrent::CountDownLatch.new
        counter = Concurrent::AtomicFixnum.new(0)
        atom = Atom.new(0)

        t = in_thread do
          atom.swap do |value|
            latch1.count_down
            latch2.wait(1)
            counter.increment
            42
          end
        end

        latch1.wait(1)
        atom.swap{ 100 }
        latch2.count_down
        t.join(1)

        expect(counter.value).to be > 1
      end

      it 'reraises the exception from block' do
        atom = Atom.new(0)
        expect do
          atom.swap do |value|
            fail 'something went wrong'
          end
        end.to raise_error 'something went wrong'
      end
    end

    context '#reset' do

      it 'sets the new value' do
        atom = Atom.new(42)
        atom.reset(:foo)
        expect(atom.value).to eq :foo
      end

      it 'returns the new value on success' do
        atom = Atom.new(42)
        expect(atom.reset(:foo)).to eq :foo
      end

      it 'returns the new value on success' do
        atom = Atom.new(42)
        expect(atom.reset(:foo)).to eq :foo
      end

      it 'returns the old value if the validator returns false' do
        validator = ->(value){ false }
        atom = Atom.new(42, validator: validator)
        expect(atom.reset(:foo)).to eq 42
      end

      it 'returns the old value if the validator raises an exception' do
        validator = ->(value){ raise StandardError }
        atom = Atom.new(42, validator: validator)
        expect(atom.reset(:foo)).to eq 42
      end
    end

    context :observable do

      subject { Atom.new(0) }

      def trigger_observable(observable)
        observable.reset(42)
      end

      it_behaves_like :observable
    end

    context 'versioned observation' do

      # Records the arguments of every notification as `[old_value, new_value,
      # version]`; the time argument is checked separately where it matters.
      let(:bucket) { [] }
      let(:recorder) { ->(time, old_value, new_value, version) { bucket << [old_value, new_value, version] } }

      context 'notification payload' do

        it 'passes the time, the old value, the new value, and the version' do
          atom = Atom.new(0)
          received = []
          atom.add_observer { |*args| received << args }

          before = Time.now
          atom.swap { 1 }
          after = Time.now

          expect(received.length).to eq 1
          time, old_value, new_value, version = received.first
          expect(time).to be_a(Time)
          expect(time).to be_between(before, after)
          expect(old_value).to eq 0
          expect(new_value).to eq 1
          expect(version).to eq 1
        end

        it 'starts the version at zero before any change' do
          expect(Atom.new(:initial).version).to eq 0
        end

        it 'increments the version once per committed change' do
          atom = Atom.new(0)
          expect { atom.swap { 1 } }.to change { atom.version }.from(0).to(1)
          expect { atom.reset(2) }.to change { atom.version }.from(1).to(2)
          expect { atom.compare_and_set(2, 3) }.to change { atom.version }.from(2).to(3)
        end

        it 'reports the version of the committed value through #version' do
          atom = Atom.new(0)
          atom.add_observer(&recorder)

          atom.swap { |value| value + 1 }
          expect(bucket.last[2]).to eq atom.version

          atom.reset(5)
          expect(bucket.last[2]).to eq atom.version
        end

        it 'reports old_value and new_value as the values around the commit' do
          atom = Atom.new(0)
          atom.add_observer(&recorder)

          atom.reset(:a)
          atom.swap { :b }
          atom.compare_and_set(:b, :c)

          expect(bucket).to eq [[0, :a, 1], [:a, :b, 2], [:b, :c, 3]]
        end

        it 'keeps working with observers written for the older three arguments' do
          atom = Atom.new(0)
          received = []
          atom.add_observer { |time, old_value, new_value| received << [old_value, new_value] }

          atom.reset(42)

          expect(received).to eq [[0, 42]]
        end
      end

      context 'retrying update blocks' do

        it 'notifies only for the value that is actually committed' do
          atom = Atom.new(0)
          atom.add_observer(&recorder)

          attempts = 0
          atom.swap do |value|
            attempts += 1
            value + 1
          end

          expect(attempts).to eq 1
          expect(bucket).to eq [[0, 1, 1]]
        end

        it 'does not notify for attempts that lose the race against another writer' do
          atom = Atom.new(0)
          atom.add_observer(&recorder)

          latch_entered = Concurrent::CountDownLatch.new
          latch_release = Concurrent::CountDownLatch.new

          contender = in_thread do
            atom.swap do |value|
              latch_entered.count_down
              latch_release.wait(1)
              value + 1
            end
          end

          latch_entered.wait(1)
          # commits 1 while the contender is still computing from 0
          atom.reset(1)
          latch_release.count_down
          contender.join(1)

          # the contender recomputes from 1 and commits 2; the abandoned
          # attempt from 0 must never be announced
          expect(bucket.map { |_, new_value, _| new_value }).to eq [1, 2]
          expect(atom.value).to eq 2
        end

        it 'does not notify when the validator rejects the value' do
          atom = Atom.new(0, validator: ->(value) { value.even? })
          atom.add_observer(&recorder)

          expect(atom.swap { 1 }).to eq 0
          expect(atom.reset(3)).to eq 0
          expect(atom.compare_and_set(0, 5)).to be false

          expect(bucket).to be_empty
          expect(atom.version).to eq 0
        end

        it 'does not notify when the validator raises' do
          atom = Atom.new(0, validator: ->(_value) { raise StandardError })
          atom.add_observer(&recorder)

          atom.swap { 1 }
          atom.reset(2)
          atom.compare_and_set(0, 3)

          expect(bucket).to be_empty
          expect(atom.value).to eq 0
        end

        it 'does not notify a compare-and-set which does not match the current value' do
          atom = Atom.new(0)
          atom.add_observer(&recorder)

          expect(atom.compare_and_set(:missing, 1)).to be false

          expect(bucket).to be_empty
          expect(atom.version).to eq 0
        end

        it 'calls the update block again when it loses the race' do
          atom = Atom.new(0)
          atom.add_observer(&recorder)

          calls = 0
          atom.swap do |value|
            calls += 1
            # on the first call, sneak in a competing commit before returning
            atom.reset(5) if calls == 1
            value + 1
          end

          expect(calls).to be > 1
          expect(bucket.map { |_, new_value, _| new_value }).to eq [5, 6]
          expect(atom.version).to eq bucket.last[2]
        end
      end

      context 'failing observers' do

        it 'does not roll back an already committed value' do
          atom = Atom.new(0)
          atom.add_observer { |*_args| raise 'observer failure' }

          expect { atom.swap { 42 } }.not_to raise_error
          expect(atom.value).to eq 42
          expect(atom.version).to eq 1
        end

        it 'still notifies the remaining observers exactly once' do
          atom = Atom.new(0)
          log = []

          atom.add_observer { |*_args| log << :first; raise 'observer failure' }
          atom.add_observer { |*args| log << [:second, args[3]] }
          atom.add_observer { |*args| log << [:third, args[3]] }

          expect { atom.reset(:committed) }.not_to raise_error

          expect(log).to eq [:first, [:second, 1], [:third, 1]]
        end

        it 'swallows the observer error and keeps the returned value correct' do
          atom = Atom.new(0)
          atom.add_observer { |*_args| raise 'observer failure' }

          expect(atom.swap { :new }).to eq :new
          expect(atom.reset(:other)).to eq :other
          expect(atom.compare_and_set(:other, :third)).to be true
        end

        it 'isolates the remaining observers even when an Exception is raised' do
          atom = Atom.new(0)
          log = []

          atom.add_observer { |*_args| log << :first; raise Exception, 'non standard' }
          atom.add_observer { |*args| log << [:second, args[3]] }

          expect { atom.reset(1) }.not_to raise_error

          expect(log).to eq [:first, [:second, 1]]
          expect(atom.value).to eq 1
        end

        it 'logs the observer error without leaking it to the caller' do
          atom = Atom.new(0)
          atom.add_observer { |*_args| raise 'the failure' }
          logger = Concurrent.create_simple_logger(:DEBUG, StringIO.new)
          messages = []
          allow(Concurrent).to receive(:global_logger) { logger }
          allow(logger).to receive(:call) { |*args, &block| messages << [args, block && block.call] }

          atom.reset(42)

          expect(messages.flatten.compact.join).to match(/the failure/)
        end
      end

      context 're-entrant updates from an observer' do

        it 'delivers nested notifications in increasing version order' do
          atom = Atom.new(0)
          versions = []
          atom.add_observer do |_time, _old_value, _new_value, version|
            versions << version
            atom.swap { |value| value + 1 } if version < 4
          end

          atom.swap { |value| value + 1 }

          expect(versions).to eq [1, 2, 3, 4]
          expect(atom.value).to eq 4
          expect(atom.version).to eq 4
        end

        it 'chains old_value of a notification to new_value of the previous one' do
          atom = Atom.new(0)
          transitions = []
          atom.add_observer do |_time, old_value, new_value, version|
            transitions << [old_value, new_value, version]
            atom.swap { |value| value + 1 } if version < 3
          end

          atom.swap { |value| value + 1 }

          expect(transitions).to eq [[0, 1, 1], [1, 2, 2], [2, 3, 3]]
        end

        it 'does not repeat a notification because of a nested update' do
          atom = Atom.new(0)
          calls = Hash.new(0)
          first = ->(_time, _old_value, _new_value, version) do
            calls[:first] += 1
            atom.reset(:nested) if version == 1
          end
          atom.add_observer(&first)
          atom.add_observer { |_time, _old_value, _new_value, _version| calls[:second] += 1 }

          atom.reset(:outer)

          expect(calls[:first]).to eq 2
          expect(calls[:second]).to eq 2
          expect(atom.value).to eq :nested
        end

        it 'returns from the nested update without deadlocking' do
          atom = Atom.new(0)
          atom.add_observer do |_time, _old_value, _new_value, version|
            atom.swap { |value| value + 1 } if version == 1
          end

          result = in_thread { atom.swap { |value| value + 1 } }
          expect(result.join(5)).not_to be_nil
          expect(atom.value).to eq 2
        end

        it 'does not deadlock when an observer waits for another thread that updates the atom' do
          atom = Atom.new(0)
          updated = Concurrent::CountDownLatch.new
          atom.add_observer do |_time, _old_value, _new_value, version|
            next unless version == 1
            # trigger an update from a different thread and wait for it
            in_thread { atom.reset(:external); updated.count_down }
            expect(updated.wait(5)).to be true
          end

          result = in_thread { atom.swap { |value| value + 1 } }
          expect(result.join(5)).not_to be_nil
          expect(atom.value).to eq :external
        end
      end

      context 'concurrent observers' do

        it 'delivers every change exactly once to every observer' do
          atom = Atom.new(0)
          seen = Concurrent::AtomicFixnum.new(0)
          versions = Concurrent::Array.new

          atom.add_observer { |_time, _old_value, _new_value, version| versions << version }
          atom.add_observer { |*_args| seen.increment }

          threads = 4.times.map do
            in_thread do
              25.times { atom.swap { |value| value + 1 } }
            end
          end
          threads.each { |thread| thread.join(5) }

          expect(atom.value).to eq 100
          expect(atom.version).to eq 100
          expect(seen.value).to eq 100
          expect(versions.size).to eq 100
          expect(versions.sort).to eq (1..100).to_a
        end

        it 'notifies each observer with strictly increasing versions' do
          atom = Atom.new(0)
          versions = Concurrent::Array.new
          atom.add_observer { |_time, _old_value, _new_value, version| versions << version }

          threads = 4.times.map { in_thread { 25.times { atom.swap { |value| value + 1 } } } }
          threads.each { |thread| thread.join(5) }

          expect(versions).to eq versions.sort
        end
      end

      context 'retaining values' do

        it 'does not keep announced values reachable from the atom' do
          atom = Atom.new(:old)
          announced = Concurrent::CountDownLatch.new
          # A real observer would copy what it needs; this one only counts.
          atom.add_observer { |*_args| announced.count_down }

          3.times { atom.swap { Object.new } }

          expect(announced.wait(1)).to be true
          expect(atom.instance_variables).not_to include(:@old_value, :@new_value)

          queue = atom.instance_variable_get(:@Changes)
          expect(queue).to be_empty
        end
      end
    end
  end
end
