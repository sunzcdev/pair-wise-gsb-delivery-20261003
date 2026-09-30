require 'concurrent/atomic/phaser'
require 'concurrent/atomic/count_down_latch'
require 'concurrent/atomic/atomic_fixnum'

module Concurrent

  RSpec.describe Phaser do

    context '#initialize' do

      it 'raises an exception if the parties count is not an integer' do
        expect { described_class.new('foo') }.to raise_error(ArgumentError)
      end

      it 'raises an exception if the parties count is negative' do
        expect { described_class.new(-1) }.to raise_error(ArgumentError)
      end

      it 'accepts zero parties' do
        phaser = described_class.new(0)
        expect(phaser.registered_parties).to eq 0
        expect(phaser.unarrived_parties).to eq 0
        expect(phaser.phase).to eq 0
      end

      it 'defaults to zero parties' do
        expect(described_class.new.registered_parties).to eq 0
      end
    end

    describe '#registered_parties, #unarrived_parties, #arrived_parties' do

      it 'exposes the parties of the current phase' do
        phaser = described_class.new(3)
        expect(phaser.registered_parties).to eq 3
        expect(phaser.unarrived_parties).to eq 3
        expect(phaser.arrived_parties).to eq 0

        phaser.arrive
        expect(phaser.unarrived_parties).to eq 2
        expect(phaser.arrived_parties).to eq 1
      end
    end

    describe '#register' do

      it 'adds an unarrived party to the current phase and returns its phase' do
        phaser = described_class.new(1)

        expect(phaser.register).to eq 0
        expect(phaser.registered_parties).to eq 2
        expect(phaser.unarrived_parties).to eq 2
      end

      it 'registers the first party of a zero-party phaser in the current phase' do
        phaser = described_class.new(0)

        expect(phaser.register).to eq 0
        expect(phaser.registered_parties).to eq 1
        expect(phaser.unarrived_parties).to eq 1
      end

      it 'adds all parties of a bulk registration atomically' do
        phaser = described_class.new(1)

        expect(phaser.bulk_register(4)).to eq 0
        expect(phaser.registered_parties).to eq 5
        expect(phaser.unarrived_parties).to eq 5
      end

      it 'raises on a non-positive bulk size' do
        phaser = described_class.new(1)
        expect { phaser.bulk_register(0) }.to raise_error(ArgumentError)
        expect { phaser.bulk_register(-1) }.to raise_error(ArgumentError)
      end

      it 'assigns a registration racing the phase boundary to the next generation' do
        action_entered = CountDownLatch.new(1)
        release_action  = CountDownLatch.new(1)
        may_register    = CountDownLatch.new(1)
        register_ready  = CountDownLatch.new(1)
        phaser = described_class.new(1) do
          action_entered.count_down
          release_action.wait(5)
          false
        end

        tripper = in_thread { phaser.arrive }
        expect(action_entered.wait(5)).to be_truthy

        # freeze the registering thread right before it calls register
        registerer = in_thread do
          register_ready.count_down
          may_register.wait(5)
          phaser.register
        end
        expect(register_ready.wait(5)).to be_truthy

        # let the completing phase roll over while the registerer is parked
        release_action.count_down
        expect(tripper.join(5)).not_to eq nil
        expect(phaser.phase).to eq 1
        expect(phaser.registered_parties).to eq 1
        expect(phaser.unarrived_parties).to eq 1

        # the registration now strictly enters the next generation
        may_register.count_down
        expect(registerer.join(5)).not_to eq nil
        expect(registerer.value).to eq 1
        expect(phaser.registered_parties).to eq 2
        expect(phaser.unarrived_parties).to eq 2

        2.times { expect(phaser.arrive).to eq 1 } # both parties arrive
        expect(phaser.phase).to eq 2
      end

      it 'assigns a registration racing a tripped phase to the next generation' do
        action_entered = CountDownLatch.new(1)
        release_action  = CountDownLatch.new(1)
        phaser = described_class.new(1) do
          action_entered.count_down
          release_action.wait(5)
          false
        end

        tripper = in_thread { phaser.arrive }
        expect(action_entered.wait(5)).to be_truthy
        repeat_until_success { expect(phaser.unarrived_parties).to eq 0 }

        # the phase is tripped but its action is still running, so the party
        # joins phase one even though the reported phase number is still zero
        expect(phaser.phase).to eq 0
        expect(phaser.register).to eq 1
        expect(phaser.registered_parties).to eq 2
        expect(phaser.unarrived_parties).to eq 0

        release_action.count_down
        join_with tripper
        expect(phaser.phase).to eq 1
        expect(phaser.unarrived_parties).to eq 2

        2.times { phaser.arrive }
        expect(phaser.phase).to eq 2
      end

      it 'assigns a registration entering an open phase to that generation' do
        phaser = described_class.new(2)
        phaser.arrive # one arrival is still missing, phase zero stays open

        expect(phaser.register).to eq 0
        expect(phaser.registered_parties).to eq 3
        expect(phaser.unarrived_parties).to eq 2

        phaser.arrive
        expect(phaser.phase).to eq 0
        phaser.arrive
        expect(phaser.phase).to eq 1
      end

      it 'counts concurrent registrations exactly' do
        phaser = described_class.new(1)
        threads = 10.times.map { in_thread { phaser.register } }
        join_with threads

        expect(threads.map(&:value)).to all(eq(0))
        expect(phaser.registered_parties).to eq 11
        expect(phaser.unarrived_parties).to eq 11
      end

      it 'does not add a party when terminated' do
        phaser = described_class.new(1)
        phaser.arrive_and_deregister

        expect(phaser).to be_terminated
        expect(phaser.register).to eq 0
        expect(phaser.registered_parties).to eq 0
      end
    end

    describe '#arrive' do

      it 'decrements the unarrived count without blocking' do
        phaser = described_class.new(3)

        expect(phaser.arrive).to eq 0
        expect(phaser.phase).to eq 0
        expect(phaser.unarrived_parties).to eq 2
      end

      it 'advances the phase once every party has arrived' do
        phaser = described_class.new(2)
        phaser.arrive

        expect(phaser.arrive).to eq 0
        expect(phaser.phase).to eq 1
        expect(phaser.unarrived_parties).to eq 2
      end

      it 'raises without changing the counts when no party can arrive' do
        phaser = described_class.new(0)
        expect { phaser.arrive }.to raise_error(Concurrent::IllegalOperationError)
        expect { phaser.arrive_and_deregister }.to raise_error(Concurrent::IllegalOperationError)
        expect(phaser.unarrived_parties).to eq 0
        expect(phaser.registered_parties).to eq 0
        expect(phaser.phase).to eq 0
      end

      it 'raises on a terminated phaser' do
        phaser = described_class.new(1)
        phaser.arrive_and_deregister
        expect { phaser.arrive }.to raise_error(Concurrent::IllegalOperationError)
      end
    end

    describe '#arrive_and_deregister' do

      it 'removes the party for the next phase' do
        phaser = described_class.new(3)

        expect(phaser.arrive_and_deregister).to eq 0
        expect(phaser.registered_parties).to eq 2
        expect(phaser.unarrived_parties).to eq 2

        phaser.arrive
        phaser.arrive
        expect(phaser.phase).to eq 1
        expect(phaser.registered_parties).to eq 2
      end

      it 'terminates the phaser when the last party deregisters' do
        phaser = described_class.new(1)

        expect(phaser.arrive_and_deregister).to eq 0
        expect(phaser).to be_terminated
        expect(phaser.phase).to eq 0
      end

      it 'releases every waiting thread with the terminal state' do
        phaser = described_class.new(2)
        waiter = in_thread { phaser.await_advance(0) }
        is_sleeping(waiter)

        phaser.arrive_and_deregister # one party remains for the open phase
        is_sleeping(waiter)
        phaser.arrive_and_deregister # the final arriving party deregisters

        join_with waiter
        expect(waiter.value).to be_nil
        expect(phaser).to be_terminated
      end

      it 'does not let an old arrival token deregister from the current phase' do
        phaser = described_class.new(2)
        phaser.arrive
        phaser.arrive # advances to phase one with two parties

        # arriving in the next phase and using the previous token only waits,
        # it must not reduce the current unarrived count
        phaser.arrive
        expect(phaser.await_advance(0)).to eq 1
        expect(phaser.unarrived_parties).to eq 1
      end
    end

    describe '#await_advance' do

      it 'blocks until the given phase completes' do
        phaser = described_class.new(2)
        done     = CountDownLatch.new(1)
        waiter   = in_thread { done.count_down if phaser.await_advance(0) == 1 }

        is_sleeping(waiter)
        phaser.arrive
        phaser.arrive

        expect(done.wait(5)).to be_truthy
      end

      it 'returns immediately for an already completed phase' do
        phaser = described_class.new(1)
        phaser.arrive_and_await_advance

        expect(phaser.await_advance(0)).to eq 1
      end

      it 'never changes the counters' do
        phaser = described_class.new(2)

        expect(phaser.await_advance(99)).to eq 0
        expect(phaser.registered_parties).to eq 2
        expect(phaser.unarrived_parties).to eq 2
      end

      it 'returns nil once terminated' do
        phaser = described_class.new(1)
        phaser.arrive_and_deregister

        expect(phaser.await_advance(0)).to be_nil
      end

      it 'wakes waiters of the completed phase on termination' do
        phaser = described_class.new(2)
        waiter = in_thread { phaser.await_advance(0) }
        is_sleeping(waiter)

        expect(phaser.force_termination).to eq 0
        join_with waiter
        expect(waiter.value).to be_nil
        expect(phaser).to be_terminated
      end

      it 'returns false on timeout without changing the state' do
        phaser = described_class.new(2)

        expect(phaser.await_advance(0, 0.05)).to eq false
        expect(phaser.phase).to eq 0
        expect(phaser.unarrived_parties).to eq 2

        # the phaser is still usable afterwards
        done = CountDownLatch.new(1)
        in_thread { done.count_down if phaser.await_advance(0) == 1 }
        phaser.arrive
        phaser.arrive
        expect(done.wait(5)).to be_truthy
      end
    end

    describe '#arrive_and_await_advance' do

      it 'cycles the same parties through several phases' do
        phaser  = described_class.new(3)
        barrier = CountDownLatch.new(3)

        threads = 3.times.map do
          in_thread do
            3.times do
              advanced_to = phaser.arrive_and_await_advance
              barrier.count_down if advanced_to >= 1
            end
          end
        end

        expect(barrier.wait(5)).to be_truthy
        join_with threads
        expect(phaser.phase).to eq 3
        expect(phaser.registered_parties).to eq 3
      end

      it 'makes an arrival during the phase action wait for the rollover' do
        action_entered = CountDownLatch.new(1)
        release_action  = CountDownLatch.new(1)
        phaser = described_class.new(1) do
          action_entered.count_down
          release_action.wait(5)
          false
        end

        first = in_thread { phaser.arrive_and_await_advance }
        expect(action_entered.wait(5)).to be_truthy

        # a fast party already starting its next arrival must not touch the
        # tripped generation; it blocks until the new generation exists
        eager = in_thread { phaser.arrive_and_await_advance }
        repeat_until_success { expect(eager.status).to eq 'sleep' }
        expect(phaser.unarrived_parties).to eq 0

        release_action.count_down
        join_with [first, eager]
        expect(first.value).to eq 1
        expect(eager.value).to eq 2
        expect(phaser.phase).to eq 2
        expect(phaser.unarrived_parties).to eq 1
      end
    end

    describe 'phase action' do

      it 'runs once on the thread which advances the phase' do
        action_phase  = nil
        action_parties = nil
        action_runs   = AtomicFixnum.new(0)
        phaser = described_class.new(2) do |phase, registered_parties|
          action_phase    = phase
          action_parties  = registered_parties
          action_runs.increment
          false
        end

        phaser.arrive
        phaser.arrive

        expect(action_runs.value).to eq 1
        expect(action_phase).to eq 0
        expect(action_parties).to eq 2
      end

      it 'terminates the phaser when the action returns a truthy value' do
        phaser = described_class.new(2) { |_phase, _parties| true }
        waiter = in_thread { phaser.await_advance(0) }
        is_sleeping(waiter)

        phaser.arrive
        phaser.arrive

        join_with waiter
        expect(waiter.value).to be_nil
        expect(phaser).to be_terminated
      end

      it 'propagates a raising action to the arriving party and terminates' do
        phaser = described_class.new(2) { raise 'boom' }
        waiter = in_thread { phaser.await_advance(0) }
        is_sleeping(waiter)

        phaser.arrive
        expect { phaser.arrive }.to raise_error(RuntimeError, 'boom')

        join_with waiter
        expect(waiter.value).to be_nil
        expect(phaser).to be_terminated
        expect { phaser.arrive }.to raise_error(Concurrent::IllegalOperationError)
      end

      it 'can be overridden through on_advance' do
        subclass = Class.new(described_class) do
          attr_reader :advanced

          def initialize
            super(1)
            @advanced = []
          end

          protected

          def on_advance(phase, registered_parties)
            @advanced << [phase, registered_parties]
            false
          end
        end

        phaser = subclass.new
        expect(phaser.arrive_and_await_advance).to eq 1
        expect(phaser.advanced).to eq [[0, 1]]
        expect(phaser).not_to be_terminated
      end
    end

    describe '#force_termination' do

      it 'is idempotent and blocks further arrivals' do
        phaser = described_class.new(2)
        expect(phaser.force_termination).to eq 0
        expect(phaser.force_termination).to eq 0
        expect(phaser).to be_terminated
        expect(phaser.registered_parties).to eq 2
        expect { phaser.arrive }.to raise_error(Concurrent::IllegalOperationError)
      end
    end
  end
end
