require 'concurrent/atomic/semaphore'

RSpec.shared_examples :semaphore do
  let(:semaphore) { described_class.new(3) }

  # Waits until the block is truthy, polling without a fixed sleep.
  #
  # @raise [RuntimeError] when the condition does not become true in time
  def wait_until(timeout = 5)
    deadline = Concurrent.monotonic_time + timeout
    until yield
      raise 'condition not met in time' if Concurrent.monotonic_time > deadline
      Thread.pass
    end
  end

  describe '#initialize' do
    it 'raises an exception if the initial count is not an integer' do
      expect {
        described_class.new('foo')
      }.to raise_error(ArgumentError)
    end

    it 'defaults to the FIFO queue mode without a bypass budget' do
      expect(semaphore.queue_mode).to eq :fifo
      expect(semaphore.bypass_limit).to eq 0
    end

    it 'accepts the bounded bypass policy' do
      semaphore = described_class.new(3, queue_mode: :bounded_bypass, bypass_limit: 2)
      expect(semaphore.queue_mode).to eq :bounded_bypass
      expect(semaphore.bypass_limit).to eq 2
    end

    it 'accepts the options hash as a positional argument' do
      semaphore = described_class.new(3, {queue_mode: :bounded_bypass, bypass_limit: 3})
      expect(semaphore.queue_mode).to eq :bounded_bypass
      expect(semaphore.bypass_limit).to eq 3
    end

    it 'ignores the bypass budget in FIFO mode' do
      semaphore = described_class.new(3, queue_mode: :fifo, bypass_limit: 5)
      expect(semaphore.bypass_limit).to eq 0
    end

    it 'raises an exception for an unknown queue mode' do
      expect {
        described_class.new(3, queue_mode: :lifo)
      }.to raise_error(ArgumentError)
    end

    it 'raises an exception for an unknown option' do
      expect {
        described_class.new(3, fairness: :strict)
      }.to raise_error(ArgumentError)
    end

    it 'raises an exception for a negative bypass limit' do
      expect {
        described_class.new(3, queue_mode: :bounded_bypass, bypass_limit: -1)
      }.to raise_error(ArgumentError)
    end

    context 'when initializing with 0' do
      let(:semaphore) { described_class.new(0) }

      it do
        expect(semaphore).to_not be nil
      end
    end

    context 'when initializing with -1' do
      let(:semaphore) { described_class.new(-1) }

      it do
        semaphore.release
        expect(semaphore.available_permits).to eq 0
      end
    end
  end

  describe '#queue_mode and #bypass_limit' do
    it 'reports the configured policy' do
      semaphore = described_class.new(1, queue_mode: :bounded_bypass, bypass_limit: 4)
      expect(semaphore.queue_mode).to eq :bounded_bypass
      expect(semaphore.bypass_limit).to eq 4
    end
  end

  describe 'queue accounting' do
    it 'reports no waiting requests on an untouched semaphore' do
      expect(semaphore.waiting_count).to eq 0
      expect(semaphore.queue_length).to eq 0
      expect(semaphore.queued_permits).to eq 0
      expect(semaphore.reserved_permits).to eq 0
    end

    it 'tracks queued requests and their total permit demand' do
      semaphore = described_class.new(0)
      waiter_a = in_thread { semaphore.acquire(2) }
      waiter_b = in_thread { semaphore.acquire(3) }
      wait_until { semaphore.waiting_count == 2 }

      expect(semaphore.waiting_count).to eq 2
      expect(semaphore.queued_permits).to eq 5
      expect(semaphore.available_permits).to eq 0

      semaphore.release(5)
      join_with([waiter_a, waiter_b])
      expect(semaphore.waiting_count).to eq 0
    end
  end

  describe 'separating available from reserved permits' do
    it 'excludes permits earmarked for the head of the queue' do
      semaphore = described_class.new(0)
      waiter = in_thread { semaphore.acquire(3) }
      wait_until { semaphore.waiting_count == 1 }

      expect(semaphore.available_permits).to eq 0
      expect(semaphore.reserved_permits).to eq 0

      semaphore.release(2)
      expect(semaphore.available_permits).to eq 0
      expect(semaphore.reserved_permits).to eq 2
      expect(semaphore.waiting_count).to eq 1

      semaphore.release(1)
      join_with([waiter])
      expect(semaphore.available_permits).to eq 0
      expect(semaphore.reserved_permits).to eq 0
      expect(semaphore.waiting_count).to eq 0
    end

    it 'drains only permits that are not reserved' do
      semaphore = described_class.new(0)
      waiter = in_thread { semaphore.acquire(3) }
      wait_until { semaphore.waiting_count == 1 }

      semaphore.release(2)
      expect(semaphore.drain_permits).to eq 0
      expect(semaphore.available_permits).to eq 0
      expect(semaphore.reserved_permits).to eq 2

      semaphore.release(1)
      join_with([waiter])
      semaphore.release(2)
      expect(semaphore.drain_permits).to eq 2
      expect(semaphore.available_permits).to eq 0
    end

    it 'partitions the free pool into available plus reserved' do
      semaphore = described_class.new(0)
      waiter = in_thread { semaphore.acquire(4) }
      wait_until { semaphore.waiting_count == 1 }

      released = 0
      [1, 1, 1].each do |chunk|
        semaphore.release(chunk)
        released += chunk
        expect(semaphore.reserved_permits).to eq released
        expect(semaphore.available_permits).to eq 0
        expect(semaphore.available_permits + semaphore.reserved_permits).to eq released
      end

      semaphore.release(1)
      join_with([waiter])

      expect(semaphore.available_permits).to eq 0
      expect(semaphore.reserved_permits).to eq 0
    end
  end

  describe 'queue admission order' do
    it 'admits waiting requests in FIFO order' do
      semaphore = described_class.new(0)
      order = Queue.new

      first = in_thread do
        semaphore.acquire(2)
        order << :first
      end
      wait_until { semaphore.waiting_count == 1 }

      second = in_thread do
        semaphore.acquire(1)
        order << :second
      end
      wait_until { semaphore.waiting_count == 2 }

      semaphore.release(2)
      join_with([first])
      expect(order.size).to eq 1

      semaphore.release(1)
      join_with([second])
      expect(order.size).to eq 2
      expect(order.pop).to eq :first
      expect(order.pop).to eq :second
    end

    it 'grants a multi-permit request as one batch, never partially' do
      semaphore = described_class.new(0)
      waiter = in_thread { semaphore.acquire(3) }
      wait_until { semaphore.waiting_count == 1 }

      semaphore.release(2)
      sleep(0.05)
      expect(semaphore.waiting_count).to eq 1
      expect(semaphore.reserved_permits).to eq 2

      semaphore.release(1)
      join_with([waiter])
      expect(semaphore.waiting_count).to eq 0
      expect(semaphore.available_permits).to eq 0
    end
  end

  describe 'bounded bypass admission' do
    it 'does not let a smaller request overtake in FIFO mode' do
      semaphore = described_class.new(0)
      order = Queue.new

      big = in_thread do
        semaphore.acquire(2)
        order << :big
      end
      wait_until { semaphore.waiting_count == 1 }

      small = in_thread do
        semaphore.acquire(1)
        order << :small
      end
      wait_until { semaphore.waiting_count == 2 }

      semaphore.release(1)
      sleep(0.05)
      expect(order.size).to eq 0

      semaphore.release(1)
      join_with([big])
      semaphore.release(1)
      join_with([small])

      expect(order.pop).to eq :big
      expect(order.pop).to eq :small
    end

    it 'lets a smaller request overtake the head up to the bypass limit' do
      semaphore = described_class.new(0, queue_mode: :bounded_bypass, bypass_limit: 1)
      order = Queue.new

      big = in_thread do
        semaphore.acquire(3)
        order << :big
      end
      wait_until { semaphore.waiting_count == 1 }

      small = in_thread do
        semaphore.acquire(1)
        order << :small
      end
      wait_until { semaphore.waiting_count == 2 }

      semaphore.release(1)
      join_with([small])
      expect(order.pop).to eq :small
      expect(semaphore.waiting_count).to eq 1

      semaphore.release(3)
      join_with([big])
      expect(order.pop).to eq :big
    end

    it 'stops bypassing once the budget for the head is exhausted' do
      semaphore = described_class.new(0, queue_mode: :bounded_bypass, bypass_limit: 1)
      order = Queue.new

      big = in_thread do
        semaphore.acquire(3)
        order << :big
      end
      wait_until { semaphore.waiting_count == 1 }

      first_small = in_thread do
        semaphore.acquire(1)
        order << :first_small
      end
      wait_until { semaphore.waiting_count == 2 }

      second_small = in_thread do
        semaphore.acquire(1)
        order << :second_small
      end
      wait_until { semaphore.waiting_count == 3 }

      semaphore.release(1)
      join_with([first_small])
      expect(order.pop).to eq :first_small

      semaphore.release(1)
      sleep(0.05)
      expect(order.size).to eq 0

      # The budget is spent, so the head has to be satisfied before the next
      # request in line can run.
      semaphore.release(2)
      join_with([big])
      semaphore.release(1)
      join_with([second_small])

      expect(order.pop).to eq :big
      expect(order.pop).to eq :second_small
    end

    it 'refuses to bypass with an equal or larger request' do
      semaphore = described_class.new(0, queue_mode: :bounded_bypass, bypass_limit: 5)
      order = Queue.new

      first = in_thread do
        semaphore.acquire(2)
        order << :first
      end
      wait_until { semaphore.waiting_count == 1 }

      equal = in_thread do
        semaphore.acquire(2)
        order << :equal
      end
      wait_until { semaphore.waiting_count == 2 }

      semaphore.release(2)
      join_with([first])
      expect(order.pop).to eq :first

      semaphore.release(2)
      join_with([equal])
      expect(order.pop).to eq :equal
    end
  end

  describe 'waiter timeout' do
    it 'removes the timed-out request from the queue' do
      semaphore = described_class.new(0)

      result = semaphore.try_acquire(1, 0.05)
      expect(result).to be_falsey
      expect(semaphore.waiting_count).to eq 0
      expect(semaphore.queued_permits).to eq 0
      expect(semaphore.available_permits).to eq 0
      expect(semaphore.reserved_permits).to eq 0
    end

    it 'treats a zero timeout as an immediate attempt only' do
      semaphore = described_class.new(1)

      expect(semaphore.try_acquire(1, 0)).to be true
      expect(semaphore.waiting_count).to eq 0
      expect(semaphore.try_acquire(1, 0)).to be false
      expect(semaphore.waiting_count).to eq 0
      expect(semaphore.available_permits).to eq 0
    end

    it 'returns the permits reserved for a waiter that times out' do
      semaphore = described_class.new(0)

      # The head accumulates a partial reservation and then gives up, so those
      # permits must go back to the free pool instead of being lost with it.
      result = nil
      waiter = in_thread do
        result = semaphore.try_acquire(3, 0.1)
      end
      wait_until { semaphore.waiting_count == 1 }

      semaphore.release(2)
      expect(semaphore.reserved_permits).to eq 2

      join_with([waiter])
      expect(result).to be_falsey
      expect(semaphore.waiting_count).to eq 0
      expect(semaphore.reserved_permits).to eq 0
      expect(semaphore.available_permits).to eq 2
    end

    it 'leaves other queued requests intact after a timeout' do
      semaphore = described_class.new(0)
      order = Queue.new

      head = in_thread { semaphore.acquire(2); order << :head }
      wait_until { semaphore.waiting_count == 1 }

      # The head needs 2 permits; a second request times out while 1 is free,
      # so its departure must not disturb the head's position or reservation.
      semaphore.release(1)
      expect(semaphore.try_acquire(1, 0.05)).to be_falsey
      expect(semaphore.waiting_count).to eq 1
      expect(semaphore.reserved_permits).to eq 1

      semaphore.release(1)
      join_with([head])
      expect(order.pop).to eq :head
      expect(semaphore.waiting_count).to eq 0
    end
  end

  describe 'block release accounting' do
    it 'releases only the permits actually granted when the block raises' do
      semaphore = described_class.new(2)
      expect {
        expect {
          semaphore.acquire(2) { raise 'boom' }
        }.to raise_error('boom')
      }.not_to change { semaphore.available_permits }
    end

    it 'does not release anything for a zero-permit request' do
      semaphore = described_class.new(2)
      expect {
        semaphore.acquire(0) { raise 'boom' }
      }.to raise_error('boom')
      expect(semaphore.available_permits).to eq 2
      expect(semaphore.reserved_permits).to eq 0
    end

    it 'does not release anything for a zero-permit try_acquire' do
      semaphore = described_class.new(2)
      expect {
        semaphore.try_acquire(0) { raise 'boom' }
      }.to raise_error('boom')
      expect(semaphore.available_permits).to eq 2
      expect(semaphore.reserved_permits).to eq 0
    end
  end

  describe 'zero permits and illegal counts' do
    it 'admits a zero-permit request immediately without changing the count' do
      expect(semaphore.acquire(0)).to be_nil
      expect(semaphore.available_permits).to eq 3
    end

    it 'returns the block value for a zero-permit request' do
      expect(semaphore.acquire(0) { :done }).to eq :done
      expect(semaphore.available_permits).to eq 3
    end

    it 'returns true for a zero-permit try_acquire' do
      expect(semaphore.try_acquire(0)).to be true
    end

    it 'raises for negative permits' do
      expect { semaphore.acquire(-1) }.to raise_error(ArgumentError)
      expect { semaphore.try_acquire(-1) }.to raise_error(ArgumentError)
      expect { semaphore.release(-1) }.to raise_error(ArgumentError)
    end

    it 'raises for non-integer permits' do
      expect { semaphore.acquire('1') }.to raise_error(ArgumentError)
      expect { semaphore.release('1') }.to raise_error(ArgumentError)
    end

    it 'rejects a non-numeric timeout' do
      expect { semaphore.try_acquire(1, 'soon') }.to raise_error(ArgumentError)
    end
  end

  describe 'counter safety under concurrent release' do
    it 'never drives the count negative through concurrent releases' do
      semaphore = described_class.new(0)
      threads = 8.times.map { in_thread { 50.times { semaphore.release } } }
      join_with(threads)
      expect(semaphore.available_permits).to eq 400
    end
  end


  describe '#acquire' do
    context 'without block' do
      context 'permits available' do
        it 'should return nil immediately' do
          result = semaphore.acquire
          expect(result).to be_nil
        end
      end

      context 'not enough permits available' do
        it 'should block thread until permits are available' do
          semaphore.drain_permits
          in_thread { sleep(0.2); semaphore.release }

          result = semaphore.acquire
          expect(result).to be_nil
          expect(semaphore.available_permits).to eq 0
        end
      end

      context 'when acquiring negative permits' do
        it 'raises ArgumentError' do
          expect {
            semaphore.acquire(-1)
          }.to raise_error(ArgumentError)
        end
      end
    end

    context 'with block' do
      context 'permits available' do
        it 'should acquire permits, run the block, release permits, and return block return value' do
          available_permits = semaphore.available_permits
          yielded = false
          expected_result = Object.new

          actual_result = semaphore.acquire do
            expect(semaphore.available_permits).to eq(available_permits - 1)
            yielded = true
            expected_result
          end

          expect(semaphore.available_permits).to eq(available_permits)
          expect(yielded).to be true
          expect(actual_result).to be(expected_result)
        end

        it 'if the block raises, the permit is still released' do
          expect {
            expect {
              semaphore.acquire do
                raise 'boom'
              end
            }.to raise_error('boom')
          }.to_not change { semaphore.available_permits }
        end
      end

      context 'not enough permits available' do
        it 'should block thread until permits are available' do
          yielded = false
          semaphore.drain_permits
          in_thread { sleep(0.2); semaphore.release }
          expected_result = Object.new

          actual_result = semaphore.acquire do
            yielded = true
            expected_result
          end

          expect(actual_result).to be(expected_result)
          expect(yielded).to be true
          expect(semaphore.available_permits).to eq 1
        end
      end

      context 'when acquiring negative permits' do
        it 'raises ArgumentError' do
          expect {
            expect {
              semaphore.acquire(-1) do
                raise 'block should never run'
              end
            }.to raise_error(ArgumentError)
          }.not_to change { semaphore.available_permits }
        end
      end
    end
  end

  describe '#drain_permits' do
    it 'drains all available permits' do
      drained = semaphore.drain_permits
      expect(drained).to eq 3
      expect(semaphore.available_permits).to eq 0
    end

    it 'drains nothing in no permits are available' do
      semaphore.reduce_permits 3
      drained = semaphore.drain_permits
      expect(drained).to eq 0
    end
  end

  describe '#try_acquire' do
    context 'without block' do
      context 'without timeout' do
        it 'acquires immediately if permits are available' do
          result = semaphore.try_acquire(1)
          expect(result).to be_truthy
        end

        it 'returns false immediately in no permits are available' do
          result = semaphore.try_acquire(20)
          expect(result).to be_falsey
        end

        context 'when trying to acquire negative permits' do
          it do
            expect {
              semaphore.try_acquire(-1)
            }.to raise_error(ArgumentError)
          end
        end
      end

      context 'with timeout' do
        it 'acquires immediately if permits are available' do
          result = semaphore.try_acquire(1, 5)
          expect(result).to be_truthy
        end

        it 'acquires when permits are available within timeout' do
          semaphore.drain_permits
          in_thread { sleep 0.1; semaphore.release }
          result = semaphore.try_acquire(1, 1)
          expect(result).to be_truthy
        end

        it 'returns false on timeout' do
          semaphore.drain_permits
          result = semaphore.try_acquire(1, 0.1)
          expect(result).to be_falsey
        end
      end
    end

    context 'with block' do
      context 'without timeout' do
        it 'acquires immediately if permits are available and returns block return value' do
          yielded = false
          available_permits = semaphore.available_permits
          expected_result = Object.new

          actual_result = semaphore.try_acquire(1) do
            yielded = true
            expect(semaphore.available_permits).to eq(available_permits - 1)
            expected_result
          end

          expect(actual_result).to be(expected_result)
          expect(yielded).to be true
          expect(semaphore.available_permits).to eq available_permits
        end

        it 'releases permit if block raises' do
          expect {
            expect {
              semaphore.try_acquire(1) do
                raise 'boom'
              end
            }.to raise_error('boom')
          }.not_to change { semaphore.available_permits }
        end

        it 'returns false immediately in no permits are available' do
          expect {
            result = semaphore.try_acquire(20) do
              raise 'block should never run'
            end

            expect(result).to be_falsey
          }.not_to change { semaphore.available_permits }
        end

        context 'when trying to acquire negative permits' do
          it do
            expect {
              expect {
                semaphore.try_acquire(-1) do
                  raise 'block should never run'
                end
              }.to raise_error(ArgumentError)
            }.not_to change { semaphore.available_permits }
          end
        end
      end

      context 'with timeout' do
        it 'acquires immediately if permits are available, and returns block return value' do
          expect {
            yielded = false
            expected_result = Object.new

            actual_result = semaphore.try_acquire(1, 5) do
              yielded = true
              expected_result
            end

            expect(actual_result).to be(expected_result)
            expect(yielded).to be true
          }.not_to change { semaphore.available_permits }
        end

        it 'releases permits if block raises' do
          expect {
            expect {
              semaphore.try_acquire(1, 5) do
                raise 'boom'
              end
            }.to raise_error('boom')
          }.not_to change { semaphore.available_permits }
        end

        it 'acquires when permits are available within timeout, and returns block return value' do
          yielded = false
          semaphore.drain_permits
          in_thread { sleep 0.1; semaphore.release }
          expected_result = Object.new

          actual_result = semaphore.try_acquire(1, 1) do
            yielded = true
            expected_result
          end

          expect(actual_result).to be(expected_result)
          expect(yielded).to be true
          expect(semaphore.available_permits).to be 1
        end

        it 'returns false on timeout' do
          semaphore.drain_permits

          result = semaphore.try_acquire(1, 0.1) do
            raise 'block should never run'
          end

          expect(result).to be_falsey
          expect(semaphore.available_permits).to be 0
        end
      end
    end
  end

  describe '#reduce_permits' do
    it 'raises ArgumentError if reducing by negative number' do
      expect {
        semaphore.reduce_permits(-1)
      }.to raise_error(ArgumentError)
    end

    it 'reduces permits below zero' do
      semaphore.reduce_permits 1003
      expect(semaphore.available_permits).to eq(-1000)
    end

    it 'reduces permits' do
      semaphore.reduce_permits 1
      expect(semaphore.available_permits).to eq 2
      semaphore.reduce_permits 2
      expect(semaphore.available_permits).to eq 0
    end

    it 'reduces zero permits' do
      semaphore.reduce_permits 0
      expect(semaphore.available_permits).to eq 3
    end
  end

  describe '#release' do
    it 'increases the number of available permits by one' do
      semaphore.release
      expect(semaphore.available_permits).to eq 4
    end

    context 'when a number of permits is specified' do
      it 'increases the number of available permits by the specified value' do
        semaphore.release(2)
        expect(semaphore.available_permits).to eq 5
      end

      context 'when permits is set to negative number' do
        it do
          expect {
            semaphore.release(-1)
          }.to raise_error(ArgumentError)
        end
      end
    end
  end
end

module Concurrent
  RSpec.describe MutexSemaphore do
    it_should_behave_like :semaphore
  end

  RSpec.describe Semaphore do
    it 'inherits from MutexSemaphore on every engine' do
      expect(Semaphore.ancestors).to include(MutexSemaphore)
    end

    it 'exposes the queueing API' do
      expect(Semaphore.instance_method(:waiting_count)).to be_a UnboundMethod
      expect(Semaphore.instance_method(:reserved_permits)).to be_a UnboundMethod
      expect(Semaphore.instance_method(:queued_permits)).to be_a UnboundMethod
      expect(Semaphore.instance_method(:queue_mode)).to be_a UnboundMethod
      expect(Semaphore.instance_method(:bypass_limit)).to be_a UnboundMethod
    end
  end
end
