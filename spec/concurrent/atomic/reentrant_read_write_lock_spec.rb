require 'concurrent/utility/engine'
require 'concurrent/atomic/reentrant_read_write_lock'
require 'concurrent/atomic/count_down_latch'
require 'concurrent/atomic/cyclic_barrier'
require 'concurrent/atomic/atomic_boolean'
require 'concurrent/set'
require 'timeout'
require 'fiber'

module Concurrent
  BaseMatcher = RSpec::Matchers::BuiltIn::BaseMatcher

  class TrackedReentrantReadWriteLock < ReentrantReadWriteLock
    def initialize(scope = Thread)
      super()

      @scope = scope
      @read_acquired = Concurrent::Set.new
      @write_acquired = Concurrent::Set.new
    end

    attr :read_acquired
    attr :write_acquired

    def try_read_lock
      if super
        @read_acquired.add(@scope.current)
        true
      else
        false
      end
    end

    def try_write_lock
      if super
        @write_acquired.add(@scope.current)
        true
      else
        false
      end
    end

    def acquire_read_lock(timeout = nil)
      if super(timeout)
        @read_acquired.add(@scope.current)
        true
      else
        false
      end
    end

    def acquire_write_lock(timeout = nil)
      if super(timeout)
        @write_acquired.add(@scope.current)
        true
      else
        false
      end
    end

    def release_read_lock
      super.tap do
        @read_acquired.delete(@scope.current)
      end
    end

    def release_write_lock
      super.tap do
        @write_acquired.delete(@scope.current)
      end
    end
  end

  # ****************************************************************
  # First some custom matchers to make our tests all nice and pretty
  # ****************************************************************

  class HoldLock
    def initialize(lock)
      @lock = lock
    end

    def for_read
      HoldReadLock.new(@lock)
    end

    def for_write
      HoldWriteLock.new(@lock)
    end

    def for_both
      HoldBoth.new(@lock)
    end
  end

  class HoldReadLock < BaseMatcher
    def match(lock, scope)
      ((lock.instance_eval { @Counter.value } & ReentrantReadWriteLock::MAX_READERS) != 0) && lock.read_acquired.include?(scope)
    end
  end

  class HoldWriteLock < BaseMatcher
    def match(lock, scope)
      ((lock.instance_eval { @Counter.value } & ReentrantReadWriteLock::RUNNING_WRITER) != 0) && lock.write_acquired.include?(scope)
    end
  end

  class HoldBoth < BaseMatcher
    def match(lock, scope)
      HoldReadLock.new(lock).matches?(scope) && HoldWriteLock.new(lock).matches?(scope)
    end
  end

  class BeFree < BaseMatcher
    MASK = ReentrantReadWriteLock::MAX_READERS + ReentrantReadWriteLock::RUNNING_WRITER

    def matches?(lock)
      (lock.instance_eval { @Counter.value } & MASK) == 0
    end

    def failure_message
      "expected lock to be free"
    end
  end

  # *******************************************************

  RSpec.shared_context TrackedReentrantReadWriteLock do
    def hold(lock)
      HoldLock.new(lock)
    end

    def be_free
      BeFree.new
    end

    def wait_up_to(secs, &condition)
      _end = Time.now + secs
      while !condition.call && Time.now < _end
        sleep(0.001)
      end
    end
  end

  RSpec.describe ReentrantReadWriteLock do
    include_context TrackedReentrantReadWriteLock

    let(:lock) { TrackedReentrantReadWriteLock.new }

    context "read lock" do

      it "allows other read locks to be acquired at the same time" do
        lock # stupid RSpec 'let' is not thread-safe!
        Timeout.timeout(3) do
          got_lock = 10.times.collect { CountDownLatch.new }
          threads  = 10.times.collect do |n|
            in_thread do
              # Each thread takes the read lock and then waits for another one
              # They will only finish if ALL of them get their read lock
              expect(lock.acquire_read_lock).to be true
              expect(Thread.current).to hold(lock).for_read
              got_lock[n].count_down
              got_lock[(n+1) % 10].wait
            end
          end
          threads.each(&:join)
        end
      end

      it "can be acquired more than once" do
        Timeout.timeout(3) do
          10.times { expect(lock.acquire_read_lock).to be true }
          expect(Thread.current).to hold(lock).for_read
          10.times { expect(lock.release_read_lock).to be true }
          expect(Thread.current).not_to hold(lock).for_read
          expect(lock).to be_free
        end
      end

      it "cannot be acquired more times than fits the per-thread counter" do
        ReentrantReadWriteLock::READ_LOCK_MASK.times { expect(lock.acquire_read_lock).to be true }
        expect { lock.acquire_read_lock }.to raise_error(ResourceLimitError)
        expect { lock.try_read_lock }.to raise_error(ResourceLimitError)
        # This thread is the only reader, so it may still take the write lock even
        # though its per-thread read depth fills the mask: the global counter only
        # ever holds its single reader unit, independent of that depth.
        expect(lock.try_write_lock).to be true
        lock.release_write_lock
        ReentrantReadWriteLock::READ_LOCK_MASK.times { expect(lock.release_read_lock).to be true }
        expect(lock).to be_free
      end

      it "can be acquired while holding a write lock" do
        Timeout.timeout(3) do
          expect(lock.acquire_write_lock).to be true
          expect(Thread.current).to hold(lock).for_write
          expect(lock.acquire_read_lock).to be true
          expect(Thread.current).to hold(lock).for_both
          expect(lock.release_read_lock).to be true
          expect(Thread.current).to hold(lock).for_write
          expect(Thread.current).not_to hold(lock).for_read
          expect(lock.release_write_lock).to be true
          expect(lock).to be_free
        end
      end

      it "can be upgraded to a write lock" do
        Timeout.timeout(3) do
          expect(lock.acquire_read_lock).to be true
          expect(Thread.current).to hold(lock).for_read
          # now we want to upgrade...
          expect(lock.acquire_write_lock).to be true
          expect(lock.release_read_lock).to be true
          expect(Thread.current).to hold(lock).for_write

          expect(lock.release_write_lock).to be true
          expect(lock).to be_free
        end
      end

      it "can be upgraded to a write lock when read lock acquired more than once" do
        Timeout.timeout(3) do
          expect(lock.acquire_read_lock).to be true
          expect(lock.acquire_read_lock).to be true
          expect(Thread.current).to hold(lock).for_read

          # now we want to upgrade...
          expect(lock.acquire_write_lock).to be true
          expect(lock.release_read_lock).to be true
          expect(lock.release_read_lock).to be true
          expect(Thread.current).to hold(lock).for_write

          expect(lock.release_write_lock).to be true
          expect(lock).to be_free
        end
      end

      it "cannot be released when not held" do
        expect { lock.release_read_lock }.to raise_error(IllegalOperationError)
      end

      it "cannot be released more times than it was taken" do
        Timeout.timeout(3) do
          2.times { lock.acquire_read_lock }
          2.times { lock.release_read_lock }
          expect { lock.release_read_lock }.to raise_error(IllegalOperationError)
        end
      end

      it "wakes up waiting writers when the last read lock is released" do
        latch1,latch2 = CountDownLatch.new(3),CountDownLatch.new
        good = AtomicBoolean.new(false)
        threads = [
          in_thread { lock.acquire_read_lock; latch1.count_down; latch2.wait; lock.release_read_lock },
          in_thread { lock.acquire_read_lock; latch1.count_down; latch2.wait; lock.release_read_lock },
          in_thread { lock.acquire_read_lock; latch1.count_down; latch2.wait; lock.release_read_lock },
          in_thread { latch1.wait; lock.acquire_write_lock; good.value = true }
        ]
        wait_up_to(0.5) { threads[3].status == 'sleep' }
        # The last thread should be waiting to acquire a write lock now...
        expect(threads[3].status).to eql "sleep"
        expect(threads[3]).not_to hold(lock).for_write
        expect(good.value).to be false
        # Throw latch2 and the 3 readers will wake up and all release their read locks...
        latch2.count_down
        wait_up_to(0.5) { good.value }
        expect(threads[3]).to hold(lock).for_write
        expect(good.value).to be true
      end
    end

    context "write lock" do
      it "cannot be acquired when another thread holds a write lock" do
        latch   = CountDownLatch.new
        threads = [
          in_thread { lock.acquire_write_lock; latch.count_down },
          in_thread { latch.wait; lock.acquire_write_lock }
        ]
        expect { Timeout.timeout(1) { threads[0].join }}.not_to raise_error
        expect(threads[0]).to hold(lock).for_write
        expect(threads[1]).not_to hold(lock).for_write
        wait_up_to(0.5) { threads[1].status == 'sleep' }
        expect(threads[1].status).to eql "sleep"
      end

      it "cannot be acquired when another thread holds a read lock" do
        latch   = CountDownLatch.new
        threads = [
          in_thread { lock.acquire_read_lock; latch.count_down },
          in_thread { latch.wait; lock.acquire_write_lock }
        ]
        expect { Timeout.timeout(1) { threads[0].join }}.not_to raise_error
        expect(threads[0]).to hold(lock).for_read
        expect(threads[1]).not_to hold(lock).for_write
        wait_up_to(0.5) { threads[1].status == 'sleep' }
        expect(threads[1].status).to eql "sleep"
      end

      it "can be acquired more than once" do
        Timeout.timeout(3) do
          10.times { expect(lock.acquire_write_lock).to be true }
          expect(Thread.current).to hold(lock).for_write
          10.times { expect(lock.release_write_lock).to be true }
          expect(Thread.current).not_to hold(lock).for_write
          expect(lock).to be_free
        end
      end

      it "can be acquired while holding a read lock" do
        Timeout.timeout(3) do
          expect(lock.acquire_read_lock).to be true
          expect(Thread.current).to hold(lock).for_read
          expect(lock.acquire_write_lock).to be true
          expect(Thread.current).to hold(lock).for_both
          expect(lock.release_write_lock).to be true
          expect(Thread.current).to hold(lock).for_read
          expect(Thread.current).not_to hold(lock).for_write
          expect(lock.release_read_lock).to be true
          expect(lock).to be_free
        end
      end

      it "can be downgraded to a read lock" do
        Timeout.timeout(3) do
          expect(lock.acquire_write_lock).to be true
          expect(Thread.current).to hold(lock).for_write
          # now we want to downgrade...
          expect(lock.acquire_read_lock).to be true
          expect(lock.release_write_lock).to be true
          expect(Thread.current).to hold(lock).for_read

          expect(lock.release_read_lock).to be true
          expect(lock).to be_free
        end
      end

      it "cannot be released when not held" do
        expect { lock.release_write_lock }.to raise_error(IllegalOperationError)
      end

      it "cannot be released more times than it was taken" do
        Timeout.timeout(3) do
          2.times { lock.acquire_write_lock }
          2.times { lock.release_write_lock }
          expect { lock.release_write_lock }.to raise_error(IllegalOperationError)
        end
      end

      it "wakes up waiting readers when the write lock is released" do
        latch1,latch2 = CountDownLatch.new,CountDownLatch.new
        good = AtomicFixnum.new(0)
        threads = [
          in_thread { lock.acquire_write_lock; latch1.count_down; latch2.wait; lock.release_write_lock },
          in_thread { latch1.wait; lock.acquire_read_lock; good.update { |n| n+1 }},
          in_thread { latch1.wait; lock.acquire_read_lock; good.update { |n| n+1 }},
          in_thread { latch1.wait; lock.acquire_read_lock; good.update { |n| n+1 }}
        ]
        wait_up_to(0.5) { threads[3].status == 'sleep' }
        # The last 3 threads should be waiting to acquire read locks now...
        unless Concurrent.on_jruby? # flaky on JRuby
          (1..3).each { |n| expect(threads[n].status).to eql "sleep" }
        end
        (1..3).each { |n| expect(threads[n]).not_to hold(lock).for_read }
        # Throw latch2 and the writer will wake up and release its write lock...
        latch2.count_down
        wait_up_to(0.5) { good.value == 3 }
        (1..3).each { |n| expect(threads[n]).to hold(lock).for_read }
      end

      it "wakes up waiting writers when the write lock is released" do
        latch1,latch2 = CountDownLatch.new,CountDownLatch.new
        good = AtomicBoolean.new(false)
        threads = [
          in_thread { lock.acquire_write_lock; latch1.count_down; latch2.wait; lock.release_write_lock },
          in_thread { latch1.wait; lock.acquire_write_lock; good.value = true },
        ]
        wait_up_to(0.5) { threads[1].status == 'sleep' }
        # The last thread should be waiting to acquire a write lock now...
        unless Concurrent.on_jruby? # flaky on JRuby
          expect(threads[1].status).to eql "sleep"
        end
        expect(threads[1]).not_to hold(lock).for_write
        # Throw latch2 and the writer will wake up and release its write lock...
        latch2.count_down
        wait_up_to(0.5) { good.value }
        expect(threads[1]).to hold(lock).for_write
      end
    end

    context "#with_read_lock" do

      it "acquires read block before yielding, then releases it" do
        expect(lock).to be_free
        lock.with_read_lock { expect(Thread.current).to hold(lock).for_read }
        expect(lock).to be_free
      end

      it "releases read lock if an exception is raised in block" do
        expect {
          lock.with_read_lock { raise "Bad" }
        }.to raise_error(RuntimeError, 'Bad')
        expect(lock).to be_free
        expect(Thread.current).not_to hold(lock).for_read
      end
    end

    context "#with_write_lock" do

      it "acquires write block before yielding, then releases it" do
        expect(lock).to be_free
        lock.with_write_lock { expect(Thread.current).to hold(lock).for_write }
        expect(lock).to be_free
      end

      it "releases write lock if an exception is raised in block" do
        expect {
          lock.with_write_lock { raise "Bad" }
        }.to raise_error(RuntimeError, 'Bad')
        expect(lock).to be_free
        expect(Thread.current).not_to hold(lock).for_write
      end
    end

    context "#try_read_lock" do

      it "returns false immediately if read lock cannot be obtained" do
        Timeout.timeout(3) do
          latch  = CountDownLatch.new
          in_thread { lock.acquire_write_lock; latch.count_down }

          latch.wait
          expect {
            Timeout.timeout(0.01) { expect(lock.try_read_lock).to be false }
          }.not_to raise_error
          expect(Thread.current).not_to hold(lock).for_read
        end
      end

      it "acquires read lock and returns true if it can do so without blocking" do
        Timeout.timeout(3) do
          latch  = CountDownLatch.new
          in_thread { lock.acquire_read_lock; latch.count_down }

          latch.wait
          expect {
            Timeout.timeout(0.01) { expect(lock.try_read_lock).to be true }
          }.not_to raise_error
          expect(lock).not_to be_free
          expect(Thread.current).to hold(lock).for_read
        end
      end

      it "can acquire a read lock if a read lock is already held" do
        Timeout.timeout(3) do
          expect(lock.acquire_read_lock).to be true
          expect(lock.try_read_lock).to be true
          expect(Thread.current).to hold(lock).for_read
          expect(lock.release_read_lock).to be true
          expect(lock.release_read_lock).to be true
          expect(Thread.current).not_to hold(lock).for_read
          expect(lock).to be_free
        end
      end

      it "can acquire a read lock if a write lock is already held" do
        Timeout.timeout(3) do
          expect(lock.acquire_write_lock).to be true
          expect(lock.try_read_lock).to be true
          expect(Thread.current).to hold(lock).for_read
          expect(lock.release_read_lock).to be true
          expect(lock.release_write_lock).to be true
          expect(Thread.current).not_to hold(lock).for_read
          expect(lock).to be_free
        end
      end
    end

    context "#try_write_lock" do

      it "returns false immediately if write lock cannot be obtained" do
        Timeout.timeout(3) do
          latch  = CountDownLatch.new
          in_thread { lock.acquire_write_lock; latch.count_down }

          latch.wait
          expect {
            Timeout.timeout(0.02) { expect(lock.try_write_lock).to be false }
          }.not_to raise_error
          expect(Thread.current).not_to hold(lock).for_write
        end
      end

      it "acquires write lock and returns true if it can do so without blocking" do
        Timeout.timeout(3) do
          expect {
            Timeout.timeout(0.02) { expect(lock.try_write_lock).to be true }
          }.not_to raise_error
          expect(lock).not_to be_free
          expect(Thread.current).to hold(lock).for_write
        end
      end

      it "can acquire a write lock if a read lock is already held" do
        Timeout.timeout(3) do
          expect(lock.acquire_read_lock).to be true
          expect(lock.try_write_lock).to be true
          expect(Thread.current).to hold(lock).for_write
          expect(lock.release_write_lock).to be true
          expect(lock.release_read_lock).to be true
          expect(Thread.current).not_to hold(lock).for_write
          expect(lock).to be_free
        end
      end

      it "can acquire a write lock if a write lock is already held" do
        Timeout.timeout(3) do
          expect(lock.acquire_write_lock).to be true
          expect(lock.try_write_lock).to be true
          expect(Thread.current).to hold(lock).for_write
          expect(lock.release_write_lock).to be true
          expect(lock.release_write_lock).to be true
          expect(Thread.current).not_to hold(lock).for_write
          expect(lock).to be_free
        end
      end
    end

    it "can survive a torture test" do
      count = 0
      writers = 5.times.collect do
        in_thread do
          500.times do
            lock.with_write_lock do
              value = (count += 1)
              sleep(0.0001)
              count = value+1
            end
          end
        end
      end
      readers = 15.times.collect do
        in_thread do
          500.times do
            lock.with_read_lock { expect(count % 2).to eq 0 }
          end
        end
      end
      writers.each(&:join)
      readers.each(&:join)
      expect(count).to eq 5000
    end

    context "#acquire_upgradeable_read_lock" do
      it "acquires a read lock and the exclusive upgrade slot" do
        Timeout.timeout(3) do
          expect(lock.acquire_upgradeable_read_lock).to be true
          expect(Thread.current).to hold(lock).for_read
          expect(lock.instance_eval { @UpgradeHolder }).to eq Thread.current
          lock.release_upgradeable_read_lock
          expect(lock).to be_free
          expect(lock.instance_eval { @UpgradeHolder }).to be_nil
        end
      end

      it "is reentrant for the slot owner" do
        Timeout.timeout(3) do
          3.times { expect(lock.acquire_upgradeable_read_lock).to be true }
          expect(lock.instance_eval { upgrade_count }).to eq 3
          3.times { lock.release_upgradeable_read_lock }
          expect(lock).to be_free
          expect(lock.instance_eval { @UpgradeHolder }).to be_nil
        end
      end

      it "lets only one thread hold the slot at a time" do
        latch = CountDownLatch.new
        second = in_thread do
          latch.wait
          lock.acquire_upgradeable_read_lock
          lock.release_upgradeable_read_lock
        end
        lock.acquire_upgradeable_read_lock
        latch.count_down
        wait_up_to(0.5) { second.status == 'sleep' }
        expect(second.status).to eql 'sleep'
        expect(lock.instance_eval { @UpgradeHolder }).to eq Thread.current
        lock.release_upgradeable_read_lock
        expect { Timeout.timeout(2) { second.join } }.not_to raise_error
        expect(lock).to be_free
      end

      it "raises TimeoutError when the slot stays busy and restores a clean state" do
        latch = CountDownLatch.new
        waiter = in_thread do
          latch.wait
          begin
            lock.acquire_upgradeable_read_lock(0.05)
            :acquired
          rescue Concurrent::TimeoutError
            :timed_out
          end
        end
        lock.acquire_upgradeable_read_lock
        latch.count_down
        expect(Timeout.timeout(2) { waiter.value }).to eq :timed_out
        expect(lock.instance_eval { @UpgradeHolder }).to eq Thread.current
        lock.release_upgradeable_read_lock
        expect(lock).to be_free
      end

      it "releases the slot when the wrapped block raises" do
        expect { lock.with_upgradeable_read_lock { raise 'boom' } }.to raise_error('boom')
        expect(lock.instance_eval { @UpgradeHolder }).to be_nil
        expect(lock).to be_free
      end
    end

    context "#try_acquire_upgradeable_read_lock" do
      it "succeeds when the slot is free" do
        expect(lock.try_acquire_upgradeable_read_lock).to be true
        expect(Thread.current).to hold(lock).for_read
        lock.release_upgradeable_read_lock
        expect(lock).to be_free
      end

      it "returns false immediately when another thread owns the slot" do
        latch = CountDownLatch.new
        result = nil
        other = in_thread do
          latch.wait
          result = lock.try_acquire_upgradeable_read_lock
        end
        lock.acquire_upgradeable_read_lock
        latch.count_down
        other.join(2)
        expect(result).to be false
        expect(lock.instance_eval { @UpgradeHolder }).to eq Thread.current
        lock.release_upgradeable_read_lock
        expect(lock).to be_free
      end
    end

    context "#release_upgradeable_read_lock" do
      it "raises when no upgradeable read lock is held" do
        expect { lock.release_upgradeable_read_lock }.to raise_error(IllegalOperationError)
      end
    end

    context "#upgrade_read_lock" do
      it "upgrades the slot owner to the write lock and keeps the read lock" do
        Timeout.timeout(3) do
          lock.acquire_upgradeable_read_lock
          expect(lock.upgrade_read_lock).to be true
          expect(Thread.current).to hold(lock).for_both
          lock.release_write_lock
          expect(Thread.current).to hold(lock).for_read
          lock.release_upgradeable_read_lock
          expect(lock).to be_free
        end
      end

      it "raises when the caller does not own the upgrade slot" do
        lock.acquire_read_lock
        expect { lock.upgrade_read_lock }.to raise_error(IllegalOperationError)
        lock.release_read_lock
        expect(lock).to be_free
      end

      it "raises when the write lock is already held" do
        lock.acquire_upgradeable_read_lock
        lock.upgrade_read_lock
        expect { lock.upgrade_read_lock }.to raise_error(IllegalOperationError)
        lock.release_write_lock
        lock.release_upgradeable_read_lock
        expect(lock).to be_free
      end

      it "waits for other readers without deadlocking and then upgrades" do
        # Deterministic interleaving: an upgrade candidate and a plain reader hold
        # read locks at the same time. The upgrader must park until the plain
        # reader leaves and then obtain the write lock instead of deadlocking.
        both_reading   = CountDownLatch.new(2)
        release_reader = CountDownLatch.new
        upgraded       = CountDownLatch.new

        plain_reader = in_thread do
          lock.acquire_read_lock
          both_reading.count_down
          release_reader.wait(2)
          lock.release_read_lock
        end

        upgrader = in_thread do
          lock.acquire_upgradeable_read_lock
          both_reading.count_down
          # The plain reader is still holding a read lock here, so this blocks.
          lock.upgrade_read_lock
          upgraded.count_down
          lock.release_write_lock
          lock.release_upgradeable_read_lock
        end

        expect(both_reading.wait(2)).to be true
        # The upgrader must be parked behind the plain reader, not deadlocked.
        wait_up_to(1) { upgrader.status == 'sleep' }
        expect(upgrader.status).to eql 'sleep'
        expect(upgraded.count).to eq 1
        # Releasing the plain reader lets the parked upgrade complete.
        release_reader.count_down
        expect(upgraded.wait(2)).to be true
        plain_reader.join(2)
        upgrader.join(2)
        expect(lock).to be_free
      end

      it "keeps the exact read depth and upgrade slot after a timeout" do
        barrier = Concurrent::CyclicBarrier.new(2)
        reader_holds = CountDownLatch.new
        slow_reader = in_thread do
          barrier.wait(2)
          lock.acquire_read_lock
          reader_holds.count_down
          sleep 0.4
          lock.release_read_lock
        end
        lock.acquire_upgradeable_read_lock
        barrier.wait(2)
        reader_holds.wait(2)
        expect { lock.upgrade_read_lock(0.05) }.to raise_error(Concurrent::TimeoutError)
        # The write attempt is undone, but the upgradeable read lock is retained:
        # same read depth, same slot ownership, and the counter still shows us as
        # a reader without any running or waiting writer.
        expect(lock.instance_eval { @HeldCount.value }).to eq 1
        expect(lock.instance_eval { @UpgradeHolder }).to eq Thread.current
        expect(lock).not_to be_write_locked
        # Once the slow reader leaves, retrying the upgrade succeeds.
        slow_reader.join(2)
        lock.upgrade_read_lock
        lock.release_write_lock
        lock.release_upgradeable_read_lock
        expect(lock).to be_free
      end

      it "does not let two simultaneous upgraders deadlock" do
        start = Concurrent::CyclicBarrier.new(3)
        upgraded = Concurrent::AtomicFixnum.new(0)
        threads = 3.times.map do
          in_thread do
            start.wait(2)
            lock.acquire_upgradeable_read_lock
            lock.upgrade_read_lock
            upgraded.increment
            lock.release_write_lock
            lock.release_upgradeable_read_lock
          end
        end
        expect { Timeout.timeout(5) { threads.each(&:join) } }.not_to raise_error
        expect(upgraded.value).to eq 3
        expect(lock).to be_free
      end
    end

    context "#try_upgrade_read_lock" do
      it "returns true immediately when no other reader is active" do
        lock.acquire_upgradeable_read_lock
        expect(lock.try_upgrade_read_lock).to be true
        expect(Thread.current).to hold(lock).for_both
        lock.release_write_lock
        lock.release_upgradeable_read_lock
        expect(lock).to be_free
      end

      it "returns false and retains the read lock when another reader is active" do
        reader_holds = CountDownLatch.new
        other = in_thread do
          lock.acquire_read_lock
          reader_holds.count_down
          sleep 0.3
          lock.release_read_lock
        end
        lock.acquire_upgradeable_read_lock
        reader_holds.wait(2)
        expect(lock.try_upgrade_read_lock).to be false
        expect(Thread.current).to hold(lock).for_read
        other.join(2)
        lock.release_upgradeable_read_lock
        expect(lock).to be_free
      end

      it "raises when the caller does not own the upgrade slot" do
        expect { lock.try_upgrade_read_lock }.to raise_error(IllegalOperationError)
      end
    end

    context "#downgrade_write_lock" do
      it "downgrades an upgraded write lock back to a read lock" do
        Timeout.timeout(3) do
          lock.acquire_upgradeable_read_lock
          lock.upgrade_read_lock
          expect(lock.downgrade_write_lock).to be true
          expect(Thread.current).to hold(lock).for_read
          expect(lock).not_to be_write_locked
          lock.release_upgradeable_read_lock
          expect(lock).to be_free
        end
      end

      it "downgrades a plain write lock through a read lock with no unlocked gap" do
        Timeout.timeout(3) do
          lock.acquire_write_lock
          lock.downgrade_write_lock
          expect(Thread.current).to hold(lock).for_read
          expect(lock).not_to be_write_locked
          lock.release_read_lock
          expect(lock).to be_free
        end
      end

      it "never leaves an unlocked window a competing writer can enter" do
        intruded = AtomicBoolean.new(false)
        100.times do
          lock.acquire_write_lock
          intruder = in_thread { intruded.make_true if lock.try_write_lock }
          lock.downgrade_write_lock
          intruder.join(2)
          lock.release_read_lock
        end
        expect(intruded.value).to be false
        expect(lock).to be_free
      end

      it "raises when the write lock is not held" do
        lock.acquire_read_lock
        expect { lock.downgrade_write_lock }.to raise_error(IllegalOperationError)
        lock.release_read_lock
        expect(lock).to be_free
      end
    end

    context "timed acquisition" do
      it "times out acquire_write_lock and rolls back every reservation" do
        gate = CountDownLatch.new
        holder = in_thread { lock.acquire_write_lock; gate.count_down; sleep 0.4; lock.release_write_lock }
        gate.wait(2)
        expect { lock.acquire_write_lock(0.05) }.to raise_error(Concurrent::TimeoutError)
        holder.join(2)
        expect(lock).to be_free
      end

      it "times out acquire_read_lock behind a writer without changing state" do
        gate = CountDownLatch.new
        holder = in_thread { lock.acquire_write_lock; gate.count_down; sleep 0.4; lock.release_write_lock }
        gate.wait(2)
        expect { lock.acquire_read_lock(0.05) }.to raise_error(Concurrent::TimeoutError)
        holder.join(2)
        expect(lock).to be_free
      end
    end

    context "writer fairness" do
      it "does not let later readers starve a waiting writer" do
        # A writer queues behind an initial reader; late readers arriving while it
        # waits must not overtake it. Barriers pin the interleaving deterministically.
        initial_gate = CountDownLatch.new
        writer_ready  = Concurrent::CyclicBarrier.new(2)
        late_ready    = Concurrent::CyclicBarrier.new(4)
        writer_went   = AtomicBoolean.new(false)
        readers_ran_before_writer = Concurrent::AtomicFixnum.new(0)

        initial = in_thread do
          lock.acquire_read_lock
          initial_gate.count_down
          Thread.stop
          lock.release_read_lock
        end
        initial_gate.wait(2)

        writer = in_thread do
          writer_ready.wait(2)
          lock.acquire_write_lock
          writer_went.make_true
          lock.release_write_lock
        end
        writer_ready.wait(2)
        wait_up_to(0.5) { writer.status == 'sleep' }

        late = 3.times.map do
          in_thread do
            late_ready.wait(2)
            lock.acquire_read_lock
            readers_ran_before_writer.increment unless writer_went.true?
            lock.release_read_lock
          end
        end
        late_ready.wait(2)
        sleep 0.1
        expect(writer_went.value).to be false
        expect(readers_ran_before_writer.value).to eq 0
        initial.wakeup
        writer.join(3)
        late.each { |t| t.join(3) }
        expect(writer_went.value).to be true
        expect(lock).to be_free
      end
    end
  end

  RSpec.describe Concurrent::ReentrantReadWriteLock, if: (Concurrent::LockLocalVar == Concurrent::FiberLocalVar) do
    include_context TrackedReentrantReadWriteLock

    let(:lock) { Concurrent::TrackedReentrantReadWriteLock.new(Fiber) }

    it "can acquire locks in separate fibers" do
      lock.with_read_lock do
        expect(Fiber.current).to hold(lock).for_read
        Fiber.new do
          expect(Fiber.current).to_not hold(lock).for_read

          lock.with_read_lock do
            expect(Fiber.current).to hold(lock).for_read
          end

        end.resume

        expect(Fiber.current).to hold(lock).for_read
      end
    end
  end
end
