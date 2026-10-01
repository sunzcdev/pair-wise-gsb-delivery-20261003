require 'concurrent/atomic/semaphore'
require 'concurrent/atomic/cyclic_barrier'
require 'concurrent/atomic/atomic_fixnum'

module Concurrent
  RSpec.describe MutexSemaphore, :fair_semaphore do
    describe 'fair mode' do
      it 'is not fair by default to preserve backwards compatibility' do
        expect(described_class.new(1).fair?).to eq false
      end

      it 'accepts the fair flag positionally and reports it through #fair?' do
        expect(described_class.new(1, true).fair?).to eq true
        expect(described_class.new(1, false).fair?).to eq false
      end

      it 'accepts the fair flag as an options hash' do
        expect(described_class.new(1, { fair: true }).fair?).to eq true
        expect(described_class.new(1, { fair: false }).fair?).to eq false
      end

      it 'keeps the original non-fair behavior available' do
        semaphore = described_class.new(3, false)
        expect(semaphore.try_acquire(3)).to eq true
        expect(semaphore.available_permits).to eq 0
      end

      def wait_for_queue(semaphore, size, timeout = 5)
        repeat_until_success(timeout) do
          raise 'queue not full yet' unless queued?(semaphore, size)
        end
      end

      def queued?(semaphore, size)
        semaphore.instance_variable_get(:@waiters).size == size
      end

      describe 'FIFO ordering with different weights' do
        it 'does not let a small later request overtake a heavy earlier request' do
          semaphore = described_class.new(1, true)
          granted = []
          granted_mutex = Mutex.new

          # heavy first: needs all 3 permits while only 1 is available
          heavy = in_thread do
            semaphore.acquire(3)
            granted_mutex.synchronize { granted << :heavy }
          end
          wait_for_queue(semaphore, 1)

          # light second: asks for a single permit, which is currently free
          light = in_thread do
            sleep 0.05
            semaphore.acquire(1)
            granted_mutex.synchronize { granted << :light }
          end
          wait_for_queue(semaphore, 2)

          # enough permits for the heavy request; the remaining permit
          # cascades straight to the queued light request
          semaphore.release(3)

          join_with([heavy, light])
          expect(granted).to eq [:heavy, :light]
          expect(semaphore.available_permits).to eq 0
        end

        it 'serves requests strictly in enqueue order as permits are released' do
          semaphore = described_class.new(0, true)
          order = []
          order_mutex = Mutex.new

          # gate worker creation so the requests enqueue strictly as 2, 3, 1
          gates = Array.new(3) { Concurrent::CyclicBarrier.new(2) }
          workers = [2, 3, 1].each_with_index.map do |weight, idx|
            in_thread(weight) do |w|
              gates[idx].wait
              semaphore.acquire(w)
              order_mutex.synchronize { order << w }
            end
          end
          gates.each_with_index { |gate, idx| gate.wait; wait_for_queue(semaphore, idx + 1) }

          semaphore.release(2) # first request (weight 2) goes through
          repeat_until_success { raise 'first not done yet' unless order.include?(2) }
          expect(order).to eq [2]

          # even though 1 permit would suffice for the queued weight-1
          # request, the weight-3 head must be served first
          semaphore.release(1)
          sleep 0.2
          expect(order).to eq [2]

          semaphore.release(3) # head (3) and its follower (1) cascade
          join_with(workers)
          expect(order).to eq [2, 3, 1]
          expect(semaphore.available_permits).to eq 0
        end
      end

      describe 'all-or-nothing grants' do
        it 'leaves all permits untouched while the head cannot be fully granted' do
          semaphore = described_class.new(2, true)
          t = in_thread { semaphore.acquire(5) }
          wait_for_queue(semaphore, 1)
          sleep 0.1
          expect(semaphore.available_permits).to eq 2
          t.kill
          t.join
          expect(semaphore.available_permits).to eq 2
        end

        it 'grants every permit atomically to the head' do
          semaphore = described_class.new(0, true)
          t = in_thread do
            semaphore.acquire(4)
            expect(semaphore.available_permits).to eq 0
          end
          wait_for_queue(semaphore, 1)
          semaphore.release(4)
          join_with(t)
        end

        it 'does not let drain_permits overtake queued requests' do
          semaphore = described_class.new(3, true)
          t = in_thread { semaphore.acquire(5) }
          wait_for_queue(semaphore, 1)
          expect(semaphore.drain_permits).to eq 0
          expect(semaphore.available_permits).to eq 3
          t.kill
          t.join
          expect(semaphore.drain_permits).to eq 3
          expect(semaphore.available_permits).to eq 0
        end
      end

      describe '#try_acquire' do
        it 'does not bypass an already queued request even with enough permits' do
          semaphore = described_class.new(3, true)
          heavy = in_thread { semaphore.acquire(5) }
          wait_for_queue(semaphore, 1)

          expect(semaphore.try_acquire(1)).to eq false
          expect(semaphore.try_acquire(1, 0.1)).to eq false
          expect(semaphore.available_permits).to eq 3

          heavy.kill
          heavy.join
        end

        it 'respects the queue with a timeout and proceeds when the head gives up' do
          semaphore = described_class.new(0, true)
          head = in_thread { semaphore.try_acquire(5, 0.15) }
          wait_for_queue(semaphore, 1)

          # this waits behind the head, which will time out
          result = nil
          second = in_thread do
            result = semaphore.try_acquire(1, 2)
          end
          wait_for_queue(semaphore, 2)

          semaphore.release(1) # head still cannot take it; second waits for the timeout
          join_with([head, second])
          expect(result).to eq true
          expect(semaphore.available_permits).to eq 0
        end

        it 'succeeds immediately with an empty queue and enough permits' do
          semaphore = described_class.new(2, true)
          expect(semaphore.try_acquire(2)).to eq true
          expect(semaphore.available_permits).to eq 0
        end

        it 'releases permits from a block just like the non-fair version' do
          semaphore = described_class.new(1, true)
          yielded = false
          semaphore.try_acquire(1) { yielded = true }
          expect(yielded).to eq true
          expect(semaphore.available_permits).to eq 1
        end
      end

      describe 'head advancement' do
        it 'does not hand permits to the next request before the head actually times out' do
          semaphore = described_class.new(0, true)

          head = in_thread { semaphore.try_acquire(3, 0.3) }
          wait_for_queue(semaphore, 1)

          got = nil
          second = in_thread do
            semaphore.acquire(1)
            got = true
          end
          wait_for_queue(semaphore, 2)

          semaphore.release(1) # head cannot use it, second must wait for head
          sleep 0.1
          expect(got).to be_nil
          expect(semaphore.available_permits).to eq 1

          join_with([head, second])
          expect(got).to eq true
          expect(semaphore.available_permits).to eq 0
        end

        it 'advances the next request promptly when the head times out (barrier-ordered)' do
          # Both requests start together behind a barrier; the single release
          # is performed while the head is blocked but before its timeout. A
          # completion barrier forces the ordering of their result records:
          # the successor records itself only after observing the head's
          # timeout result.
          semaphore = described_class.new(0, true)
          ordered = []
          ordered_mutex = Mutex.new
          start_barrier = Concurrent::CyclicBarrier.new(3)
          head_done = Concurrent::CyclicBarrier.new(2)
          head_result = nil

          head = in_thread do
            start_barrier.wait
            got = semaphore.try_acquire(3, 1)
            ordered_mutex.synchronize do
              head_result = got
              ordered << [:head, got]
            end
            head_done.wait
          end

          second = in_thread do
            start_barrier.wait
            semaphore.acquire(1)
            head_done.wait
            ordered_mutex.synchronize { ordered << [:second, true] }
          end

          start_barrier.wait
          wait_for_queue(semaphore, 2)
          semaphore.release(1)

          join_with([head, second])
          expect(head_result).to eq false
          expect(ordered).to eq [[:head, false], [:second, true]]
          expect(semaphore.available_permits).to eq 0
        end

        it 'advances the next request when the head thread is killed' do
          semaphore = described_class.new(0, true)
          head = in_thread { semaphore.acquire(10) }
          wait_for_queue(semaphore, 1)

          got = nil
          second = in_thread { semaphore.acquire(1); got = true }
          wait_for_queue(semaphore, 2)

          semaphore.release(1)
          sleep 0.1
          expect(got).to be_nil

          head.kill
          head.join
          join_with(second)
          expect(got).to eq true
        end
      end

      describe 'invariants under contention' do
        it 'never loses permits and never grants a request twice' do
          initial = 10
          semaphore = described_class.new(initial, true)
          threads_count = 12
          rounds = 200

          violations = Concurrent::AtomicFixnum.new(0)
          start = Concurrent::CyclicBarrier.new(threads_count + 2)

          workers = Array.new(threads_count) do |i|
            in_thread do
              start.wait
              rounds.times do
                weight = (i % 4) + 1 # weights 1..4
                semaphore.acquire(weight)
                sleep(rand * 0.002)
                semaphore.release(weight)
              end
            end
          end

          # Continuously assert free + granted == initial, sampling under the
          # same lock that performs every grant and release.
          checker = in_thread do
            start.wait
            5000.times do
              free, granted = semaphore.permit_accounting
              violations.increment unless free + granted == initial
              violations.increment if free.negative? || granted.negative?
            end
          end

          start.wait
          join_with(workers, 20)
          join_with([checker], 5)
          expect(violations.value).to eq 0
          expect(semaphore.available_permits).to eq initial
          expect(semaphore.permit_accounting).to eq [initial, 0]
        end

        it 'fairs out permits in order even with mixed timed try_acquire calls' do
          semaphore = described_class.new(0, true)
          finished = Queue.new

          weights = [2, 1, 3, 1]
          # gate threads so they enqueue strictly in index order
          gates = Array.new(weights.size) { Concurrent::CyclicBarrier.new(2) }

          workers = weights.each_with_index.map do |weight, idx|
            in_thread do
              gates[idx].wait
              result = if idx.even?
                         semaphore.acquire(weight)
                         true
                       else
                         semaphore.try_acquire(weight, 10)
                       end
              finished << [idx, result] if result
            end
          end

          weights.each_with_index do |_, idx|
            gates[idx].wait
            wait_for_queue(semaphore, idx + 1)
          end

          # release permits for one queued request at a time
          weights.each { |w| semaphore.release(w) }
          join_with(workers, 15)

          order = []
          order << finished.pop until finished.empty?
          expect(order.map(&:first)).to eq [0, 1, 2, 3]
          expect(semaphore.available_permits).to eq 0
        end

        it 'does not double grant a waiter when release wins the race against timeout' do
          100.times do
            semaphore = described_class.new(0, true)
            granted = Concurrent::AtomicFixnum.new(0)
            barrier = Concurrent::CyclicBarrier.new(2)

            t = in_thread do
              barrier.wait
              granted.increment if semaphore.try_acquire(1, 0.5)
            end

            barrier.wait
            wait_for_queue(semaphore, 1)
            semaphore.release(1)
            join_with([t], 2)

            expect(granted.value).to eq 1
            expect(semaphore.available_permits).to eq 0
          end
        end
      end
    end
  end
end
