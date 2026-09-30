require 'concurrent/atomic/phaser'
require 'concurrent/atomic/count_down_latch'
require 'concurrent/atomic/atomic_fixnum'

module Concurrent

  RSpec.describe Phaser do

    describe '#initialize' do

      it 'accepts zero initial parties' do
        expect(described_class.new.registered_parties).to eq 0
      end

      it 'raises if the initial count is negative' do
        expect { described_class.new(-1) }.to raise_error(ArgumentError)
      end

      it 'raises if the initial count is not an integer' do
        expect { described_class.new('foo') }.to raise_error(ArgumentError)
      end
    end

    describe 'basic accounting' do

      it 'starts at phase zero with the given number of parties' do
        phaser = described_class.new(2)
        expect(phaser.phase).to eq 0
        expect(phaser.registered_parties).to eq 2
        expect(phaser.unarrived_parties).to eq 2
        expect(phaser.arrived_parties).to eq 0
        expect(phaser).not_to be_terminated
      end

      it 'registers additional parties into the current phase' do
        phaser = described_class.new(1)
        expect(phaser.register).to eq 0
        expect(phaser.registered_parties).to eq 2
        expect(phaser.unarrived_parties).to eq 2
      end

      it 'raises when registering a non positive number of parties' do
        phaser = described_class.new(1)
        expect { phaser.register(0) }.to raise_error(ArgumentError)
        expect { phaser.register(-1) }.to raise_error(ArgumentError)
      end

      it 'attributes a single arrival to one phase' do
        phaser = described_class.new(2)
        expect(phaser.arrive).to eq 0
        expect(phaser.arrived_parties).to eq 1
        expect(phaser.unarrived_parties).to eq 1
        expect(phaser.phase).to eq 0
      end

      it 'advances the phase once every party has arrived' do
        phaser = described_class.new(2)
        phaser.arrive
        expect(phaser.arrive).to eq 0
        expect(phaser.phase).to eq 1
        expect(phaser.registered_parties).to eq 2
        expect(phaser.unarrived_parties).to eq 2
        expect(phaser.arrived_parties).to eq 0
      end
    end

    describe '#arrive_and_deregister' do

      it 'removes the party from future phases' do
        phaser = described_class.new(2)
        phaser.arrive_and_deregister
        expect(phaser.registered_parties).to eq 1
        expect(phaser.phase).to eq 0
      end

      it 'terminates when the last party deregisters (default action)' do
        phaser = described_class.new(1)
        expect(phaser.arrive_and_deregister).to eq 0
        expect(phaser).to be_terminated
        expect(phaser.phase).to eq 0
        expect(phaser.registered_parties).to eq 0
      end

      it 'advances and keeps running with a continuing custom action' do
        phaser = described_class.new(1) { |_phase, _registered| false }
        phaser.arrive_and_deregister
        expect(phaser).not_to be_terminated
        expect(phaser.phase).to eq 1
        expect(phaser.registered_parties).to eq 0
        expect(phaser.unarrived_parties).to eq 0
      end

      it 'carries zero parties into the next phase when the last waiter deregisters' do
        phaser = described_class.new(2) { |_phase, _registered| false }
        done = CountDownLatch.new(1)
        ready = CountDownLatch.new(1)
        result = nil
        credential = nil

        # The first party arrives, deregisters and waits for the phase to end.
        waiter = in_thread do
          ready.count_down
          credential = phaser.arrive_and_deregister
          result = phaser.await_advance(credential)
          done.count_down
        end

        ready.wait(1)
        repeat_until_success { expect(phaser.number_waiting).to eq 1 }
        expect(phaser.registered_parties).to eq 1

        # The final party also deregisters, completing the phase with the
        # continuing action carrying zero parties onward.
        phaser.arrive_and_deregister

        expect(done.wait(1)).to be_truthy
        expect(waiter.join(1)).not_to be_nil
        expect(result).to eq 1
        expect(phaser).not_to be_terminated
        expect(phaser.phase).to eq 1
        expect(phaser.registered_parties).to eq 0
        expect(phaser.unarrived_parties).to eq 0
      end
    end

    describe '#await_advance / #arrive_and_wait' do

      it 'blocks until the phase ends and returns the new phase' do
        phaser = described_class.new(2)
        result = nil
        done   = CountDownLatch.new(1)

        in_thread do
          result = phaser.arrive_and_wait
          done.count_down
        end

        repeat_until_success { expect(phaser.number_waiting).to eq 1 }
        expect(done.count).to eq 1

        phaser.arrive

        expect(done.wait(1)).to be_truthy
        expect(result).to eq 1
        expect(phaser.number_waiting).to eq 0
      end

      it 'returns immediately for an old phase credential without counting' do
        phaser = described_class.new(2)
        phaser.arrive # one of two arrived, phase 0 still open
        credential = 0

        # Reusing the current/older credential is purely a read-only wait; it
        # must not register an arrival.
        waiter = in_thread { phaser.await_advance(credential) }
        repeat_until_success { expect(phaser.number_waiting).to eq 1 }
        expect(phaser.arrived_parties).to eq 1

        phaser.arrive # closes phase 0
        expect(waiter.join(1)).not_to be_nil
        expect(waiter.value).to eq 1
        expect(phaser.arrived_parties).to eq 0
      end

      it 'returns nil on timeout' do
        phaser = described_class.new(2)
        expect(phaser.await_advance(0, 0.05)).to be_nil
      end
    end

    describe 'phase attribution at the advance boundary' do

      # A controllable action lets us park the phaser exactly at the boundary
      # between two phases without relying on sleeps.
      def phaser_with_gate(parties)
        action_started = CountDownLatch.new(1)
        release_action = CountDownLatch.new(1)
        action_calls   = AtomicFixnum.new(0)

        phaser = described_class.new(parties) do |phase, registered|
          action_calls.increment
          action_started.count_down
          release_action.wait(5)
          phase >= 100 # never terminate for the tested rounds
        end

        [phaser, action_started, release_action, action_calls]
      end

      it 'attributes a concurrent registration to the new phase' do
        phaser, started, release, = phaser_with_gate(1)

        finisher = in_thread { phaser.arrive }
        expect(started.wait(1)).to be_truthy

        registering = in_thread { phaser.register }
        repeat_until_success { expect(phaser.number_waiting).to eq 1 }

        # Still phase 0 while the boundary action is running.
        expect(phaser.phase).to eq 0
        expect(phaser.registered_parties).to eq 1

        release.count_down
        expect(registering.value).to eq 1
        expect(finisher.join(1)).not_to be_nil

        expect(phaser.phase).to eq 1
        # The new registration belongs to phase 1, not the finished phase 0.
        expect(phaser.registered_parties).to eq 2
        expect(phaser.unarrived_parties).to eq 2
      end

      it 'attributes an arrival during the boundary to the new phase' do
        phaser, started, release, = phaser_with_gate(1)

        phaser.register # a second party joins phase 0
        phaser.register # a third party joins phase 0
        phaser.arrive   # one arrival in phase 0 (two remain)

        # Two phase-0 arrivals remain; the last one trips the boundary and
        # runs the action.
        in_thread { phaser.arrive }
        tripper = in_thread { phaser.arrive }
        expect(started.wait(1)).to be_truthy

        # The third party arrives at the open boundary; it is attributed to
        # phase 1 and never consumed by the finished phase 0.
        late_phase = nil
        late = in_thread { late_phase = phaser.arrive }
        repeat_until_success { expect(phaser.number_waiting).to eq 1 }

        release.count_down
        expect(tripper.join(1)).not_to be_nil
        expect(late.join(1)).not_to be_nil

        expect(late_phase).to eq 1
        # Two of the three carried parties still have to arrive in phase 1.
        expect(phaser.phase).to eq 1
        expect(phaser.registered_parties).to eq 3
        expect(phaser.unarrived_parties).to eq 2
        expect(phaser.arrived_parties).to eq 1
      end

      it 'runs the action exactly once per advanced phase' do
        phaser, started, release, calls = phaser_with_gate(1)

        in_thread { phaser.arrive }
        expect(started.wait(1)).to be_truthy
        release.count_down

        repeat_until_success { expect(phaser.phase).to eq 1 }
        expect(calls.value).to eq 1
      end
    end

    describe 'duplicate / stray arrivals' do

      it 'never drives the unarrived count below zero' do
        phaser = described_class.new(0) { false }
        expect { phaser.arrive }.to raise_error(IllegalOperationError)
        expect(phaser.unarrived_parties).to eq 0
      end

      it 'does not let an old phase arrival leak into the next phase' do
        phaser = described_class.new(1) { false }
        phaser.arrive_and_deregister # completes phase 0 carrying zero parties
        expect(phaser.phase).to eq 1

        # A stray arrival against a phase that already finished cannot be
        # accepted (there is nobody unarrived) and must not corrupt state.
        expect { phaser.arrive }.to raise_error(IllegalOperationError)
        expect(phaser.phase).to eq 1
        expect(phaser.registered_parties).to eq 0
        expect(phaser.unarrived_parties).to eq 0

        # Joining the next phase and arriving there is independent: the arrival
        # is attributed to phase 1 and cannot have been swallowed by phase 0.
        phaser.register
        expect(phaser.arrive).to eq 1
        expect(phaser.phase).to eq 2
      end
    end

    describe 'phase action raising an error' do

      it 'terminates the phaser and releases every waiter with terminal state' do
        boom    = Class.new(StandardError)
        phaser  = described_class.new(2) { raise boom, 'phase failed' }

        waiter_result = nil
        waiter_ready  = CountDownLatch.new(1)
        waiter = in_thread do
          waiter_ready.count_down
          waiter_result = phaser.arrive_and_wait
        end

        expect(waiter_ready.wait(1)).to be_truthy
        repeat_until_success { expect(phaser.number_waiting).to eq 1 }

        expect { phaser.arrive }.to raise_error(boom)
        expect(waiter.join(1)).not_to be_nil

        expect(phaser).to be_terminated
        expect(waiter_result).to be < 0
        expect(phaser.number_waiting).to eq 0
      end

      it 'does not run the action again after a failure' do
        calls = AtomicFixnum.new(0)
        phaser = described_class.new(1) { calls.increment; raise 'boom' }

        expect { phaser.arrive }.to raise_error(RuntimeError)
        expect(calls.value).to eq 1

        # Further arrivals are rejected as terminal and never re-run it.
        expect(phaser.arrive).to be < 0
        expect(calls.value).to eq 1
      end
    end

    describe '#terminate' do

      it 'releases all waiting threads with the terminal phase' do
        phaser = described_class.new(3)
        results = []
        ready   = CountDownLatch.new(2)

        threads = 2.times.map do
          in_thread do
            ready.count_down
            results << phaser.arrive_and_wait
          end
        end

        expect(ready.wait(1)).to be_truthy
        repeat_until_success { expect(phaser.number_waiting).to eq 2 }

        phaser.terminate
        threads.each { |t| expect(t.join(1)).not_to be_nil }

        expect(phaser).to be_terminated
        expect(results).to all(be < 0)
      end

      it 'makes registrations and arrivals return a negative phase' do
        phaser = described_class.new(1)
        phaser.terminate

        expect(phaser.register).to be < 0
        expect(phaser.arrive).to be < 0
        expect(phaser.arrive_and_deregister).to be < 0
        expect(phaser.registered_parties).to eq 1
        expect(phaser.unarrived_parties).to eq 1
      end

      it 'is idempotent' do
        phaser = described_class.new(1)
        expect(phaser.terminate).to be_nil
        expect(phaser.terminate).to be_nil
        expect(phaser).to be_terminated
      end
    end

    describe 'under churn', :stress do

      it 'keeps counts consistent across many rounds of concurrent arrivals' do
        worker_count = 8
        rounds       = 200
        phaser       = described_class.new(worker_count) { false }
        errors       = Queue.new

        workers = worker_count.times.map do |worker|
          in_thread do
            begin
              rounds.times do
                arrived_phase = phaser.arrive_and_wait
                if arrived_phase.nil? || arrived_phase.negative?
                  raise "unexpected phase result #{arrived_phase.inspect}"
                end
              end
            rescue => error
              errors << error
            end
          end
        end

        workers.each { |t| expect(t.join(30)).not_to be_nil }
        expect(errors).to be_empty
        expect(phaser.phase).to eq rounds
        expect(phaser.registered_parties).to eq worker_count
        expect(phaser.unarrived_parties).to eq worker_count
      end
    end
  end
end
