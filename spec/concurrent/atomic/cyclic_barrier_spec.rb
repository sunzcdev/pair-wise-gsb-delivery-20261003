require 'concurrent/atomic/cyclic_barrier'
require 'concurrent/atomic/count_down_latch'
require 'concurrent/atomic/atomic_fixnum'

module Concurrent

  RSpec.describe CyclicBarrier do

    let(:parties) { 3 }
    let!(:barrier) { described_class.new(3) }

    context '#initialize' do

      it 'raises an exception if the initial count is less than 1' do
        expect {
          described_class.new(0)
        }.to raise_error(ArgumentError)
      end

      it 'raises an exception if the initial count is not an integer' do
        expect {
          described_class.new('foo')
        }.to raise_error(ArgumentError)
      end
    end

    describe '#parties' do

      it 'should be the value passed to the constructor' do
        expect(barrier.parties).to eq 3
      end

    end

    describe '#number_waiting' do
      context 'without any waiting thread' do
        it 'should be equal to zero' do
          expect(barrier.number_waiting).to eq 0
        end
      end

      context 'with waiting threads' do
        it 'should be equal to the waiting threads count' do
          in_thread { barrier.wait }
          in_thread { barrier.wait }
          repeat_until_success { expect(barrier.number_waiting).to eq 2 }
        end
      end
    end

    describe '#broken?' do
      it 'should not be broken when created' do
        expect(barrier.broken?).to eq false
      end

      it 'should not be broken when reset is called without waiting thread' do
        barrier.reset
        expect(barrier.broken?).to eq false
      end
    end

    describe '#generation' do
      it 'starts at zero' do
        expect(barrier.generation).to eq 0
      end

      it 'advances by one when the barrier trips' do
        latch = CountDownLatch.new(parties)

        parties.times { in_thread { barrier.wait; latch.count_down } }
        expect(latch.wait(1)).to be_truthy

        expect(barrier.generation).to eq 1
        expect(barrier).not_to be_broken
      end

      it 'advances on reset and keeps advancing across rounds' do
        expect { barrier.reset }.to change { barrier.generation }.from(0).to(1)
        expect { barrier.reset }.to change { barrier.generation }.from(1).to(2)
      end

      it 'does not change while the generation is broken' do
        t = in_thread { barrier.wait(0.01) }
        join_with t

        expect(barrier).to be_broken
        expect(barrier.generation).to eq 0

        expect { barrier.reset }.to change { barrier.generation }.to 1
      end

      it 'is shared by the waiters of one round and changes for the next one' do
        observed = ::Queue.new
        latch    = CountDownLatch.new(parties)

        parties.times do
          in_thread do
            before = barrier.generation
            result = barrier.wait
            observed << [before, barrier.generation, result]
            latch.count_down
          end
        end

        expect(latch.wait(1)).to be_truthy

        entries = parties.times.map { observed.pop }
        expect(entries.map(&:first).uniq).to eq [0]
        expect(entries.map(&:last).uniq).to eq [true]
        expect(entries.map { |_, after, _| after }.uniq).to eq [1]
      end
    end

    describe 'reset' do
      it 'should release all waiting threads' do
        start_latch    = CountDownLatch.new(1)
        continue_latch = CountDownLatch.new(1)

        in_thread do
          start_latch.count_down
          barrier.wait
          continue_latch.count_down
        end

        start_latch.wait(1)
        repeat_until_success { expect(barrier.number_waiting).to eq 1 }
        barrier.reset

        expect(barrier).not_to be_broken
        expect(barrier.number_waiting).to eq 0
      end

      it 'wakes the old generation without letting it join the new one' do
        results = ::Queue.new

        old_waiter = in_thread { results << barrier.wait }
        repeat_until_success { expect(barrier.number_waiting).to eq 1 }
        old_waiter.join(1)

        barrier.reset
        expect(results.pop).to eq false

        # the new generation accepts waiters independently of the old round
        latch = CountDownLatch.new(parties)
        parties.times { in_thread { results << barrier.wait; latch.count_down } }
        expect(latch.wait(1)).to be_truthy
        expect(parties.times.map { results.pop }).to eq Array.new(parties, true)
      end

      it 'does not deliver a stale wake-up from the previous generation' do
        results = ::Queue.new
        barrier = described_class.new(2)

        # break one generation, then start a fresh one
        timed_out = in_thread { results << barrier.wait(0.01) }
        expect(results.pop).to eq false
        expect(barrier).to be_broken
        join_with timed_out

        barrier.reset
        expect(barrier.generation).to eq 1

        waiter = in_thread { results << barrier.wait }
        repeat_until_success { expect(barrier.number_waiting).to eq 1 }

        # the surviving waiter belongs to generation 1 and must not return
        # until its own generation completes
        expect(waiter.join(0.1)).to be_nil

        in_thread { results << barrier.wait }
        expect(results.pop).to eq true
        expect(results.pop).to eq true
      end
    end

    describe '#wait' do
      context 'without timeout' do
        it 'should block the thread' do
          t = in_thread { barrier.wait }
          t.join(0.1)

          expect(t.status).to eq 'sleep'
        end

        it 'should release all threads when their number matches the desired one' do
          latch = CountDownLatch.new(parties)

          parties.times { in_thread { barrier.wait; latch.count_down } }
          expect(latch.wait(1)).to be_truthy
          expect(barrier.number_waiting).to eq 0
          expect(barrier).not_to be_broken
        end

        it 'returns true when released' do
          latch = CountDownLatch.new(parties)

          parties.times { in_thread { latch.count_down if barrier.wait == true } }
          expect(latch.wait(1)).to be_truthy
        end

        it 'executes the block once' do
          counter = AtomicFixnum.new
          barrier = described_class.new(parties) { counter.increment }

          latch = CountDownLatch.new(parties)

          parties.times { in_thread { latch.count_down if barrier.wait == true } }
          expect(latch.wait(1)).to be_truthy

          expect(counter.value).to eq 1
        end

        it 'can be reused' do
          first_latch = CountDownLatch.new(parties)
          parties.times { in_thread { barrier.wait; first_latch.count_down } }

          latch = CountDownLatch.new(parties)
          parties.times { in_thread { barrier.wait; latch.count_down } }
          expect(latch.wait(1)).to be_truthy
        end

        it 'return false if barrier has been reset' do
          latch = CountDownLatch.new(1)

          t = in_thread { latch.count_down if barrier.wait == false }
          t.join(0.1)
          barrier.reset
          expect(latch.wait(1)).to be_truthy
        end
      end

      context 'with timeout' do
        context 'timeout not expiring' do
          it 'should block the thread' do
            t = in_thread { barrier.wait(1) }
            t.join(0.1)

            expect(t.status).to eq 'sleep'
          end

          it 'should release all threads when their number matches the desired one' do
            latch = CountDownLatch.new(parties)

            parties.times { in_thread { barrier.wait(1); latch.count_down } }
            expect(latch.wait(0.2)).to be_truthy
            expect(barrier.number_waiting).to eq 0
          end

          it 'returns true when released' do
            latch = CountDownLatch.new(parties)

            parties.times { in_thread { latch.count_down if barrier.wait(1) == true } }
            expect(latch.wait(1)).to be_truthy
          end
        end

        context 'timeout expiring' do

          it 'returns false' do
            latch = CountDownLatch.new(1)

            in_thread { latch.count_down if barrier.wait(0.1) == false }
            expect(latch.wait(1)).to be_truthy
          end

          it 'breaks the barrier and release all other threads' do
            latch = CountDownLatch.new(2)

            in_thread { barrier.wait(0.1); latch.count_down }
            in_thread { barrier.wait; latch.count_down }

            expect(latch.wait(1)).to be_truthy
            expect(barrier).to be_broken
          end

          it 'breaks the barrier and release all other threads 2' do
            t1 = in_thread { barrier.wait(0.1) }
            t2 = in_thread { barrier.wait(0.1) }

            [t1, t2].each(&:join)

            expect(barrier).to be_broken
          end

          it 'does not execute the block on timeout' do
            counter = AtomicFixnum.new
            barrier = described_class.new(parties) { counter.increment }

            barrier.wait(0.1)

            expect(counter.value).to eq 0
          end
        end
      end

      context '#broken barrier' do
        it 'should not accept new threads' do
          t = in_thread { barrier.wait(0.01) }
          join_with t

          expect(barrier).to be_broken
          expect(barrier.wait).to be_falsey
        end

        it 'can be reset' do
          t = in_thread { barrier.wait(0.01) }
          join_with t

          expect(barrier).to be_broken

          barrier.reset

          expect(barrier).not_to be_broken
        end
      end

      context 'barrier action raising an exception' do
        let(:action_error) { Class.new(StandardError) }

        it 're-raises the exception in the designated thread and breaks the round' do
          results     = ::Queue.new
          action_runs = AtomicFixnum.new(0)
          barrier = described_class.new(parties) do
            action_runs.increment
            raise action_error, 'boom'
          end

          threads = parties.times.map do
            in_thread do
              begin
                results << barrier.wait
              rescue action_error
                results << :raised
              end
            end
          end
          join_with threads

          outcomes = parties.times.map { results.pop }
          expect(outcomes.grep(Symbol)).to eq [:raised]
          expect(outcomes.grep(false)).to eq Array.new(parties - 1, false)
          expect(action_runs.value).to eq 1
          expect(barrier).to be_broken
          expect(barrier.number_waiting).to eq 0
        end

        it 'makes every waiter of the round observe the same broken generation' do
          ready_latch = CountDownLatch.new(parties - 1)
          results     = ::Queue.new
          barrier = described_class.new(parties) { raise 'boom' }

          (parties - 1).times do
            in_thread do
              ready_latch.count_down
              begin
                results << barrier.wait
              rescue StandardError
                results << :raised
              end
            end
          end
          expect(ready_latch.wait(1)).to be_truthy
          repeat_until_success { expect(barrier.number_waiting).to eq parties - 1 }

          last = in_thread do
            begin
              results << barrier.wait
            rescue StandardError
              results << :raised
            end
          end
          join_with last

          outcomes = parties.times.map { results.pop }
          expect(outcomes).to contain_exactly(:raised, *Array.new(parties - 1, false))
          expect(barrier).to be_broken
        end

        it 'does not run the action again for a latecomer' do
          action_runs = AtomicFixnum.new(0)
          barrier = described_class.new(parties) do
            action_runs.increment
            raise 'boom'
          end

          threads = parties.times.map do
            in_thread do
              begin
                barrier.wait
              rescue StandardError
                nil
              end
            end
          end
          join_with threads

          expect(barrier.wait).to eq false
          expect(barrier).to be_broken
          expect(action_runs.value).to eq 1
        end

        it 'can query the barrier state from inside the action without deadlocking' do
          observed = ::Queue.new
          barrier = described_class.new(parties) do
            # re-entrant status inspection performed by the designated thread
            observed << {
              waiting:    barrier.number_waiting,
              broken:     barrier.broken?,
              parties:    barrier.parties,
              generation: barrier.generation
            }
          end

          latch = CountDownLatch.new(parties)
          parties.times { in_thread { barrier.wait; latch.count_down } }

          expect(latch.wait(1)).to be_truthy
          snapshot = observed.pop
          expect(snapshot[:waiting]).to eq parties
          expect(snapshot[:broken]).to eq false
          expect(snapshot[:parties]).to eq parties
          expect(snapshot[:generation]).to eq 0
          expect(observed.empty?).to eq true
          expect(barrier).not_to be_broken
        end
      end

      context 'timeouts' do
        it 'measures the timeout against the monotonic clock' do
          # ns_wait_until (provided by the synchronization layer) drives the
          # wait with Concurrent.monotonic_time rather than the wall clock
          calls = AtomicFixnum.new(0)
          allow(Concurrent).to receive(:monotonic_time).and_wrap_original do |original, *args|
            calls.increment
            original.call(*args)
          end

          expect(barrier.wait(0.01)).to eq false

          expect(calls.value).to be > 0
          expect(barrier).to be_broken
        end

        it 'is unaffected by wall-clock travel' do
          require 'timecop'

          start = Concurrent.monotonic_time
          Timecop.travel(Time.now + 3600) do
            expect(barrier.wait(0.05)).to eq false
          end
          elapsed = Concurrent.monotonic_time - start

          expect(elapsed).to be < 1
          expect(barrier).to be_broken
        end
      end
    end

    context 'spurious wake ups' do

      before(:each) do
        def barrier.simulate_spurious_wake_up
          synchronize do
            ns_signal
            ns_broadcast
          end
        end
      end

      it 'should resist to spurious wake ups without timeout' do
        @expected = false
        t         = in_thread { barrier.wait; @expected = true }
        t.join(0.1)

        barrier.simulate_spurious_wake_up

        t.join(0.1)
        expect(@expected).to be_falsey
      end

      it 'should resist to spurious wake ups with timeout' do
        @expected = false
        t         = in_thread { barrier.wait(0.5); @expected = true }

        t.join(0.1)
        barrier.simulate_spurious_wake_up

        t.join(0.1)
        expect(@expected).to be_falsey

      end
    end
  end
end
