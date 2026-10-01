require 'securerandom'
require 'set'
require 'concurrent'
require 'concurrent/executor/work_stealing_executor'

module Concurrent

  RSpec.describe RubyWorkStealingExecutor, :type => :mri do
    subject(:pool) { described_class.new(num_threads, fallback_policy: :abort) }

    let(:num_threads) { 4 }

    after(:each) do
      pool.kill unless pool.shutdown?
      pool.wait_for_termination(pool_termination_timeout)
    end

    # All N workers park on a rendezvous so their identities are known. The
    # first worker to register becomes the feeder role; the other workers wait
    # on a gate which the feeder opens, enabling deterministic steal scenarios.
    def build_steal_rendezvous(threads)
      state = {
        feeder: nil, feeder_lock: Mutex.new,
        registered: CountDownLatch.new(threads),
        thief_gate: CountDownLatch.new(1)
      }
      threads.times do
        pool.post do
          wid = Thread.current[:concurrent_ws_pool].last
          state[:feeder_lock].synchronize { state[:feeder] ||= wid }
          state[:registered].count_down
          if state[:feeder] == wid
            yield :feeder, wid, state
          else
            state[:thief_gate].wait(10)
            yield :thief, wid, state
          end
        end
      end
      expect(state[:registered].wait(10)).to be true
      state
    end

    describe 'submission protocol compatibility' do
      it 'is an executor service supporting the usual lifecycle' do
        expect(pool).to be_a(AbstractExecutorService)
        expect(pool).to respond_to(:post, :shutdown, :kill, :wait_for_termination)
        expect(pool.length).to eq 4
        expect(pool.running?).to be true
        expect(pool.shuttingdown?).to be false
        expect(pool.shutdown?).to be false
        expect(pool.can_overflow?).to be false
        expect(pool.serialized?).to be false
      end

      it 'requires a block for #post' do
        expect { pool.post }.to raise_error(ArgumentError)
      end

      it '#<< returns self and runs the task' do
        latch = CountDownLatch.new(1)
        expect(pool << -> { latch.count_down }).to be pool
        expect(latch.wait(5)).to be true
      end

      it 'rejects new work after shutdown with the :abort policy' do
        pool.shutdown
        expect(pool.wait_for_termination(5)).to be true
        # #post without a running executor applies the fallback policy outside
        # the lock: discard-style returns false, abort raises
        expect { pool.post { nil } }.to raise_error(RejectedExecutionError)
      end

      it ':discard policy drops the task after shutdown' do
        p = described_class.new(2, fallback_policy: :discard)
        p.shutdown
        expect(p.wait_for_termination(5)).to be true
        expect(p.post { nil }).to be false
      end

      it ':caller_runs policy executes on the calling thread after shutdown' do
        p = described_class.new(2, fallback_policy: :caller_runs)
        p.shutdown
        expect(p.wait_for_termination(5)).to be true
        ran_on = nil
        expect(p.post { ran_on = Thread.current }).to be true
        expect(ran_on).to eq Thread.current
      end

      it 'validates the number of threads and the fallback policy' do
        expect { described_class.new(0) }.to raise_error(ArgumentError)
        expect { described_class.new(2, fallback_policy: :bogus) }.
          to raise_error(ArgumentError)
      end
    end

    describe 'routing: external entry versus worker-local entry' do
      it 'marks work submitted from outside as :external' do
        executed = CountDownLatch.new(1)
        pool.post { executed.count_down }
        expect(executed.wait(5)).to be true

        pool.shutdown
        expect(pool.wait_for_termination(5)).to be true
        expect(pool.acquisition_sources.flatten).to include(:external)
      end

      it 'routes a nested post onto the local deque of the running worker' do
        parent_worker_id = Queue.new
        nested_worker_id = Queue.new

        pool.post do
          parent_worker_id << Thread.current[:concurrent_ws_pool].last
          pool.post { nested_worker_id << Thread.current[:concurrent_ws_pool].last }
        end

        pid = parent_worker_id.pop
        expect(nested_worker_id.pop).to eq pid

        pool.shutdown
        expect(pool.wait_for_termination(5)).to be true
        expect(pool.acquisition_sources[pid]).to include(:local)
      end

      it 'posts from a worker targeting another executor use that inbox' do
        other = described_class.new(2)
        nested_ran = CountDownLatch.new(1)
        pool.post { other.post { nested_ran.count_down } }
        expect(nested_ran.wait(5)).to be true
        other.shutdown
        expect(other.wait_for_termination(5)).to be true
      end
    end

    describe 'local takes and cross-thread steals' do
      # The feeder posts children onto its deque, opens the gate and then
      # blocks until every child has run, executing none itself. Every child
      # is therefore provably taken by cross-thread stealing from the back
      # (oldest end).
      it 'cross-thread steals every forked item from the oldest end' do
        n_children = 120
        drained    = CountDownLatch.new(n_children)
        record     = Mutex.new
        executions = [] # [child_index, worker_id]
        fid        = nil

        build_steal_rendezvous(num_threads) do |role, wid, st|
          if role == :feeder
            fid = wid
            n_children.times do |i|
              pool.post do
                record.synchronize { executions << [i, Thread.current[:concurrent_ws_pool].last] }
                drained.count_down
              end
            end
            st[:thief_gate].count_down
            drained.wait(10) # park: the owner runs none of the children
          end
        end

        expect(drained.wait(10)).to be true
        pool.shutdown
        expect(pool.wait_for_termination(5)).to be true

        # task identity: every child ran exactly once ...
        indices = executions.map(&:first).sort
        expect(indices).to eq (0...n_children).to_a
        # ... and none ran on the feeder worker
        expect(executions.map(&:last)).not_to include(fid)

        # provenance: the feeder itself never stole, and every child was
        # acquired as :steal somewhere (it may additionally have stolen the
        # parked parent tasks of other workers, so only check the lower bound)
        sources = pool.acquisition_sources
        expect(sources[fid]).not_to include(:steal)
        steal_total = sources.each_with_index.inject(0) do |total, (worker_sources, w)|
          total + (w == fid ? 0 : worker_sources.count(:steal))
        end
        expect(steal_total).to be >= n_children

        # thieves take from the back: each thief's observed indices are a
        # subsequence of the increasing submission order
        executions.group_by(&:last).each_value do |runs|
          seq = runs.map(&:first)
          expect(seq).to eq seq.sort
        end
      end

      it 'pops the local front (LIFO) and the stealable back (FIFO)' do
        deque = described_class.const_get(:Deque).new
        deque << :a
        deque << :b
        deque << :c
        expect(deque.pop_back).to eq  :a   # thief end takes oldest first
        expect(deque.pop_front).to eq :c   # owner end takes newest first
        expect(deque.pop_front).to eq :b
        expect(deque.pop_back).to be_nil
        expect(deque.pop_front).to be_nil
      end

      it 'spreads forked work across multiple workers' do
        # The children synchronize on a barrier of (N-1) parties: the feeder
        # stays parked on its own task, so only the three thieves execute. Each
        # barrier round can be completed only by three distinct worker threads,
        # which deterministically proves the forked work is stolen by several
        # workers instead of being hoarded by one.
        parties    = num_threads - 1
        rounds     = 20
        n_children = parties * rounds
        drained    = CountDownLatch.new(n_children)
        per_round  = Concurrent::CyclicBarrier.new(parties)
        actors     = Mutex.new
        worker_ids = Set.new

        build_steal_rendezvous(num_threads) do |role, _wid, st|
          if role == :feeder
            n_children.times do
              pool.post do
                # blocks until +parties+ different workers run children together
                expect(per_round.wait(5)).to be true
                actors.synchronize { worker_ids << Thread.current[:concurrent_ws_pool].last }
                drained.count_down
              end
            end
            st[:thief_gate].count_down
            sleep 0.05 until pool.shutdown? || drained.count.zero?
          end
        end

        expect(drained.wait(10)).to be true
        pool.shutdown
        expect(pool.wait_for_termination(5)).to be true

        # all (N-1) thieves were observed executing children
        expect(worker_ids.size).to eq parties
        thieves = pool.acquisition_sources.count { |s| s.include?(:steal) }
        expect(thieves).to eq parties
      end
    end

    describe 'exactly-once execution under contention' do
      it 'runs every externally queued task once when workers race to wake' do
        50.times do
          p = described_class.new(3)
          counts = Array.new(30, 0)
          lock   = Mutex.new
          parked = CountDownLatch.new(3)
          go     = CountDownLatch.new(1)
          finish = CountDownLatch.new(30)

          3.times { p.post { parked.count_down; go.wait(5) } }
          expect(parked.wait(5)).to be true

          30.times do |i|
            p.post do
              lock.synchronize { counts[i] += 1 }
              finish.count_down
            end
          end
          go.count_down # all workers wake and race for the inbox work

          expect(finish.wait(5)).to be true
          p.shutdown
          expect(p.wait_for_termination(5)).to be true
          expect(counts).to eq Array.new(30, 1)
        end
      end

      it 'never duplicates the final item of a deque raced by owner and thief' do
        100.times do
          p = described_class.new(2)
          run_count = Concurrent::AtomicFixnum.new(0)
          armed     = Queue.new
          done      = CountDownLatch.new(1)
          go        = Queue.new

          p.post do
            go.pop # park while the other worker is idle-sleeping
            p.post { run_count.increment; done.count_down }
            armed << :armed
            sleep 0.05
          end

          go << :go
          armed.pop
          expect(done.wait(5)).to be true
          sleep 0.05 # surface any (incorrect) duplicate execution
          expect(run_count.value).to eq 1
          p.shutdown
          expect(p.wait_for_termination(5)).to be true
        end
      end
    end

    describe 'task failures' do
      it 'contains raising tasks and keeps executing accepted work' do
        finished = CountDownLatch.new(2)
        pool.post { raise 'boom one' }
        pool.post { raise 'boom two' }
        pool.post { finished.count_down }
        pool.post { finished.count_down }
        expect(finished.wait(5)).to be true

        pool.shutdown
        expect(pool.wait_for_termination(5)).to be true
        expect(pool.completed_task_count).to eq 4
      end
    end

    describe 'shutdown interleaved with submissions and deque switching' do
      it 'accepts work queued before shutdown and loses none of it' do
        ran = Concurrent::Array.new
        200.times { |i| pool.post { ran << i } }
        pool.shutdown
        expect(pool.wait_for_termination(5)).to be true
        expect(ran.size).to eq 200
        expect(ran.sort).to eq (0...200).to_a
        expect(pool.queue_length).to eq 0
        expect(pool.scheduled_task_count).to eq pool.completed_task_count
      end

      it 'drains forked work already accepted when shutdown starts' do
        n_children = 40
        results    = Concurrent::Array.new
        finish     = CountDownLatch.new(n_children)
        forked     = CountDownLatch.new(1)

        pool.post do
          # fork all children (accepting them onto the local deque), then
          # block until shutdown has been requested
          n_children.times { |i| pool.post { results << i; finish.count_down } }
          forked.count_down
          sleep 0.01 until pool.shuttingdown?
        end

        expect(forked.wait(5)).to be true
        pool.shutdown
        expect(finish.wait(5)).to be true
        expect(pool.wait_for_termination(5)).to be true
        expect(results.size).to eq n_children
        expect(results.sort).to eq (0...n_children).to_a
        expect(pool.scheduled_task_count).to eq pool.completed_task_count
      end

      it 'refuses work posted only after shutdown has started' do
        entered = CountDownLatch.new(1)
        proceed = CountDownLatch.new(1)
        rejected = Concurrent::AtomicBoolean.new(false)
        pool.post do
          entered.count_down
          proceed.wait(5)
          # pool is shutting down here: with the :abort policy the nested post
          # raises instead of being silently accepted (the error is contained
          # by the worker like any other task error)
          begin
            pool.post { nil }
          rescue RejectedExecutionError
            rejected.make_true
          end
        end

        expect(entered.wait(5)).to be true
        pool.shutdown
        proceed.count_down
        expect(pool.wait_for_termination(5)).to be true
        expect(rejected.true?).to be true
      end

      it 'shutdown is idempotent and the state transitions are clean' do
        expect(pool.shutdown).to be true
        expect(pool.shutdown).to be true
        expect(pool.wait_for_termination(5)).to be true
        expect(pool.running?).to be false
        expect(pool.shutdown?).to be true
      end

      it 'terminates an empty pool immediately on shutdown' do
        p = described_class.new(3)
        p.shutdown
        expect(p.wait_for_termination(1)).to be true
      end

      it 'keeps waiting for termination while a task is still in flight' do
        task_started = CountDownLatch.new(1)
        finish_task  = CountDownLatch.new(1)
        pool.post do
          task_started.count_down
          finish_task.wait(10)
        end
        expect(task_started.wait(5)).to be true
        pool.shutdown
        # still in flight: termination must not be reported yet
        expect(pool.wait_for_termination(0.2)).to be false
        expect(pool.shuttingdown?).to be true
        finish_task.count_down
        expect(pool.wait_for_termination(5)).to be true
      end

      it 'waits for tasks draining from one deque via stealing before stopping' do
        # park N-1 workers and the feeder, after the feeder accepted many local
        # children; shutdown while parked must still drain all children
        n_children = 30
        all_children = CountDownLatch.new(n_children)
        build_steal_rendezvous(num_threads) do |role, _wid, st|
          if role == :feeder
            n_children.times { pool.post { all_children.count_down } }
            st[:thief_gate].count_down
            pool.shutdown # request shutdown while work is pending
            all_children.wait(10)
          end
        end
        expect(all_children.wait(10)).to be true
        expect(pool.wait_for_termination(5)).to be true
        expect(pool.completed_task_count).to eq(n_children + num_threads)
      end

      it 'kill dismisses queued work and terminates promptly' do
        p = described_class.new(3)
        50.times { p.post { sleep 5 } }
        # let at most a few tasks start, then kill while the rest is queued
        sleep 0.2
        p.kill
        expect(p.wait_for_termination(1)).to be true
        expect(p.queue_length).to eq 0
        expect(p.shutdown?).to be true
      end

      it 'keeps counting completed raising tasks toward the drain condition' do
        done = CountDownLatch.new(1)
        pool.post { raise 'fails but still counts as completed' }
        pool.post { done.count_down }
        pool.shutdown
        expect(done.wait(5)).to be true
        expect(pool.wait_for_termination(5)).to be true
      end
    end

    describe 'idle workers do not busy-wait' do
      it 'parks worker threads on the condition variable' do
        pool # eagerly construct the subject
        repeat_until_success { expect(pool.worker_statuses).to all(eq('sleep')) }

        latch = CountDownLatch.new(1)
        pool.post { latch.count_down }
        expect(latch.wait(5)).to be true
        repeat_until_success { expect(pool.worker_statuses).to all(eq('sleep')) }
      end

      it 'reuses parked workers and releases them all on shutdown' do
        pool # eagerly construct the subject
        repeat_until_success do
          expect(pool.idle_wait_counts.inject(0, :+)).to be >= num_threads
        end

        latch = CountDownLatch.new(3)
        3.times { pool.post { latch.count_down } }
        expect(latch.wait(5)).to be true

        pool.shutdown
        expect(pool.wait_for_termination(5)).to be true
        repeat_until_success { expect(pool.worker_statuses).to all(eq(false)) }
      end
    end

    describe 'recursive divide-and-conquer workload' do
      it 'completes a forking tree with no lost or duplicated leaves' do
        leaves     = Concurrent::Array.new
        leaf_lock  = Mutex.new
        depth      = 6
        n_leaves   = 2 ** depth

        split = lambda do |level|
          if level.zero?
            leaf_lock.synchronize { leaves << SecureRandom.uuid }
          else
            2.times { pool.post { split.call(level - 1) } }
          end
        end

        pool.post { split.call(depth) }

        deadline = Concurrent.monotonic_time + 5
        sleep 0.05 until leaves.size >= n_leaves || Concurrent.monotonic_time > deadline
        expect(leaves.size).to eq n_leaves
        expect(leaves.uniq.size).to eq n_leaves

        pool.shutdown
        expect(pool.wait_for_termination(5)).to be true
        expect(pool.scheduled_task_count).to eq pool.completed_task_count
      end
    end

    describe 'heavy contention (stress)', :stress do
      it 'never loses or duplicates a task with mixed local/external posts' do
        p          = described_class.new(8)
        n_external = 5_000
        n_leaves   = 4_096
        ext_counts = Array.new(n_external, 0)
        leaf_ids   = Concurrent::Array.new
        leaf_lock  = Mutex.new
        ext_finish = CountDownLatch.new(n_external)
        tree_finish = CountDownLatch.new(n_leaves)

        # many external producers posting concurrently
        ext_threads = 8.times.map do |t|
          Thread.new do
            Thread.abort_on_exception = true
            n_external.times do |i|
              next unless (i % 8) == t
              p.post do
                # increments without the GVL protecting read-modify-write would
                # race, so guard it; the guard is not the executor's queue
                ext_counts[i] += 1
                ext_finish.count_down
              end
            end
          end
        end

        # concurrently a deep forking tree is running on the pool
        split = lambda do |level|
          if level.zero?
            leaf_lock.synchronize { leaf_ids << SecureRandom.uuid }
            tree_finish.count_down
          else
            2.times { p.post { split.call(level - 1) } }
          end
        end
        p.post { split.call(12) }

        ext_threads.each { |t| t.join(15) }
        expect(ext_finish.wait(20)).to be true
        expect(tree_finish.wait(20)).to be true
        p.shutdown
        expect(p.wait_for_termination(10)).to be true

        expect(ext_counts).to eq Array.new(n_external, 1)
        expect(leaf_ids.size).to eq n_leaves
        expect(leaf_ids.uniq.size).to eq n_leaves
        expect(p.scheduled_task_count).to eq p.completed_task_count
      end

      it 'survives shutdown racing continuous submissions without losing accepted work' do
        50.times do
          p = described_class.new(4)
          accepted = Concurrent::AtomicFixnum.new(0)
          finished = Concurrent::AtomicFixnum.new(0)
          stop     = Concurrent::AtomicBoolean.new(false)

          producers = 4.times.map do
            Thread.new do
              Thread.abort_on_exception = true
              until stop.true?
                begin
                  if p.post { finished.increment }
                    accepted.increment
                  end
                rescue RejectedExecutionError
                  # pool shutting down, expected
                end
              end
            end
          end

          sleep 0.05
          p.shutdown
          expect(p.wait_for_termination(10)).to be true
          stop.make_true
          producers.each { |t| t.join(5) }

          # every task that returned true from post ran exactly once
          expect(finished.value).to eq accepted.value
        end
      end
    end
  end
end
