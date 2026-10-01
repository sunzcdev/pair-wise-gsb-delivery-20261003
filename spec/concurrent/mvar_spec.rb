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

    describe '.select' do

      it 'raises when no MVar is given' do
        expect { MVar.select }.to raise_error(ArgumentError)
      end

      it 'raises when an argument is not an MVar' do
        expect { MVar.select(MVar.new, 14) }.to raise_error(ArgumentError)
      end

      it 'takes from a single full MVar and returns index and value' do
        m = MVar.new(14)
        expect(MVar.select(m)).to eq [0, 14]
        expect(m).to be_empty
      end

      it 'waits for another thread to #put on a single empty MVar' do
        m = MVar.new
        producer = in_thread { sleep(0.1); m.put 14 }
        expect(MVar.select(m)).to eq [0, 14]
        expect(producer.join(5)).not_to be_nil
      end

      it 'takes from the earliest full slot when several are full' do
        a = MVar.new(:a)
        b = MVar.new(:b)
        c = MVar.new(:c)

        expect(MVar.select(a, b, c)).to eq [0, :a]
        expect(a).to be_empty
        expect(b).to be_full
        expect(c).to be_full
      end

      it 'ignores leading empty slots and takes from the first full slot' do
        a = MVar.new
        b = MVar.new(:b)
        c = MVar.new(:c)

        expect(MVar.select(a, b, c)).to eq [1, :b]
        expect(a).to be_empty
        expect(b).to be_empty
        expect(c).to be_full
      end

      it 'returns the correct index when a later slot becomes full first' do
        a = MVar.new
        b = MVar.new
        c = MVar.new

        producer = in_thread { sleep(0.05); c.put(:c) }

        expect(MVar.select(a, b, c)).to eq [2, :c]
        expect(producer.join(5)).not_to be_nil
      end

      it 'returns TIMEOUT when no slot becomes full' do
        a = MVar.new
        b = MVar.new
        expect(MVar.select(a, b, timeout: 0.1)).to eq MVar::TIMEOUT
      end

      it 'does not leave a waiting registration behind after a timeout' do
        m = MVar.new
        expect(MVar.select(m, timeout: 0.05)).to eq MVar::TIMEOUT

        # A registration that timed out must not consume values put later.
        producer = in_thread { sleep(0.1); m.put(14) }
        expect(m.take).to eq 14
        expect(m).to be_empty
        expect(producer.join(5)).not_to be_nil
      end

      it 'leaves values of non-winning full MVars untouched' do
        a = MVar.new(:a)
        b = MVar.new(:b)

        expect(MVar.select(a, b)).to eq [0, :a]
        expect(a.take(0.01)).to eq MVar::TIMEOUT
        expect(b.take).to eq :b
      end

      it 'keeps every non-winning value available for ordinary takes' do
        a = MVar.new
        b = MVar.new
        c = MVar.new

        gate = Queue.new
        producers = [
          in_thread { gate.pop; a.put(:a) },
          in_thread { gate.pop; b.put(:b) },
          in_thread { gate.pop; c.put(:c) }
        ]

        result = nil
        thread = in_thread { result = MVar.select(a, b, c) }
        sleep 0.1
        gate << nil << nil << nil

        expect(thread.value).to eq(result)
        index, value = result
        expect(value).to eq [:a, :b, :c][index]

        losers = [[:a, a], [:b, b], [:c, c]].reject { |name, _| name == value }
        losers.each do |name, mvar|
          expect(mvar.take).to eq name
        end
        [a, b, c].each { |mvar| expect(mvar).to be_empty }
        producers.each { |producer| expect(producer.join(5)).not_to be_nil }
      end

      it 'consumes exactly one value when two producers put concurrently' do
        50.times do
          a = MVar.new
          b = MVar.new
          gate = Queue.new

          producers = [
            in_thread { gate.pop; a.put(:a) },
            in_thread { gate.pop; b.put(:b) }
          ]
          selector = in_thread { gate.pop; MVar.select(a, b) }

          sleep 0.05
          gate << nil << nil << nil

          index, value = selector.value
          expect(index).to eq(value == :a ? 0 : 1)

          expect(a.take).to eq(:a) if value == :b
          expect(b.take).to eq(:b) if value == :a
          expect(a).to be_empty
          expect(b).to be_empty

          producers.each { |thread| expect(thread.join(5)).not_to be_nil }
        end
      end

      it 'has exactly one winner when a take competes with a selection' do
        50.times do
          m = MVar.new
          winners = Queue.new

          taker = in_thread { winners << [:take, m.take] }
          selector = in_thread { winners << [:select, MVar.select(m).last] }

          sleep 0.05
          m.put(:x)

          kind, value = winners.pop
          expect(value).to eq :x
          expect([:take, :select]).to include(kind)

          # The loser is still blocked and is served by the next put.
          m.put(:y)
          loser_kind, loser_value = winners.pop
          expect(loser_kind).to eq(kind == :take ? :select : :take)
          expect(loser_value).to eq :y
          taker.join
          selector.join
        end
      end

      it 'is deterministic when a timeout collides with a put' do
        wins = 0
        timeouts = 0

        100.times do
          m = MVar.new
          producer = in_thread { sleep(0.02); m.put(7) }

          result = MVar.select(m, timeout: 0.02)

          if result == MVar::TIMEOUT
            timeouts += 1
            # Timed out selection must not steal the racing value.
            expect(m.take).to eq 7
          else
            wins += 1
            expect(result).to eq [0, 7]
            expect(m).to be_empty
          end

          expect(producer.join(5)).not_to be_nil
        end

        expect(wins).to be > 0
        expect(timeouts).to be > 0
      end

      it 'serves several selections with one value each' do
        m = MVar.new
        selectors = 10.times.map { in_thread { MVar.select(m) } }

        sleep 0.1
        10.times { |i| m.put(i) }

        values = selectors.map(&:value).map(&:last)
        expect(values.sort).to eq (0...10).to_a
        expect(m).to be_empty
      end

      it 'delivers every value exactly once when two producers run concurrently' do
        count = 200
        a = MVar.new
        b = MVar.new
        received = Queue.new

        consumers = 2.times.map do
          in_thread do
            loop do
              result = MVar.select(a, b, timeout: 0.5)
              break if received.length >= 2 * count
              received << result unless result == MVar::TIMEOUT
            end
          end
        end

        producer_a = in_thread { count.times { |i| a.put([:a, i]) } }
        producer_b = in_thread { count.times { |i| b.put([:b, i]) } }

        producer_a.join
        producer_b.join
        consumers.each(&:join)

        values = (2 * count).times.map { received.pop }
        got_a = values.map(&:last).select { |name, _| name == :a }.map(&:last).sort
        got_b = values.map(&:last).select { |name, _| name == :b }.map(&:last).sort
        expect(got_a).to eq (0...count).to_a
        expect(got_b).to eq (0...count).to_a
        expect(a.try_take!).to eq MVar::EMPTY
        expect(b.try_take!).to eq MVar::EMPTY
      end

      it 'applies dereferenceable options to the winning value' do
        value = 'v'
        m = MVar.new(value, dup_on_deref: true)
        _, taken = MVar.select(m)
        expect(taken).to eq 'v'
        expect(taken).not_to be value
      end

    end

    describe '.take_any' do

      it 'is an alias of .select' do
        m = MVar.new(14)
        expect(MVar.take_any(m)).to eq [0, 14]
      end

      it 'supports a timeout' do
        expect(MVar.take_any(MVar.new, timeout: 0.05)).to eq MVar::TIMEOUT
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
