require 'concurrent/atomic/semaphore'
require 'concurrent/atomic/cyclic_barrier'
require 'concurrent/atomic/atomic_fixnum'
require 'concurrent/atomic/atomic_boolean'

# Shared behaviour for semaphores that support an explicit fair (FIFO) mode.
# Included both for MutexSemaphore and the public Semaphore facade.
RSpec.shared_examples :fair_semaphore do
  def new_fair(count)
    described_class.new(count, true)
  end
  # Blocks until the semaphore's internal fair queue holds exactly `n`
  # requests, so tests can establish a deterministic enqueue order.
  def wait_for_queue(semaphore, n, timeout = 2)
    deadline = Concurrent.monotonic_time + timeout
    loop do
      size = semaphore.instance_variable_get(:@queue).size
      break if size == n
      raise "queue never reached #{n} (was #{size})" if Concurrent.monotonic_time > deadline
      sleep 0.002
    end
  end

  describe '#initialize fair mode' do
    it 'is not fair by default' do
      expect(described_class.new(3).fair?).to be false
    end

    it 'is fair when requested' do
      expect(new_fair(3).fair?).to be true
    end
  end

  describe 'fair, weighted FIFO granting' do
    it 'grants whole requests strictly in enqueue order' do
      semaphore = new_fair(2)
      order     = Queue.new

      # Notifies only once a request is parked in its fair wait, proving it
      # is already in the FIFO queue. Lets us establish enqueue order without
      # relying on thread scheduling.
      parked = Queue.new
      inner  = semaphore.method(:ns_wait)
      semaphore.define_singleton_method(:ns_wait) do |timeout|
        parked << :parked
        inner.call(timeout)
      end

      # head needs 3, only 2 free -> waits
      heavy = Thread.new do
        semaphore.acquire(3)
        order << :heavy
        semaphore.release(3)
      end
      parked.pop

      # a later 1-permit request must NOT steal the 2 free permits
      small = Thread.new do
        semaphore.acquire(1)
        order << :small
        semaphore.release(1)
      end
      parked.pop
      wait_for_queue(semaphore, 2)

      # a further 3-permit request queues behind the small one
      second_heavy = Thread.new do
        semaphore.acquire(3)
        order << :second_heavy
        semaphore.release(3)
      end
      parked.pop
      wait_for_queue(semaphore, 3)

      expect(order).to be_empty

      # Donate one permit: the head becomes grantable (3 free). Once it
      # releases its 3, the small request is next (needs 1); only after it
      # releases is the final 3-permit request satisfiable. This ordering is
      # decided by different weights, not by wake-up scheduling.
      Thread.new { semaphore.release(1) }

      heavy.join
      expect(order.pop).to eq :heavy
      small.join
      expect(order.pop).to eq :small
      second_heavy.join
      expect(order.pop).to eq :second_heavy

      # every permit is accounted for: 2 initial + 1 donated
      expect(semaphore.available_permits).to eq 3
      semaphore.reduce_permits(1)
      expect(semaphore.available_permits).to eq 2
    end

    it 'never partially reserves permits: a heavy head does not consume what it cannot use' do
      semaphore = new_fair(2)

      acquired = Concurrent::AtomicFixnum.new(0)
      heavy = Thread.new do
        semaphore.acquire(3)
        acquired.increment
        semaphore.release(3)
      end
      sleep 0.05

      # the head wants 3 but only 2 exist; the 2 must stay untouched
      expect(semaphore.available_permits).to eq 2

      # donate the missing permit so the head can be granted, then claw it
      # back with reduce_permits once the head has returned its 3
      Thread.new { semaphore.release(1) }
      heavy.join(5)
      expect(acquired.value).to eq 1
      semaphore.reduce_permits(1)
      expect(semaphore.available_permits).to eq 2
    end

    it 'can serve several consecutive small head requests without losing permits' do
      semaphore = new_fair(2)
      results   = Queue.new

      threads = Array.new(4) do
        Thread.new do
          expect(semaphore.acquire(1)).to be_nil
          results << :ok
          sleep 0.01
          semaphore.release(1)
        end
      end
      threads.each { |t| t.join(5) }
      expect(Array.new(4) { results.pop }).to eq [:ok, :ok, :ok, :ok]
      expect(semaphore.available_permits).to eq 2
    end
  end

  describe 'head timeout / cancellation' do
    it 'advances the next request promptly when the head times out' do
      semaphore = new_fair(1)

      # head needs 3 permits but only 1 exists, and gives up quickly
      head = Thread.new { semaphore.try_acquire(3, 0.05) }

      # next request needs just 1 permit; it must be served once head bails
      next_got = Concurrent::AtomicBoolean.new(false)
      tail = Thread.new do
        semaphore.acquire(1)
        next_got.make_true
        semaphore.release(1)
      end

      head.join(2)
      tail.join(2)
      expect(next_got.true?).to be true
      expect(semaphore.available_permits).to eq 1
    end

    it 'returns false on timeout without consuming permits' do
      semaphore = new_fair(0)
      expect(semaphore.try_acquire(2, 0.05)).to be false
      expect(semaphore.available_permits).to eq 0
    end

    it 'grants a timed request if permits arrive before the deadline' do
      semaphore = new_fair(0)
      Thread.new { sleep 0.05; semaphore.release(2) }
      expect(semaphore.try_acquire(2, 1)).to be true
      expect(semaphore.available_permits).to eq 0
      semaphore.release(2)
    end

    it 'removes a timed-out head so a following timed request can succeed' do
      semaphore = new_fair(1)

      slow = Thread.new { semaphore.try_acquire(2, 0.05) } # will time out
      fast = Thread.new do
        sleep 0.1 # queues after slow, still has time after slow bails
        semaphore.try_acquire(1, 1)
      end

      expect(slow.value).to be false
      expect(fast.join(3).value).to be true
      semaphore.release(1)
      expect(semaphore.available_permits).to eq 1
    end
  end

  describe '#try_acquire respects the queue' do
    it 'cannot cut in front of a queued request even when permits are free' do
      semaphore = new_fair(2)

      blocker = Thread.new { semaphore.acquire(3) } # head, unsatisfiable
      sleep 0.05

      # 2 permits are free, but a request is already queued ahead
      expect(semaphore.try_acquire(1)).to be false
      expect(semaphore.try_acquire(2, 0.05)).to be false
      expect(semaphore.available_permits).to eq 2

      semaphore.release(1)
      blocker.join(3)
      expect(semaphore.available_permits).to eq 0
      semaphore.release(3)
      expect(semaphore.available_permits).to eq 3
    end

    it 'succeeds immediately when the queue is empty and permits are available' do
      semaphore = new_fair(3)
      expect(semaphore.try_acquire(2)).to be true
      expect(semaphore.available_permits).to eq 1
      semaphore.release(2)
    end

    it 'drains nothing while requests are queued' do
      semaphore = new_fair(2)
      blocker = Thread.new { semaphore.acquire(3) }
      sleep 0.05
      expect(semaphore.drain_permits).to eq 0
      expect(semaphore.available_permits).to eq 2
      semaphore.release(1)
      blocker.join(3)
      semaphore.release(3)
    end
  end

  describe 'release/timeout ordering' do
    # A timed head and a releaser are released from a barrier at the same
    # instant, both acting outside the semaphore lock. When the release lands
    # before the head's deadline the head must be granted exactly once; when
    # it lands after the deadline the head must time out and the released
    # permit must remain (never lost, never double granted).

    it 'grants exactly once when the release lands before the deadline' do
      semaphore = new_fair(0)
      barrier   = Concurrent::CyclicBarrier.new(2)
      grant     = Concurrent::AtomicFixnum.new(0)

      head = Thread.new do
        barrier.wait(5)
        result = semaphore.try_acquire(1, 1)
        grant.increment if result
        result
      end

      releaser = Thread.new do
        barrier.wait(5)
        semaphore.release(1) # well before the 1s deadline
      end

      expect(head.join(5).value).to be true
      releaser.join(5)
      expect(grant.value).to eq 1
      expect(semaphore.available_permits).to eq 0
      semaphore.release(1)
    end

    it 'times out and keeps the permit when the release lands after the deadline' do
      semaphore = new_fair(0)
      barrier   = Concurrent::CyclicBarrier.new(2)
      grant     = Concurrent::AtomicFixnum.new(0)

      head = Thread.new do
        barrier.wait(5)
        result = semaphore.try_acquire(1, 0.02)
        grant.increment if result
        result
      end

      releaser = Thread.new do
        barrier.wait(5)
        sleep 0.1 # strictly after the head's deadline
        semaphore.release(1)
      end

      expect(head.join(5).value).to be false
      releaser.join(5)
      expect(grant.value).to eq 0
      # the head gave up; the later release must not have been swallowed
      expect(semaphore.available_permits).to eq 1
    end

    # The genuine critical-section collision: hammer timeouts and releases
    # against the same head/queue boundary from many threads at once. This
    # directly exercises the requirement "use a barrier to control the
    # ordering of release and timeout" at concurrency, asserting the global
    # permit invariant after every iteration.
    it 'never loses permits or grants one request twice under concurrent release/timeout' do
      120.times do
        semaphore = new_fair(0)
        start     = Concurrent::CyclicBarrier.new(5)
        grants    = Concurrent::AtomicFixnum.new(0)

        # four heads that will mostly time out
        heads = Array.new(4) do
          Thread.new do
            start.wait(5)
            result = semaphore.try_acquire(1, 0.02)
            grants.increment if result
            result
          end
        end

        # one releaser colliding with the heads' deadlines
        releaser = Thread.new do
          start.wait(5)
          semaphore.release(1)
        end

        results = heads.map do |t|
          joined = t.join(5)
          raise 'head thread did not finish' unless joined
          joined.value
        end
        expect(releaser.join(5)).not_to be_nil

        granted_count = results.count(true)
        expect(granted_count).to eq grants.value
        # at most the single released permit can have been handed out
        expect(granted_count).to be <= 1

        if granted_count == 1
          expect(semaphore.available_permits).to eq 0
        else
          expect(semaphore.available_permits).to eq 1
        end
      end
    end
  end
  describe 'sustained weighted traffic' do
    it 'preserves every permit and never overcommits' do
      semaphore   = new_fair(4)
      stop        = Concurrent::AtomicBoolean.new(false)
      in_critical = Concurrent::AtomicFixnum.new(0)
      max_seen    = Concurrent::AtomicFixnum.new(0)
      errors      = Queue.new

      workers = Array.new(6) do |i|
        Thread.new do
          weight = (i % 3) + 1
          loop do
            break if stop.true?
            acquired = semaphore.try_acquire(weight, 0.05)
            next unless acquired

            current = in_critical.increment(weight)
            max_seen.update { |v| [v, current].max }
            raise "overcommitted: #{current}" if current > 4
            sleep(rand * 0.005)
            in_critical.decrement(weight)
            semaphore.release(weight)
          end
        rescue => e
          errors << e
        end
      end

      sleep 1.5
      stop.make_true
      workers.each { |w| w.join(5) }

      expect(errors).to be_empty
      expect(max_seen.value).to be <= 4
      expect(in_critical.value).to eq 0
      expect(semaphore.available_permits).to eq 4
    end
  end
end

module Concurrent
  RSpec.describe MutexSemaphore do
    it_should_behave_like :fair_semaphore
  end

  RSpec.describe Semaphore do
    it_should_behave_like :fair_semaphore
  end
end
