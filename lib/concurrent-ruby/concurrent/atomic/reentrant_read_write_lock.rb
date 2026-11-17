require 'thread'
require 'concurrent/atomic/atomic_reference'
require 'concurrent/atomic/atomic_fixnum'
require 'concurrent/errors'
require 'concurrent/synchronization/object'
require 'concurrent/synchronization/lock'
require 'concurrent/atomic/lock_local_var'

module Concurrent

  # Re-entrant read-write lock implementation
  #
  # Allows any number of concurrent readers, but only one concurrent writer
  # (And while the "write" lock is taken, no read locks can be obtained either.
  # Hence, the write lock can also be called an "exclusive" lock.)
  #
  # If another thread has taken a read lock, any thread which wants a write lock
  # will block until all the readers release their locks. However, once a thread
  # starts waiting to obtain a write lock, any additional readers that come along
  # will also wait (so writers are not starved).
  #
  # A thread can acquire both a read and write lock at the same time. A thread can
  # also acquire a read lock OR a write lock more than once. Only when the read (or
  # write) lock is released as many times as it was acquired, will the thread
  # actually let it go, allowing other threads which might have been waiting
  # to proceed. Therefore the lock can be upgraded by first acquiring
  # read lock and then write lock and that the lock can be downgraded by first
  # having both read and write lock a releasing just the write lock.
  #
  # == Explicit, deadlock-free lock upgrade
  #
  # Naively upgrading an ordinary read lock into a write lock by calling
  # {#acquire_write_lock} while still holding the read lock deadlocks as soon as
  # two readers try to do it at the same time: each one keeps holding its read
  # lock while waiting for the *other* reader to release, so neither can ever
  # make progress.
  #
  # To avoid that, this lock offers an explicit *upgradeable read lock* through
  # {#acquire_upgradeable_read_lock} (and the non-blocking
  # {#try_acquire_upgradeable_read_lock} / timed variants). An upgradeable read
  # lock behaves exactly like an ordinary read lock for the thread that owns it,
  # but at most ONE thread may hold an upgradeable read lock at any given time.
  # When that thread later calls {#upgrade_read_lock}, it is the *only* reader
  # that is allowed to wait for the write lock. Every other reader keeps using a
  # plain read lock and simply releases it when done, so a second reader can
  # never end up waiting for the write lock while itself keeping a read lock:
  # there is no circular wait and therefore no upgrade deadlock.
  #
  # The rules for several readers asking to upgrade at the same time are thus:
  # * the first reader to request an upgradeable read lock keeps the single
  #   upgrade slot and is the only one allowed to upgrade;
  # * the later readers wait on the upgrade queue until the slot is released
  #   ({#acquire_upgradeable_read_lock}), fail immediately
  #   ({#try_acquire_upgradeable_read_lock}), or fail after a timeout
  #   ({#acquire_upgradeable_read_lock} with a `timeout`);
  # * a thread which does not own the upgrade slot is refused by
  #   {#upgrade_read_lock} / {#try_upgrade_read_lock} instead of being allowed to
  #   block while holding a plain read lock.
  #
  # Every timed or failed acquisition rolls back exactly the reservations it made
  # itself and restores the calling thread's original read depth; permits
  # belonging to other threads are never released. {#downgrade_write_lock}
  # performs the inverse transition with a fixed order - it takes the read lock
  # first and only then releases the write lock - so the downgrading thread is
  # never left unlocked in between.
  #
  # If both read and write locks are acquired by the same thread, it is not strictly
  # necessary to release them in the same order they were acquired. In other words,
  # the following code is legal:
  #
  # @example
  #   lock = Concurrent::ReentrantReadWriteLock.new
  #   lock.acquire_write_lock
  #   lock.acquire_read_lock
  #   lock.release_write_lock
  #   # At this point, the current thread is holding only a read lock, not a write
  #   # lock. So other threads can take read locks, but not a write lock.
  #   lock.release_read_lock
  #   # Now the current thread is not holding either a read or write lock, so
  #   # another thread could potentially acquire a write lock.
  #
  # This implementation was inspired by `java.util.concurrent.ReentrantReadWriteLock`.
  #
  # @example
  #   lock = Concurrent::ReentrantReadWriteLock.new
  #   lock.with_read_lock  { data.retrieve }
  #   lock.with_write_lock { data.modify! }
  #
  # @see http://docs.oracle.com/javase/7/docs/api/java/util/concurrent/locks/ReentrantReadWriteLock.html java.util.concurrent.ReentrantReadWriteLock
  class ReentrantReadWriteLock < Synchronization::Object

    # Implementation notes:
    #
    # A goal is to make the uncontended path for both readers/writers mutex-free
    # Only if there is reader-writer or writer-writer contention, should mutexes be used
    # Otherwise, a single CAS operation is all we need to acquire/release a lock
    #
    # Internal state is represented by a single integer ("counter"), and updated
    #   using atomic compare-and-swap operations
    # When the counter is 0, the lock is free
    # Each thread which has one OR MORE read locks increments the counter by 1
    #   (and decrements by 1 when releasing the read lock)
    # The counter is increased by (1 << 15) for each writer waiting to acquire the
    #   write lock, and by (1 << 29) if the write lock is taken
    #
    # Additionally, each thread uses a thread-local variable to count how many times
    #   it has acquired a read lock, AND how many times it has acquired a write lock.
    # It uses a similar trick; an increment of 1 means a read lock was taken, and
    #   an increment of (1 << 15) means a write lock was taken
    # This is what makes re-entrancy possible
    #
    # 2 rules are followed to ensure good liveness properties:
    # 1) Once a writer has queued up and is waiting for a write lock, no other thread
    #    can take a lock without waiting
    # 2) When a write lock is released, readers are given the "first chance" to wake
    #    up and acquire a read lock
    # Following these rules means readers and writers tend to "take turns", so neither
    #   can starve the other, even under heavy contention

    # @!visibility private
    READER_BITS    = 15
    # @!visibility private
    WRITER_BITS    = 14

    # Used with @Counter:
    # @!visibility private
    WAITING_WRITER = 1 << READER_BITS
    # @!visibility private
    RUNNING_WRITER = 1 << (READER_BITS + WRITER_BITS)
    # @!visibility private
    MAX_READERS    = WAITING_WRITER - 1
    # @!visibility private
    MAX_WRITERS    = RUNNING_WRITER - MAX_READERS - 1

    # Used with @HeldCount:
    # @!visibility private
    WRITE_LOCK_HELD = 1 << READER_BITS
    # @!visibility private
    READ_LOCK_MASK  = WRITE_LOCK_HELD - 1
    # @!visibility private
    WRITE_LOCK_MASK = MAX_WRITERS

    safe_initialization!

    # Create a new `ReentrantReadWriteLock` in the unlocked state.
    def initialize
      super()
      @Counter    = AtomicFixnum.new(0)       # single integer which represents lock state
      @ReadQueue  = Synchronization::Lock.new # used to queue waiting readers
      @WriteQueue = Synchronization::Lock.new # used to queue waiting writers
      # Serializes access to the single upgrade slot (@UpgradeHolder) and is used
      # to queue threads waiting to become the upgrade candidate.
      @UpgradeQueue = Synchronization::Lock.new
      # The thread (or fiber, depending on LockLocalVar semantics) which currently
      # owns the single upgradeable read lock, or nil if the slot is free. Only
      # ever read or mutated while holding @UpgradeQueue.
      @UpgradeHolder = nil
      @HeldCount  = LockLocalVar.new(0) # indicates # of R & W locks held by this thread
      # How many upgradeable read locks the current thread holds. Kept separate
      # from @HeldCount so that timed/failed operations can restore the thread's
      # ordinary read depth without touching this counter.
      @UpgradeCount = LockLocalVar.new(nil)
      # Number of read locks the current thread currently holds. Tracked
      # independently of @HeldCount so reader-unit accounting never has to decode
      # packed fields.
      @ReadHolds = LockLocalVar.new(0)
      # True once this thread's single reader unit has been placed in the global
      # counter's reader region. A thread contributes at most one reader unit
      # however many reentrant read holds it owns; the unit stays in place even
      # while the same thread also holds the write lock after an upgrade, and is
      # removed only when the thread's last read hold is released.
      @ReaderUnitInCounter = LockLocalVar.new(nil)
    end

    # Execute a block operation within a read lock.
    #
    # @yield the task to be performed within the lock.
    #
    # @return [Object] the result of the block operation.
    #
    # @raise [ArgumentError] when no block is given.
    # @raise [Concurrent::ResourceLimitError] if the maximum number of readers
    #   is exceeded.
    def with_read_lock
      raise ArgumentError.new('no block given') unless block_given?
      acquire_read_lock
      begin
        yield
      ensure
        release_read_lock
      end
    end

    # Execute a block operation within a write lock.
    #
    # @yield the task to be performed within the lock.
    #
    # @return [Object] the result of the block operation.
    #
    # @raise [ArgumentError] when no block is given.
    # @raise [Concurrent::ResourceLimitError] if the maximum number of readers
    #   is exceeded.
    def with_write_lock
      raise ArgumentError.new('no block given') unless block_given?
      acquire_write_lock
      begin
        yield
      ensure
        release_write_lock
      end
    end

    # Execute a block operation while holding an upgradeable read lock.
    #
    # An upgradeable read lock is a read lock with an exclusive option to upgrade
    # to the write lock later via {#upgrade_read_lock}. Only one thread at a time
    # can hold an upgradeable read lock, which guarantees that upgrading can never
    # deadlock.
    #
    # @param [Numeric, nil] timeout in seconds, `nil` means wait indefinitely
    # @yield the task to be performed within the lock.
    #
    # @return [Object] the result of the block operation.
    #
    # @raise [ArgumentError] when no block is given.
    # @raise [Concurrent::TimeoutError] when `timeout` elapses before the
    #   upgradeable read lock can be acquired.
    def with_upgradeable_read_lock(timeout = nil)
      raise ArgumentError.new('no block given') unless block_given?
      acquire_upgradeable_read_lock(timeout)
      begin
        yield
      ensure
        release_upgradeable_read_lock
      end
    end

    # Acquire a read lock. If a write lock is held by another thread, will block
    # until it is released.
    #
    # @param [Numeric, nil] timeout in seconds to wait, `nil` means wait
    #   indefinitely. On timeout the call raises and leaves both the global lock
    #   state and this thread's read depth exactly as they were before the call.
    #
    # @return [Boolean] true if the lock is successfully acquired
    #
    # @raise [Concurrent::TimeoutError] if `timeout` elapses before the lock is
    #   acquired
    # @raise [Concurrent::ResourceLimitError] if the maximum number of readers
    #   or per-thread reentrant acquires is exceeded.
    def acquire_read_lock(timeout = nil)
      ns_acquire_read_lock(timeout)
    end

    # Try to acquire a read lock and return true if we succeed. If it cannot be
    # acquired immediately, return false.
    #
    # @return [Boolean] true if the lock is successfully acquired
    #
    # @raise [Concurrent::ResourceLimitError] if the maximum number of per-thread
    #   reentrant acquires is exceeded.
    def try_read_lock
      if (held = @HeldCount.value) > 0
        raise ResourceLimitError.new('Too many reader holds on this thread') if (held & READ_LOCK_MASK) == READ_LOCK_MASK

        # Reentrant (or write-lock-held) path: never blocks; the helper looks
        # after the global reader unit.
        register_read_hold
        @HeldCount.value = held + 1
        return true
      else
        c = @Counter.value
        if !waiting_or_running_writer?(c) && @Counter.compare_and_set(c, c+1)
          @ReaderUnitInCounter.value = true
          @ReadHolds.value = 1
          @HeldCount.value = held + 1
          return true
        end
      end
      false
    end

    # Release a previously acquired read lock.
    #
    # @return [Boolean] true if the lock is successfully released
    def release_read_lock
      held = @HeldCount.value = @HeldCount.value - 1
      rlocks_held = held & READ_LOCK_MASK
      if rlocks_held == 0
        # Last read hold of this thread. unregister_last_read_hold removes the
        # global reader unit if it was in the counter; a unit merged into the
        # running writer is skipped here and accounted for by release_write_lock.
        c = unregister_last_read_hold

        # If one or more writers were waiting, wake the write queue up. When no
        # reader units remain at all a single signal is enough, exactly as before.
        # When reader units remain, however, one of them may belong to a reader
        # that is itself waiting on this same queue to upgrade to the write lock:
        # a plain #signal could land on that upgrader, and the last plain reader
        # would then release without anyone left to signal. Broadcasting lets the
        # single eligible waiter claim the lock and the rest go back to waiting.
        if waiting_or_running_writer?(c)
          if running_readers(c) == 0
            @WriteQueue.signal
          else
            @WriteQueue.broadcast
          end
        end
      elsif rlocks_held == READ_LOCK_MASK
        raise IllegalOperationError, "Cannot release a read lock which is not held"
      else
        @ReadHolds.value = @ReadHolds.value - 1
      end
      true
    end

    # Acquire a write lock. Will block and wait for all active readers and writers.
    #
    # @param [Numeric, nil] timeout in seconds to wait, `nil` means wait
    #   indefinitely. On timeout the call raises after undoing every reservation
    #   it made itself; neither this thread's read depth nor any other thread's
    #   permits are affected.
    #
    # @return [Boolean] true if the lock is successfully acquired
    #
    # @raise [Concurrent::TimeoutError] if `timeout` elapses before the lock is
    #   acquired
    # @raise [Concurrent::ResourceLimitError] if the maximum number of writers
    #   is exceeded.
    def acquire_write_lock(timeout = nil)
      ns_acquire_write_lock(timeout)
    end

    # Try to acquire a write lock and return true if we succeed. If it cannot be
    # acquired immediately, return false.
    #
    # @return [Boolean] true if the lock is successfully acquired
    def try_write_lock
      if (held = @HeldCount.value) >= WRITE_LOCK_HELD
        @HeldCount.value = held + WRITE_LOCK_HELD
        return true
      else
        c = @Counter.value
        # One reader unit per thread holding any read lock, independent of how
        # deeply reentered that thread's read lock is.
        reader_units = (held & READ_LOCK_MASK) > 0 ? 1 : 0
        # The guard compares our single reader unit against the global reader
        # count, so another reading thread always blocks us however deeply
        # reentered our own read lock is.
        if !waiting_or_running_writer?(c) &&
           running_readers(c) == reader_units &&
           @Counter.compare_and_set(c, c + RUNNING_WRITER)
          # Add RUNNING_WRITER on top of the reader units we already own instead of
          # converting them; the reader region stays occupied while both locks are
          # held and release_read_lock removes the unit afterwards.
           @HeldCount.value = held + WRITE_LOCK_HELD
          return true
        end
      end
      false
    end

    # Release a previously acquired write lock.
    #
    # @return [Boolean] true if the lock is successfully released
    def release_write_lock
      held = @HeldCount.value = @HeldCount.value - WRITE_LOCK_HELD
      wlocks_held = held & WRITE_LOCK_MASK
      if wlocks_held == 0
        # The write lock only ever added RUNNING_WRITER; an upgrading reader's
        # reader unit was left in the reader region and is independently removed by
        # release_read_lock. So releasing the write lock just removes its bit.
        c = @Counter.update { |counter| counter - RUNNING_WRITER }
        @ReadQueue.broadcast
        @WriteQueue.signal if waiting_writers(c) > 0
      elsif wlocks_held == WRITE_LOCK_MASK
        raise IllegalOperationError, "Cannot release a write lock which is not held"
      end
      true
    end

    # Acquire an upgradeable read lock.
    #
    # An upgradeable read lock counts as an ordinary read lock for the current
    # thread, but the upgrade slot is exclusive: at most one thread at a time can
    # hold one. Only the thread holding the upgrade slot is later allowed to call
    # {#upgrade_read_lock}, which is what makes read-to-write upgrade deadlock
    # free. Other threads requesting the slot wait on the upgrade queue (or fail
    # immediately via {#try_acquire_upgradeable_read_lock}), rather than keeping a
    # read lock while competing for the write lock.
    #
    # @param [Numeric, nil] timeout in seconds to wait for the upgrade slot,
    #   `nil` means wait indefinitely. On timeout the call raises without having
    #   acquired anything: the thread's read depth and the global counter are left
    #   exactly as they were before the call.
    #
    # @return [Boolean] true if the lock is successfully acquired
    #
    # @raise [Concurrent::TimeoutError] if `timeout` elapses before the upgrade
    #   slot becomes available
    # @raise [Concurrent::ResourceLimitError] if the maximum number of readers
    #   or per-thread reentrant acquires is exceeded.
    def acquire_upgradeable_read_lock(timeout = nil)
      # Re-entrant acquisition by the thread that already owns the upgrade slot.
      # This is just an additional ordinary read lock acquisition, exactly like a
      # nested acquire_read_lock call, plus one more upgradeable hold.
      if (upgrade_held = upgrade_count) > 0
        acquire_read_lock
        @UpgradeCount.value = upgrade_held + 1
        return true
      end

      deadline = timeout ? Concurrent.monotonic_time + timeout : nil

      # Become the single upgrade candidate. Only one thread can pass this point
      # at a time; every later reader parks on @UpgradeQueue instead of holding a
      # read lock and competing for the write lock.
      @UpgradeQueue.synchronize do
        if deadline
          remaining = deadline - Concurrent.monotonic_time
          if remaining <= 0 || !@UpgradeQueue.ns_wait_until(remaining) { @UpgradeHolder.nil? }
            raise TimeoutError, 'Timed out waiting to acquire the upgradeable read lock'
          end
        else
          @UpgradeQueue.ns_wait until @UpgradeHolder.nil?
        end
        @UpgradeHolder = Thread.current
      end

      begin
        # The slot is ours; take the actual read lock (blocking on real writers).
        # If this raises (timeout, ResourceLimitError, ...) we have not changed
        # the read depth yet, so we only have to hand the empty slot back.
        remaining = deadline ? deadline - Concurrent.monotonic_time : nil
        acquire_read_lock(remaining)
      rescue Exception
        release_upgrade_slot
        raise
      end

      @UpgradeCount.value = upgrade_count + 1
      true
    end

    # Try to acquire an upgradeable read lock and return true if we succeed.
    # If the single upgrade slot is already owned by another thread, or the
    # underlying read lock cannot be acquired immediately, return false without
    # blocking and without changing any lock state.
    #
    # @return [Boolean] true if the lock is successfully acquired
    def try_acquire_upgradeable_read_lock
      return try_acquire_reentrant_upgradeable_read_lock if upgrade_count > 0

      acquired_slot = @UpgradeQueue.synchronize do
        if @UpgradeHolder.nil?
          @UpgradeHolder = Thread.current
          true
        end
      end
      return false unless acquired_slot

      begin
        unless try_read_lock
          release_upgrade_slot
          return false
        end
      rescue Exception
        release_upgrade_slot
        raise
      end

      @UpgradeCount.value = upgrade_count + 1
      true
    end

    # Release a previously acquired upgradeable read lock.
    #
    # This releases one ordinary read lock and, when the last upgradeable hold of
    # the current thread is released, hands the exclusive upgrade slot back and
    # wakes the next thread waiting on the upgrade queue.
    #
    # @return [Boolean] true if the lock is successfully released
    #
    # @raise [Concurrent::IllegalOperationError] if the current thread does not
    #   hold an upgradeable read lock
    def release_upgradeable_read_lock
      if upgrade_count == 0
        raise IllegalOperationError, 'Cannot release an upgradeable read lock which is not held'
      end

      # Always release the read permit first, so that by the time another thread
      # can become the upgrade candidate every read permit we owned is gone.
      release_read_lock

      new_count = upgrade_count - 1
      @UpgradeCount.value = new_count == 0 ? nil : new_count
      release_upgrade_slot if new_count == 0
      true
    end

    # Upgrade the upgradeable read lock held by the current thread into a write
    # lock.
    #
    # The current thread must own the upgrade slot, otherwise the call fails
    # immediately instead of blocking while holding a plain read lock (which is
    # precisely the situation that would deadlock two competing readers). Because
    # the upgrade slot is exclusive, when this method waits for the write lock the
    # current thread is the only reader allowed to do so; all other readers hold
    # plain read locks and will release them, so the upgrade can always complete.
    #
    # On success the read lock remains held alongside the write lock, so the
    # caller can either {#release_read_lock} to end the read side or
    # {#downgrade_write_lock} to give the write side back.
    #
    # On timeout (only possible when an explicit `timeout` is given) the write
    # attempt fails and every reservation it made is undone, but the upgradeable
    # read lock is retained with exactly the same read depth and the same upgrade
    # slot the caller had before the call. No permit belonging to another thread is
    # touched; the caller may retry the upgrade or release the read lock normally.
    #
    # @param [Numeric, nil] timeout in seconds to wait for the write lock, `nil`
    #   means wait indefinitely
    #
    # @return [Boolean] true if the write lock is successfully acquired
    #
    # @raise [Concurrent::IllegalOperationError] if the current thread does not
    #   hold an upgradeable read lock, or already holds the write lock
    # @raise [Concurrent::TimeoutError] if `timeout` elapses before the write lock
    #   is acquired
    def upgrade_read_lock(timeout = nil)
      if upgrade_count == 0
        raise IllegalOperationError, 'Only the thread holding the upgradeable read lock may upgrade to the write lock'
      elsif @HeldCount.value >= WRITE_LOCK_HELD
        raise IllegalOperationError, 'Cannot upgrade to a write lock which is already held'
      end

      # Go through the public timed entry point (rather than the private helper)
      # so subclasses that wrap write-lock acquisition - e.g. for tracking - keep
      # observing upgrades.
      acquire_write_lock(timeout)
    end

    # Try to upgrade the upgradeable read lock into a write lock without blocking.
    #
    # @return [Boolean] true if the write lock is successfully acquired, false if
    #   it cannot be acquired immediately (the upgradeable read lock is retained)
    #
    # @raise [Concurrent::IllegalOperationError] if the current thread does not
    #   hold an upgradeable read lock, or already holds the write lock
    def try_upgrade_read_lock
      if upgrade_count == 0
        raise IllegalOperationError, 'Only the thread holding the upgradeable read lock may upgrade to the write lock'
      elsif @HeldCount.value >= WRITE_LOCK_HELD
        raise IllegalOperationError, 'Cannot upgrade to a write lock which is already held'
      end

      try_write_lock
    end

    # Downgrade the write lock held by the current thread to a read lock.
    #
    # The transition always happens in the same fixed order so the downgrading
    # thread never passes through an unlocked state where another writer could
    # sneak in:
    # * a plain writer (one that did not hold an upgradeable read lock) first
    #   acquires the read lock while still holding the write lock, and only then
    #   releases the write lock;
    # * an upgrading reader already holds the read lock (its reader unit stays in
    #   the counter the whole time), so it just releases the write lock; the
    #   original read depth is preserved exactly.
    #
    # Either way the thread ends up holding the write lock no longer and exactly
    # one read lock (plus any reentrant read holds it already had).
    #
    # @return [Boolean] true if the write lock is successfully downgraded
    #
    # @raise [Concurrent::IllegalOperationError] if the current thread does not
    #   hold the write lock
    def downgrade_write_lock
      if (@HeldCount.value & WRITE_LOCK_MASK) == 0
        raise IllegalOperationError, 'Cannot downgrade a write lock which is not held'
      end

      if upgrade_count > 0
        # The read lock already exists and its reader unit is already in the
        # counter, so releasing the write lock leaves the read depth unchanged.
        release_write_lock
      else
        # Take the read lock before giving the write lock away so there is no gap.
        # acquire_read_lock never blocks here: we already own the write lock and
        # its fast path does not add a second reader unit.
        acquire_read_lock
        release_write_lock
      end
      true
    end

    # Queries if the write lock is held by any thread.
    #
    # @return [Boolean] true if the write lock is held else false
    def write_locked?
      @Counter.value >= RUNNING_WRITER
    end

    # Queries if at least one read lock is currently held by any thread.
    #
    # @return [Boolean] true if one or more read locks are held else false
    def read_locked?
      running_readers?
    end

    # Queries whether any threads are waiting to acquire the read, write or
    # upgradeable read lock.
    #
    # @return [Boolean] true if any threads are waiting for a lock else false
    def has_waiters?
      waiting_writers(@Counter.value) > 0 ||
        @UpgradeQueue.synchronize { !@UpgradeHolder.nil? && @UpgradeHolder != Thread.current }
    end

    private

    # @!visibility private
    # How many upgradeable read locks the current thread holds (0 if none).
    def upgrade_count
      @UpgradeCount.value || 0
    end

    # @!visibility private
    # Gain a read hold. On the first hold the thread's single reader unit is
    # added to the global counter; later (reentrant) holds only change the
    # per-thread depth. A reader unit always stays in the counter, including while
    # the same thread holds the write lock after an upgrade.
    def register_read_hold
      if @ReadHolds.value == 0 && !@ReaderUnitInCounter.value
        @Counter.update { |c| c + 1 }
        @ReaderUnitInCounter.value = true
      end
      @ReadHolds.value = @ReadHolds.value + 1
    end

    # @!visibility private
    # Drop the last read hold and remove the thread's reader unit from the global
    # counter. Returns the resulting counter value.
    def unregister_last_read_hold
      @ReaderUnitInCounter.value = nil
      c = @Counter.update { |counter| counter - 1 }
      @ReadHolds.value = 0
      c
    end

    # @!visibility private
    # Hand the exclusive upgrade slot back and wake one queued candidate.
    # Only ever called by the thread that currently owns the slot.
    def release_upgrade_slot
      @UpgradeQueue.synchronize do
        @UpgradeHolder = nil
        @UpgradeQueue.ns_signal
      end
    end

    # @!visibility private
    # Re-entrant path shared by the blocking and non-blocking entry points.
    def try_acquire_reentrant_upgradeable_read_lock
      return false unless try_read_lock
      @UpgradeCount.value = upgrade_count + 1
      true
    end

    # @!visibility private
    # Timeout-aware implementation backing #acquire_read_lock. It is a copy of
    # the original waiting loop with explicit deadlines added; a timeout raises
    # before any state mutation, so the thread's read depth is always restored to
    # exactly what it was on entry.
    def ns_acquire_read_lock(timeout)
      deadline = timeout ? Concurrent.monotonic_time + timeout : nil

      if (held = @HeldCount.value) > 0
        raise ResourceLimitError.new('Too many reader holds on this thread') if (held & READ_LOCK_MASK) == READ_LOCK_MASK

        # If we already have a lock, there's no need to wait. register_read_hold
        # adds the global reader unit on the first read hold and leaves it in
        # place for every later (reentrant) hold.
        register_read_hold
        @HeldCount.value = held + 1
        return true
      end

      while true
        c = @Counter.value
        raise ResourceLimitError.new('Too many reader threads') if max_readers?(c)

        # If a writer is waiting OR running when we first queue up, we need to wait
        if waiting_or_running_writer?(c)
          # Before going to sleep, check again with the ReadQueue mutex held
          @ReadQueue.synchronize do
            if waiting_or_running_writer?
              raise TimeoutError, 'Timed out waiting to acquire the read lock' unless wait_on_queue(@ReadQueue, deadline)
            end
          end
          # Note: the above 'synchronize' block could have used #wait_until,
          #   but that waits repeatedly in a loop, checking the wait condition
          #   each time it wakes up (to protect against spurious wakeups)
          # But we are already in a loop, which is only broken when we successfully
          #   acquire the lock! So we don't care about spurious wakeups, and would
          #   rather not pay the extra overhead of using #wait_until

          # After a reader has waited once, they are allowed to "barge" ahead of waiting writers
          # But if a writer is *running*, the reader still needs to wait (naturally)
          while true
            c = @Counter.value
            if running_writer?(c)
              @ReadQueue.synchronize do
                if running_writer?
                  raise TimeoutError, 'Timed out waiting to acquire the read lock' unless wait_on_queue(@ReadQueue, deadline)
                end
              end
            elsif @Counter.compare_and_set(c, c+1)
              @ReaderUnitInCounter.value = true
              @ReadHolds.value = 1
              @HeldCount.value = held + 1
              return true
            end
          end
        elsif @Counter.compare_and_set(c, c+1)
          @ReaderUnitInCounter.value = true
          @ReadHolds.value = 1
          @HeldCount.value = held + 1
          return true
        end
      end
    end

    # @!visibility private
    # Timeout-aware implementation backing #acquire_write_lock. The only state
    # this method ever reserves on its own is a single WAITING_WRITER increment;
    # on timeout that exact increment is removed again (with a CAS retry loop) so
    # no other thread's permits, and none of this thread's existing read permits,
    # are touched.
    def ns_acquire_write_lock(timeout)
      deadline = timeout ? Concurrent.monotonic_time + timeout : nil

      if (held = @HeldCount.value) >= WRITE_LOCK_HELD
        # if we already have a write (exclusive) lock, there's no need to wait
        @HeldCount.value = held + WRITE_LOCK_HELD
        return true
      end

      # The global counter counts reading *threads*, one unit each, no matter how
      # many reentrant read holds this thread owns. `reader_units` is therefore 1
      # exactly when we hold any read lock and 0 otherwise; it decides whether the
      # single-reader fast path is available.
      reader_units = (held & READ_LOCK_MASK) > 0 ? 1 : 0

      while true
        c = @Counter.value
        raise ResourceLimitError.new('Too many writer threads') if max_writers?(c)

        # To go ahead and take the lock without waiting, there must be no writer
        #   running right now, AND no writers who came before us still waiting to
        #   acquire the lock
        # Additionally, if any read locks have been taken, we must hold all of them
        if reader_units > 0 && @Counter.compare_and_set(1, 1 + RUNNING_WRITER)
          # We are the only reader (the counter holds exactly our one reader unit).
          # We add RUNNING_WRITER *on top of* the existing reader unit: the reader
          # region stays non-zero for the whole time we hold both locks, while the
          # running-writer bit makes other readers/writers wait. The reader unit is
          # later removed by the regular release_read_lock.
          @HeldCount.value = held + WRITE_LOCK_HELD
          return true
        elsif @Counter.compare_and_set(c, c+WAITING_WRITER)
          while true
            # Now we have successfully incremented, so no more readers will be able to increment
            #   (they will wait instead)
            # However, readers OR writers could decrement right here
            timed_out = false
            @WriteQueue.synchronize do
              # So we have to do another check inside the synchronized section
              # If a writer OR another reader is running, then go to sleep. The
              # guard compares the single reader *unit* this thread owns (1 when
              # it holds any read lock, 0 otherwise) against the global reader
              # count, so another reading thread always keeps us waiting however
              # deeply reentered our own read lock is.
              c = @Counter.value
              if running_writer?(c) || running_readers(c) != reader_units
                timed_out = !wait_on_queue(@WriteQueue, deadline)
              end
            end
            if timed_out
              # Undo precisely the WAITING_WRITER reservation this call added and
              # then fail. Wake waiters because removing the barrier may let a
              # queued reader barge in. The reader unit and the upgrade slot are
              # deliberately left untouched: an upgrade is a *write* attempt made
              # on top of an upgradeable read lock the caller already owns, so a
              # failed/timed-out write attempt only gives back what the write
              # attempt itself reserved. The caller keeps its exact original read
              # depth and can retry the upgrade or release the read lock normally.
              rollback_waiting_writer
              raise TimeoutError, 'Timed out waiting to acquire the write lock'
            end
            # Note: if you are thinking of replacing the above 'synchronize' block
            # with #wait_until, read the comment in #acquire_read_lock first!

            # We just came out of a wait
            # If we successfully turn the RUNNING_WRITER bit on with an atomic swap,
            #   then we are OK to stop waiting and go ahead
            # Otherwise go back and wait again
            c = @Counter.value
            if !running_writer?(c) &&
               running_readers(c) == reader_units &&
               @Counter.compare_and_set(c, c+RUNNING_WRITER-WAITING_WRITER)
              # Only the WAITING_WRITER reservation is swapped for the running
              # writer; our reader unit stays in the reader region for the whole
              # time both locks are held and is removed by release_read_lock.
              @HeldCount.value = held + WRITE_LOCK_HELD
              return true
            end
          end
        end
      end
    end

    # @!visibility private
    # Wait once on the given queue, honoring an optional monotonic deadline.
    # Returns false (without waiting) once the deadline has passed; otherwise
    # performs one condition wait (indefinitely when there is no deadline) and
    # returns true. Spurious wake-ups are handled by the caller's surrounding
    # retry loop.
    def wait_on_queue(queue, deadline)
      if deadline.nil?
        queue.ns_wait
        return true
      end
      remaining = deadline - Concurrent.monotonic_time
      return false if remaining <= 0
      queue.ns_wait(remaining)
      true
    end

    # @!visibility private
    # Remove exactly one WAITING_WRITER reservation previously made by this
    # thread's own timed-out write-lock acquisition.
    def rollback_waiting_writer
      until (c = @Counter.value) && @Counter.compare_and_set(c, c - WAITING_WRITER)
        # the counter only changed in unrelated bits; the reservation is still
        # present and the same subtraction remains valid, so retry
      end
      @ReadQueue.broadcast
      @WriteQueue.signal if waiting_writers(@Counter.value) > 0
    end

    # @!visibility private
    def running_readers(c = @Counter.value)
      c & MAX_READERS
    end

    # @!visibility private
    def running_readers?(c = @Counter.value)
      (c & MAX_READERS) > 0
    end

    # @!visibility private
    def running_writer?(c = @Counter.value)
      c >= RUNNING_WRITER
    end

    # @!visibility private
    def waiting_writers(c = @Counter.value)
      (c & MAX_WRITERS) >> READER_BITS
    end

    # @!visibility private
    def waiting_or_running_writer?(c = @Counter.value)
      c >= WAITING_WRITER
    end

    # @!visibility private
    def max_readers?(c = @Counter.value)
      (c & MAX_READERS) == MAX_READERS
    end

    # @!visibility private
    def max_writers?(c = @Counter.value)
      (c & MAX_WRITERS) == MAX_WRITERS
    end
  end
end
