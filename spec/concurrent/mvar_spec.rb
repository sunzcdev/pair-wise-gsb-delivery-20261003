require_relative 'concern/dereferenceable_shared'
require 'concurrent/mvar'

module Concurrent

  RSpec.describe MVar do

    context 'behavior' do

      # dereferenceable

      def dereferenceable_subject(value, opts = {})
        MVar.new(value, opts)
      end

      it_should_behave_like :dereferenceable
    end

    describe '#initialize' do

      it 'accepts no initial value' do
        m = MVar.new
        expect(m).to be_empty
      end

      it 'accepts an empty initial value' do
        m = MVar.new(MVar::EMPTY)
        expect(m).to be_empty
      end

      it 'accepts an initial value' do
        m = MVar.new(14)
        expect(m).not_to be_empty
      end

      it 'accepts a nil initial value' do
        m = MVar.new(nil)
        expect(m).not_to be_empty
      end

    end

    describe '#take' do

      it 'sets the MVar to empty' do
        m = MVar.new(14)
        m.take
        expect(m).to be_empty
      end

      it 'returns the value on a full MVar' do
        m = MVar.new(14)
        expect(m.take).to eq 14
      end

      it 'waits for another thread to #put' do
        m = MVar.new

        in_thread {
          sleep(0.1)
          m.put 14
        }

        expect(m.take).to eq 14
      end

      it 'returns TIMEOUT on timeout on an empty MVar' do
        m = MVar.new
        expect(m.take(0.1)).to eq MVar::TIMEOUT
      end

    end

    describe '#borrow' do

      it 'yields current value to the block and puts back value' do
        m = MVar.new(14)
        expect { |b| m.borrow(&b) }.to yield_with_args(14)
        expect(m.take).to eq(14)
      end

      it 'puts back value even if an exception is raised' do
        m = MVar.new(14)
        expect { m.borrow { fail 'boom!' } }.to raise_error('boom!')
        expect(m.take).to eq(14)
      end

      it 'returns the returned value of the block' do
        m = MVar.new(14)
        expect(m.borrow { 2 }).to eq(2)
        expect(m.take).to eq(14)
      end

      it 'returns TIMEOUT on timeout on an empty MVar' do
        m = MVar.new
        expect(m.borrow(0.1) { }).to eq MVar::TIMEOUT
      end

    end


    describe '#put' do

      it 'sets the MVar to be empty' do
        m = MVar.new(14)
        m.take
        expect(m).to be_empty
      end

      it 'sets a new value on an empty MVar' do
        m = MVar.new
        m.put 14
        expect(m.take).to eq 14
      end

      it 'waits for another thread to #take' do
        m = MVar.new(14)

        in_thread {
          sleep(0.1)
          m.take
        }

        expect(m.put(14)).to eq 14
      end

      it 'returns TIMEOUT on timeout on a full MVar' do
        m = MVar.new(14)
        expect(m.put(14, 0.1)).to eq MVar::TIMEOUT
      end

      it 'returns the value' do
        m = MVar.new
        expect(m.put(14)).to eq 14
      end

    end

    describe '#empty?' do

      it 'returns true on an empty MVar' do
        m = MVar.new
        expect(m).to be_empty
      end

      it 'returns false on a full MVar' do
        m = MVar.new(14)
        expect(m).not_to be_empty
      end

    end

    describe '#full?' do

      it 'returns false on an empty MVar' do
        m = MVar.new
        expect(m).not_to be_full
      end

      it 'returns true on a full MVar' do
        m = MVar.new(14)
        expect(m).to be_full
      end

    end

    describe '#modify' do

      it 'raises an exception when no block given' do
        m = MVar.new(14)
        expect { m.modify }.to raise_error(ArgumentError)
      end

      it 'modifies a full MVar' do
        m = MVar.new(14)
        m.modify { |v| v + 2 }
        expect(m.take).to eq 16
      end

      it 'returns the unmodified value' do
        m = MVar.new(14)
        expect(m.modify { |v| v + 2 }).to eq 14
      end

      it 'waits for another thread to #put' do
        m = MVar.new

        in_thread {
          sleep(0.1)
          m.put 14
        }

        expect(m.modify { |v| v + 2 }).to eq 14
      end

      it 'is atomic' do
        m = MVar.new(0)

        # #modify conceptually does #take and #put - but it should be atomic.
        # Check that another #put can't sneak it during the #modify.

        in_thread {
          m.modify do |v|
            sleep(0.5)
            1
          end
        }

        sleep(0.1)
        expect(m.put(2, 0.5)).to eq MVar::TIMEOUT
        expect(m.take).to eq 1
      end

      it 'returns TIMEOUT on timeout on an empty MVar' do
        m = MVar.new
        expect(m.modify(0.1) { |v| v + 2 }).to eq MVar::TIMEOUT
      end

    end

    describe '.select' do

      it 'requires at least one MVar' do
        expect { MVar.select }.to raise_error(ArgumentError)
      end

      it 'accepts an array of MVars as well as varargs' do
        m1 = MVar.new(1)
        m2 = MVar.new(2)
        expect(MVar.select([m1, m2])).to eq [0, 1]
      end

      it 'returns the winning slot index and value from a full MVar' do
        m1 = MVar.new
        m2 = MVar.new(14)
        expect(MVar.select(m1, m2)).to eq [1, 14]
        expect(m2).to be_empty
      end

      it 'picks the earliest slot in argument order when several are full' do
        m1 = MVar.new(1)
        m2 = MVar.new(2)
        m3 = MVar.new(3)
        expect(MVar.select(m1, m2, m3)).to eq [0, 1]
        # the losers keep their values
        expect(m2.take).to eq 2
        expect(m3.take).to eq 3
      end

      it 'blocks until another thread puts into any of the MVars' do
        m1 = MVar.new
        m2 = MVar.new

        in_thread {
          sleep(0.1)
          m2.put 14
        }

        expect(MVar.select(m1, m2)).to eq [1, 14]
      end

      it 'consumes exactly one slot when two producers put simultaneously' do
        200.times do
          m1 = MVar.new
          m2 = MVar.new

          barrier = Queue.new
          t1 = in_thread { barrier.pop; m1.put :one }
          t2 = in_thread { barrier.pop; m2.put :two }
          barrier << :go << :go

          index, value = MVar.select(m1, m2)
          expect([:one, :two]).to include(value)
          expect([m1, m2][index].empty?).to be true

          t1.join
          t2.join

          # exactly one slot was consumed; the loser's value is still there
          loser = [m1, m2].find(&:full?)
          expect([m1, m2].count(&:full?)).to eq 1
          expect(loser.take).to eq(value == :one ? :two : :one)
        end
      end

      it 'leaves every non-winning value available to a later #take' do
        mvars = 5.times.map { |i| MVar.new(i) }
        won = []
        5.times do
          index, value = MVar.select(*mvars)
          won << value
          expect(mvars[index]).to be_empty
        end
        expect(won.sort).to eq [0, 1, 2, 3, 4]
      end

      it 'competes with a normal #take so that exactly one of them wins' do
        100.times do
          m1 = MVar.new
          m2 = MVar.new

          taken = Queue.new
          t = in_thread { taken << m1.take }

          m1.put 1
          m2.put 2

          index, value = MVar.select(m1, m2, timeout: 5)
          if index == 0
            # the select won slot 0; the racing take gets slot 0's next value
            expect(value).to eq 1
            m1.put 3
            expect(taken.pop).to eq 3
            expect(m2.take).to eq 2
          else
            # the normal take won slot 0; the select must have taken slot 1
            expect([index, value]).to eq [1, 2]
            expect(taken.pop).to eq 1
            expect(m1).to be_empty
          end
          t.join
        end
      end

      it 'returns TIMEOUT when nothing is put in time' do
        m1 = MVar.new
        m2 = MVar.new
        expect(MVar.select(m1, m2, timeout: 0.1)).to eq MVar::TIMEOUT
        expect(m1).to be_empty
        expect(m2).to be_empty
      end

      it 'leaves no waiter registration behind after a timeout' do
        m1 = MVar.new
        m2 = MVar.new
        MVar.select(m1, m2, timeout: 0.05)

        [m1, m2].each do |m|
          waiters = m.instance_variable_get(:@select_waiters)
          expect(waiters).to be_empty
        end

        # values put after the timeout are delivered to a normal #take,
        # not stolen by the dead select
        m1.put 1
        m2.put 2
        expect(m1.take).to eq 1
        expect(m2.take).to eq 2
      end

      it 'wins deterministically when a put collides with the timeout' do
        100.times do
          m = MVar.new
          result = nil
          selector = in_thread { result = MVar.select(m, timeout: 0.05) }
          sleep(0.05) # put lands exactly as the select times out
          m.put 14
          selector.join

          if result == MVar::TIMEOUT
            # the select lost the race: the value must remain for a #take
            expect(m.take).to eq 14
          else
            # the select won: exactly one consumption happened
            expect(result).to eq [0, 14]
            expect(m).to be_empty
          end
        end
      end

      it 'supports an infinite wait when no timeout is given' do
        m1 = MVar.new
        m2 = MVar.new
        in_thread { sleep(0.1); m1.put 14 }
        expect(MVar.select(m1, m2)).to eq [0, 14]
      end

    end

    describe '#try_put!' do

      it 'returns true an empty MVar' do
        m = MVar.new
        expect(m.try_put!(14)).to eq true
      end

      it 'returns false on a full MVar' do
        m = MVar.new(14)
        expect(m.try_put!(14)).to eq false
      end

      it 'sets an empty MVar to be full' do
        m = MVar.new
        m.try_put! 14
        expect(m).to be_full
      end

    end

    describe '#try_take!' do

      it 'returns EMPTY an empty MVar' do
        m = MVar.new
        expect(m.try_take!).to eq MVar::EMPTY
      end

      it 'returns the value on a full MVar' do
        m = MVar.new(14)
        expect(m.try_take!).to eq 14
      end

      it 'sets a full MVar to be empty' do
        m = MVar.new(14)
        m.try_take!
        expect(m).to be_empty
      end

    end

    describe '#set!' do

      it 'sets an empty MVar to be full' do
        m = MVar.new
        m.set! 14
        expect(m).to be_full
      end

      it 'sets a full MVar to be full' do
        m = MVar.new(2)
        m.set! 14
        expect(m).to be_full
        expect(m.take).to eq 14
      end

      it 'returns EMPTY on an empty MVar' do
        m = MVar.new
        expect(m.set!(2)).to eq MVar::EMPTY
      end

      it 'returns the original value on a full MVar' do
        m = MVar.new(14)
        expect(m.set!(2)).to eq 14
      end

    end

    describe '#modify!' do

      it 'raises an exception when no block given' do
        m = MVar.new(14)
        expect { m.modify! }.to raise_error(ArgumentError)
      end

      it 'modifies a full MVar' do
        m = MVar.new(14)
        m.modify! { |v| v + 2 }
        expect(m.take).to eq 16
      end

      it 'modifies an empty MVar' do
        m = MVar.new
        m.modify! { |v| 14 }
        expect(m.take).to eq 14
      end

      it 'can be used to set a full MVar to empty' do
        m = MVar.new(14)
        m.modify! { |v| MVar::EMPTY }
        expect(m).to be_empty
      end

      it 'can be used to set an empty MVar to empty' do
        m = MVar.new
        m.modify! { |v| MVar::EMPTY }
        expect(m).to be_empty
      end

      it 'returns the unmodified value' do
        m = MVar.new(14)
        expect(m.modify! { |v| v + 2 }).to eq 14
      end

    end

    context 'spurious wake ups' do

      let(:m) { MVar.new }

      before(:each) do
        def m.simulate_spurious_wake_up
          @mutex.synchronize do
            @full_condition.broadcast
            @empty_condition.broadcast
          end
        end
      end

      describe '#take' do
        it 'waits for another thread to #put' do
          in_thread { sleep(0.5); m.put 14 }
          in_thread { sleep(0.1); m.simulate_spurious_wake_up }

          expect(m.take).to eq 14
        end

        it 'returns TIMEOUT on timeout on an empty MVar' do
          result = nil
          in_thread { result = m.take(0.3) }
          sleep(0.1)
          in_thread { m.simulate_spurious_wake_up }
          sleep(0.1)
          expect(result).to be_nil
          sleep(0.2)
          expect(result).to eq MVar::TIMEOUT
        end
      end

      describe '#modify' do

        it 'waits for another thread to #put' do
          in_thread { sleep(0.5); m.put 14 }
          in_thread { sleep(0.1); m.simulate_spurious_wake_up }

          expect(m.modify { |v| v + 2 }).to eq 14
        end

        it 'returns TIMEOUT on timeout on an empty MVar' do
          result = nil
          in_thread { result = m.modify(0.3) { |v| v + 2 } }
          sleep(0.1)
          in_thread { m.simulate_spurious_wake_up }
          sleep(0.1)
          expect(result).to be_nil
          sleep(0.2)
          expect(result).to eq MVar::TIMEOUT
        end
      end

      describe '#put' do

        before(:each) { m.put(42) }

        it 'waits for another thread to #take' do
          in_thread { sleep(0.5); m.take }
          in_thread { sleep(0.1); m.simulate_spurious_wake_up }

          expect(m.put(14)).to eq 14
        end

        it 'returns TIMEOUT on timeout on a full MVar' do
          # TODO (pitr-ch 15-Oct-2016): fails on jruby
          result = nil
          in_thread { result = m.put(14, 0.3) }
          sleep(0.1)
          in_thread { m.simulate_spurious_wake_up }
          sleep(0.1)
          expect(result).to be_nil
          sleep(0.2)
          expect(result).to eq MVar::TIMEOUT
        end
      end


    end

  end

end
