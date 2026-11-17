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

      it 'commits the transformed value in one step' do
        m = MVar.new(14)
        m.modify { |v| MVar::EMPTY }
        expect(m).to be_empty
      end

      it 'signals empty waiters when the block empties the cell' do
        m    = MVar.new(14)
        done = Concurrent::CountDownLatch.new

        in_thread do
          m.modify { |v| MVar::EMPTY }
          done.count_down
        end

        expect(m.put(28, 1)).to eq 28
        expect(done.wait(1)).to be_truthy
      end

      context 'when the block raises' do

        let(:value) { { 'timeout' => 5 } }
        let(:m) { MVar.new(value) }

        it 'propagates the exception' do
          expect { m.modify { raise 'boom!' } }.to raise_error('boom!')
        end

        it 'does not leave the MVar empty' do
          expect { m.modify { raise 'boom!' } }.to raise_error('boom!')
          expect(m).to be_full
          expect(m.take).to eq value
        end
        it 'keeps the very same reference' do
          original = m.value
          expect { m.modify { raise 'boom!' } }.to raise_error('boom!')
          expect(m.value.object_id).to eq original.object_id
        end

        it 'does not publish a copy or a half built value' do
          expect { m.modify { |v| v.merge('extra' => 1); raise 'boom!' } }.to raise_error('boom!')
          expect(m.value).to eq('timeout' => 5)
          expect(m.value.object_id).to eq value.object_id
        end

        it 'does not signal empty waiters' do
          waiter = in_thread { m.put(28, 0.3) }
          sleep(0.05)

          expect { m.modify { raise 'boom!' } }.to raise_error('boom!')

          expect(waiter.join(2)).not_to be_nil
          expect(waiter.value).to eq MVar::TIMEOUT
          expect(m).to be_full
          expect(m.take).to eq value
        end

        it 'leaves the MVar usable for later updates' do
          expect { m.modify { raise 'boom!' } }.to raise_error('boom!')
          expect(m.modify { |v| v.merge('timeout' => 9) }).to eq value
          expect(m.take).to eq('timeout' => 9)
        end

        it 'restores the reference when the exception is not a StandardError' do
          expect { m.modify { raise NotImplementedError, 'nope' } }.to raise_error(NotImplementedError)
          expect(m).to be_full
          expect(m.value.object_id).to eq value.object_id
        end
      end

      context 'readers during the update' do

        it 'never observes an empty or intermediate cell while the block runs' do
          m       = MVar.new(14)
          states  = []
          started = Concurrent::CountDownLatch.new
          proceed = Concurrent::CountDownLatch.new

          modifier = in_thread do
            m.modify do |v|
              started.count_down
              proceed.wait(2)
              v + 1
            end
          end

          reader = in_thread do
            started.wait(2)
            # This read cannot complete while the block runs; the cell is only
            # observable as the state before or after the single commit.
            states << [m.empty?, m.value]
          end

          sleep(0.1)
          expect(reader).to be_alive # blocked, not served a half-built value
          proceed.count_down

          expect(modifier.join(2)).not_to be_nil
          expect(reader.join(2)).not_to be_nil
          expect(states).to eq [[false, 15]]
          expect(m.value).to eq 15
        end

        it 'does not apply the timeout to the block execution' do
          m = MVar.new(14)
          expect(m.modify(0.05) { |v| sleep(0.2); v + 1 }).to eq 14
          expect(m.take).to eq 15
        end

        it 'wakes a waiter blocked on empty only after the commit' do
          m       = MVar.new
          waiting = Concurrent::CountDownLatch.new
          result  = nil

          waiter = in_thread do
            waiting.count_down
            result = m.take
          end

          waiting.wait(2)
          elapsed = monotonic_interval do
            m.modify! do
              sleep(0.2)
              14
            end
          end

          expect(waiter.join(2)).not_to be_nil
          expect(result).to eq 14
          expect(elapsed).to be >= 0.15
          expect(m).to be_empty
        end
      end

    end

    describe '#update' do

      it 'raises an exception when no block given' do
        m = MVar.new(14)
        expect { m.update }.to raise_error(ArgumentError)
      end

      it 'returns a committed result' do
        m      = MVar.new(14)
        result = m.update { |v| v + 2 }

        expect(result).to be_updated
        expect(result).to be_success
        expect(result.status).to eq :updated
        expect(result.previous_value).to eq 14
        expect(result.value).to eq 16
        expect(result.current_value).to eq 16
        expect(result.error).to be_nil
        expect(m.take).to eq 16
      end

      it 'reports a failed update without raising' do
        m      = MVar.new(14)
        error  = RuntimeError.new('boom!')
        result = nil

        expect { result = m.update { raise error } }.not_to raise_error

        expect(result).to be_failed
        expect(result).not_to be_updated
        expect(result.status).to eq :failed
        expect(result.error).to eq error
        expect(result.previous_value).to eq 14
        expect(result.value).to be_nil
        expect(result.current_value).to eq 14
      end

      it 'keeps the original reference on failure' do
        value  = { 'timeout' => 5 }
        m      = MVar.new(value)
        result = m.update { |v| v['timeout'] = 9; raise 'boom!' }

        expect(result).to be_failed
        expect(m).to be_full
        expect(m.value.object_id).to eq value.object_id
      end

      it 'reports a timeout before anything is committed' do
        m      = MVar.new
        called = false

        result = m.update(0.1) { |v| called = true; v }

        expect(result).to be_timed_out
        expect(result.status).to eq :timeout
        expect(result.previous_value).to be_nil
        expect(result.current_value).to be_nil
        expect(called).to be(false)
        expect(m).to be_empty
      end

      it 'waits for another thread to #put' do
        m = MVar.new

        in_thread {
          sleep(0.1)
          m.put 14
        }

        result = m.update { |v| v + 2 }
        expect(result).to be_updated
        expect(result.previous_value).to eq 14
        expect(m.take).to eq 16
      end

      it 'distinguishes a timeout from an empty cell and from a value' do
        m = MVar.new

        expect(m.update(0.1) { |v| v }).not_to eq MVar::TIMEOUT
        expect(m.update(0.1) { |v| v }).not_to eq MVar::EMPTY
        expect(m.try_update { |v| v }).not_to eq MVar::EMPTY

        m.put(14)
        expect(m.update { |v| v }).not_to eq 14
        expect(m.update { |v| v }).to be_updated
      end
    end

    describe '#try_update' do

      it 'raises an exception when no block given' do
        m = MVar.new(14)
        expect { m.try_update }.to raise_error(ArgumentError)
      end

      it 'returns a committed result' do
        m      = MVar.new(14)
        result = m.try_update { |v| v + 2 }

        expect(result).to be_updated
        expect(result.previous_value).to eq 14
        expect(result.value).to eq 16
        expect(m.take).to eq 16
      end

      it 'reports an empty cell without running the block' do
        m      = MVar.new
        called = false

        result = m.try_update { |v| called = true; v }

        expect(result).to be_empty_cell
        expect(result).to be_missed
        expect(result.status).to eq :empty
        expect(result.current_value).to be_nil
        expect(called).to be(false)
        expect(m).to be_empty
      end

      it 'reports a failed update without raising' do
        m      = MVar.new(14)
        result = m.try_update { raise 'boom!' }

        expect(result).to be_failed
        expect(result.error).to be_a(RuntimeError)
        expect(m.take).to eq 14
      end

      it 'keeps the original reference on failure' do
        value  = { 'timeout' => 5 }
        m      = MVar.new(value)
        result = m.try_update { |v| v['timeout'] = 9; raise 'boom!' }

        expect(result).to be_failed
        expect(m.value.object_id).to eq value.object_id
      end
    end

    describe 're-entrant access' do

      let(:m) { MVar.new(14) }

      it 'rejects a nested #modify from its own block' do
        expect { m.modify { m.modify { |v| v } } }
          .to raise_error(Concurrent::ReentrantOperationError, /re-entrant/)
      end

      it 'rejects a nested #update from its own block' do
        expect { m.update { m.update { |v| v } } }
          .to raise_error(Concurrent::ReentrantOperationError)
      end

      it 'rejects a nested #try_update from its own block' do
        expect { m.try_update { m.try_update { |v| v } } }
          .to raise_error(Concurrent::ReentrantOperationError)
      end

      it 'rejects a nested #modify! from its own block' do
        expect { m.modify! { m.modify! { |v| v } } }
          .to raise_error(Concurrent::ReentrantOperationError)
      end

      it 'rejects reads from the block' do
        expect { m.modify { m.value } }.to raise_error(Concurrent::ReentrantOperationError)
        expect { m.modify { m.empty? } }.to raise_error(Concurrent::ReentrantOperationError)
        expect { m.modify { m.full? } }.to raise_error(Concurrent::ReentrantOperationError)
      end

      it 'rejects other blocking operations from the block' do
        expect { m.modify { m.take } }.to raise_error(Concurrent::ReentrantOperationError)
        expect { m.modify { m.put(1) } }.to raise_error(Concurrent::ReentrantOperationError)
        expect { m.modify { m.try_take! } }.to raise_error(Concurrent::ReentrantOperationError)
        expect { m.modify { m.try_put!(1) } }.to raise_error(Concurrent::ReentrantOperationError)
        expect { m.modify { m.set!(1) } }.to raise_error(Concurrent::ReentrantOperationError)
      end

      it 'rejects nested access from a #borrow block' do
        expect { m.borrow { m.modify { |v| v } } }
          .to raise_error(Concurrent::ReentrantOperationError)
      end

      it 'raises instead of blocking' do
        started = Concurrent::CountDownLatch.new
        elapsed = monotonic_interval do
          expect { m.modify { |v| started.count_down; m.modify { |x| x } } }
            .to raise_error(Concurrent::ReentrantOperationError)
        end

        expect(started.count).to eq 0
        expect(elapsed).to be < 1.0
      end

      it 'keeps the original reference after the rejection' do
        expect { m.modify { m.modify { |v| v } } }
          .to raise_error(Concurrent::ReentrantOperationError)
        expect(m.value).to eq 14
      end

      it 'releases the ownership after the block' do
        m.modify { |v| v + 1 }
        expect(m.value).to eq 15
        expect(m.take).to eq 15
      end

      it 'releases the ownership after a rejected nested access' do
        expect { m.modify { m.value } }.to raise_error(Concurrent::ReentrantOperationError)
        expect(m.modify { |v| v + 1 }).to eq 14
        expect(m.value).to eq 15
      end

      it 'allows using another MVar from the block' do
        other = MVar.new(1)

        expect(m.modify { |v| v + other.take }).to eq 14
        expect(m.take).to eq 15
        expect(other).to be_empty
      end

      it 'does not treat a foreign thread as the owner' do
        done   = Concurrent::CountDownLatch.new
        result = nil

        in_thread do
          result = m.modify { |v| v + 1 }
          done.count_down
        end

        expect(done.wait(2)).to be_truthy
        expect(result).to eq 14
      end

      it 'rejects nested access performed through dereference callbacks' do
        m = MVar.new(14, copy_on_deref: proc { |v| m.empty? })

        expect { m.value }.to raise_error(Concurrent::ReentrantOperationError)
      end

      it 'reports the rejection as an IllegalOperationError' do
        expect { m.modify { m.value } }.to raise_error(Concurrent::IllegalOperationError)
      end

      it 'is not reported as a failed update result' do
        expect { m.try_update { m.value } }
          .to raise_error(Concurrent::ReentrantOperationError)
      end

      it 'does not mistake a user raised error for a rejection' do
        result = nil
        expect { result = m.update { raise Concurrent::ReentrantOperationError, 'mine' } }
          .not_to raise_error

        expect(result).to be_failed
        expect(result.error.message).to eq 'mine'
        expect(m.value).to eq 14
      end
    end

    describe 'result distinctions' do

      let(:m) { MVar.new }

      it 'uses distinct sentinels for empty, timeout and try results' do
        expect(MVar::EMPTY).not_to eq MVar::TIMEOUT
        expect(m.value).to eq MVar::EMPTY
        expect(m.take(0.1)).to eq MVar::TIMEOUT
        expect(m.try_take!).to eq MVar::EMPTY
      end

      it 'returns booleans from #try_put!' do
        expect(m.try_put!(14)).to be(true)
        expect(m.try_put!(28)).to be(false)
      end

      it 'reports a pending timeout only while the cell is not committed' do
        m.put(14)
        expect(m.put(28, 0.1)).to eq MVar::TIMEOUT
        expect(m.take).to eq 14
      end

      it 'rejects a timeout which is not a number' do
        expect { m.take(:soon) }.to raise_error(ArgumentError)
      end

      it 'treats a non-positive timeout as an immediate timeout' do
        expect(m.take(0)).to eq MVar::TIMEOUT
        expect(m.take(-1)).to eq MVar::TIMEOUT
        # No waiting is needed when the cell is already readable.
        expect(MVar.new(14).take(-1)).to eq 14
        expect(MVar.new.put(14, -1)).to eq 14
      end
    end

    describe '#modify!' do

      it 'propagates an exception from the block' do
        expect { MVar.new(14).modify! { raise 'boom!' } }.to raise_error('boom!')
      end

      it 'keeps the original reference when the block raises' do
        value = { 'timeout' => 5 }
        m     = MVar.new(value)

        expect { m.modify! { |v| v.merge('retries' => 1); raise 'boom!' } }.to raise_error('boom!')

        expect(m).to be_full
        expect(m.value.object_id).to eq value.object_id
        expect(m.take).to eq('timeout' => 5)
      end

      it 'does not undo mutations the block made to the stored object' do
        value = { 'timeout' => 5 }
        m     = MVar.new(value)

        expect { m.modify! { |v| v['timeout'] = 9; raise 'boom!' } }.to raise_error('boom!')

        # The reference is restored, the object itself is not rolled back.
        expect(m.value.object_id).to eq value.object_id
        expect(m.value['timeout']).to eq 9
      end

      it 'keeps an empty cell empty when the block raises' do
        m = MVar.new
        expect { m.modify! { raise 'boom!' } }.to raise_error('boom!')
        expect(m).to be_empty
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
