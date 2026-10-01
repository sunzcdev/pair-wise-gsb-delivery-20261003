require 'concurrent/atomic/count_down_latch'
require 'concurrent/atomic/cyclic_barrier'
require 'concurrent/executor/work_stealing_executor'

module Concurrent

  RSpec.describe WorkStealingExecutor, :type => :mri do
    # Small, fixed thread counts by mandate: correctness with barriers, not
    # throughput benchmarks.

    after(:each) do
      # Every example owns its pool and must be able to shut it down. A pool
      # that cannot terminate within the timeout fails the example.
      if @pool
        @pool.kill unless @pool.shutdown?
        expect(@pool.wait_for_termination(pool_termination_timeout)).to eq true
      end
    end

    def make_pool(threads: 2, **opts)
      @pool = WorkStealingExecutor.new({ max_threads: threads }.merge(opts))
    end

    def worker_id_for(thread)
      @pool.send(:worker_for, thread)&.id
    end

    # ------------------------------------------------------------------ deque

    describe WorkStealingDeque do
      let(:deque) { WorkStealingDeque.new }
      let(:job_class) { WorkStealingDeque::Job }

      def job(id)
        job_class.new(proc { }, [], id)
      end

      it 'is LIFO on the owner (local) end and FIFO on the steal end' do
        deque.push(job(1))
        deque.push(job(2))
        deque.push(job(3))

        # A thief takes the oldest first.
        expect(deque.pop_steal.id).to eq 1
        # The owner takes the youngest first.
        expect(deque.pop_local.id).to eq 3
        expect(deque.pop_steal.id).to eq 2
        expect(deque.pop_local).to eq nil
        expect(deque.pop_steal).to eq nil
      end

      it 'hands the last contended item to exactly one claimer' do
        deque.push(job(:only))
        results = []
        mutex = Mutex.new
        start = Concurrent::CountDownLatch.new(1)
        finish = Concurrent::CountDownLatch.new(8)

        threads = 8.times.map do
          Thread.new do
            start.wait
            100.times do
              deque.push(job(:x))
              # Aggressive mixed local/steal contention from many threads.
              got = rand(2).zero? ? deque.pop_local : deque.pop_steal
              mutex.synchronize { results << got.id } if got
            end
            finish.count_down
          end
        end
        start.count_down
        expect(finish.wait(10)).to eq true
        threads.each(&:join)

        # Every claim returned a real (never nil/duplicated) item: the initial
        # :only plus the 800 pushed :x give 801 pushes and 800 removals.
        expect(results).not_to include(nil)
        expect(results.size).to eq(8 * 100)
        expect(results.uniq.sort).to contain_exactly(:only, :x)
        # Exactly one item remains: the deque was pushed once more than popped.
        expect(deque.size).to eq 1
        last = deque.pop_local
        expect(last).not_to eq nil
        expect(deque.empty?).to eq true
      end

      it 'returns nil from both ends only when genuinely empty and never hands an item out twice' do
        jobs = Array.new(200) { |i| job(i) }
        jobs.each { |j| deque.push(j) }
        taken = Queue.new

        # 5 drainer threads keep claiming until the deque is fully empty.
        threads = 5.times.map do
          Thread.new do
            loop do
              item = rand(2).zero? ? deque.pop_local : deque.pop_steal
              break if item.nil?
              taken << item.id
            end
          end
        end
        threads.each(&:join)

        ids = []
        ids << taken.pop until taken.empty?
        # No item was handed out twice...
        expect(ids.size).to eq(ids.uniq.size)
        # ...and the union of claimed items is every pushed item exactly once.
        expect(ids.sort).to eq((0...200).to_a)
        expect(deque.pop_local).to eq nil
        expect(deque.pop_steal).to eq nil
      end
    end

    # ---------------------------------------------------------- construction

    describe 'construction' do
      it 'creates exactly the fixed number of threads' do
        pool = make_pool(threads: 3)
        wait_until = Concurrent.monotonic_time + 2
        sleep 0.05 until pool.length == 3 || Concurrent.monotonic_time > wait_until
        expect(pool.length).to eq 3
        expect(pool.max_length).to eq 3
        expect(pool.largest_length).to eq 3
      end

      it 'rejects zero or negative thread counts' do
        expect { WorkStealingExecutor.new(max_threads: 0) }.to raise_error(ArgumentError)
        expect { WorkStealingExecutor.new(max_threads: -2) }.to raise_error(ArgumentError)
      end

      it 'rejects unknown fallback policies' do
        expect { WorkStealingExecutor.new(fallback_policy: :bogus) }.to raise_error(ArgumentError)
      end
    end

    # --------------------------------------------------- submission protocol

    describe 'submission protocol compatibility' do
      it 'requires a block for #post' do
        pool = make_pool
        expect { pool.post }.to raise_error(ArgumentError)
        expect { pool.post(1, 2) }.to raise_error(ArgumentError)
      end

      it '#post returns true and #<< returns the pool' do
        pool = make_pool(threads: 2)
        latch = Concurrent::CountDownLatch.new(2)
        expect(pool.post { latch.count_down }).to eq true
        expect((pool << proc { latch.count_down }).equal?(pool)).to eq true
        expect(latch.wait(5)).to eq true
      end

      it 'passes arguments through to the task' do
        pool = make_pool
        latch = Concurrent::CountDownLatch.new(1)
        received = nil
        pool.post(1, :two, 'three') do |a, b, c|
          received = [a, b, c]
          latch.count_down
        end
        expect(latch.wait(5)).to eq true
        expect(received).to eq [1, :two, 'three']
      end

      it 'is not serialized and cannot overflow (unbounded per-worker deques)' do
        pool = make_pool
        expect(pool.serialized?).to eq false
        expect(pool.can_overflow?).to eq false
      end
    end

    # ------------------------------------------------- local vs steal claims

    describe 'claim routing' do
      it 'routes a task forked from inside a worker to the worker own deque (LIFO local claim)' do
        pool = make_pool(threads: 1)
        gate = Concurrent::CountDownLatch.new(1)
        finish = Concurrent::CountDownLatch.new(1)
        seen = Queue.new

        pool.post do
          pool.post(:c1) { |n| seen << [:c1, worker_id_for(Thread.current)] }
          pool.post(:c2) { |n| seen << [:c2, worker_id_for(Thread.current)] }
          gate.wait(5)
          finish.count_down
        end
        gate.count_down
        expect(finish.wait(5)).to eq true
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true

        claims = pool.worker_claims[0]
        # Both children were local claims of worker 0.
        expect(claims[:local]).to eq 2
        expect(claims[:external]).to eq 1
        expect(claims[:stolen]).to eq 0
      end

      it 'lets an idle worker steal not-yet-started forked work from a blocked worker' do
        pool = make_pool(threads: 2)
        root_inside = Concurrent::CyclicBarrier.new(2)
        root_gate = Concurrent::CountDownLatch.new(1)
        children_started = Concurrent::CountDownLatch.new(4)
        trace = []
        trace_mutex = Mutex.new

        # One external task occupies worker A and ONLY forks: it synchronizes
        # with the test while its four children already sit in A's deque, and
        # only afterwards does it block. By the time A blocks the idle peer B
        # has been parked through the whole fork burst, so B wakes on the
        # completion relay and steals the children; A is parked inside a gate
        # and cannot execute any of them.
        pool.post do
          4.times do |i|
            pool.post(i) do |n|
              trace_mutex.synchronize do
                trace << { id: n, worker: worker_id_for(Thread.current) }
              end
              children_started.count_down
            end
          end
          # Children are laid down; rendezvous with the test, then block.
          root_inside.wait(5)
          root_gate.wait(5)
        end

        # Wait until the fork burst is done (and the owner is about to block).
        expect(root_inside.wait(5)).to eq true
        # Small deterministic grace for the owner to enter its gate; once
        # blocked there it cannot claim its own deque any more.
        sleep 0.2
        expect(children_started.wait(5)).to eq true
        root_gate.count_down
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true

        root_worker = pool.worker_claims.find { |_, c| c[:external] == 1 }.first
        thief_worker = (0..1).find { |id| id != root_worker }

        expect(trace.map { |t| t[:id] }.sort).to eq [0, 1, 2, 3]
        expect(trace.map { |t| t[:worker] }.uniq).to eq [thief_worker]
        expect(pool.worker_claims[thief_worker][:stolen]).to eq 4
        expect(pool.worker_claims[root_worker][:stolen]).to eq 0
        expect(pool.worker_claims[root_worker][:local]).to eq 0
      end

      it 'executes every task exactly once under a forking storm' do
        pool = make_pool(threads: 4)
        executed_ids = []
        ids_mutex = Mutex.new
        # 20 roots, each forking 4 intermediates, each forking 4 leaves:
        # 20 + 80 + 320 = 420 tasks, 320 leaves.
        roots_done = Concurrent::CountDownLatch.new(20)
        mids_done = Concurrent::CountDownLatch.new(80)
        leaves_done = Concurrent::CountDownLatch.new(320)

        20.times do |root|
          pool.post(root) do |r|
            4.times do |a|
              pool.post([r, a]) do |ra|
                4.times do |b|
                  pool.post([ra, b]) do |rab|
                    ids_mutex.synchronize { executed_ids << rab }
                    leaves_done.count_down
                  end
                end
                mids_done.count_down
              end
            end
            roots_done.count_down
          end
        end

        expect(roots_done.wait(15)).to eq true
        expect(mids_done.wait(15)).to eq true
        expect(leaves_done.wait(15)).to eq true
        expect(executed_ids.size).to eq 320
        expect(executed_ids.uniq.size).to eq 320
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
        expect(pool.completed_task_count).to eq pool.scheduled_task_count
      end

      it 'records provenance for externally submitted work' do
        pool = make_pool(threads: 3)
        done = Concurrent::CountDownLatch.new(30)
        30.times { pool.post { done.count_down } }
        expect(done.wait(5)).to eq true
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true

        claims = pool.worker_claims
        # Nothing was forked, so no local and no stolen claims may exist.
        expect(claims.values.sum { |c| c[:local] }).to eq 0
        expect(claims.values.sum { |c| c[:stolen] }).to eq 0
        expect(claims.values.sum { |c| c[:external] }).to eq 30
      end
    end

    # ------------------------------------------------------------- task errors

    describe 'task failure handling' do
      it 'contains raised StandardErrors and keeps processing accepted work' do
        pool = make_pool(threads: 2)
        later = Concurrent::CountDownLatch.new(20)
        20.times { |i| pool.post { raise "boom #{i}" } }
        20.times { pool.post { later.count_down } }
        expect(later.wait(5)).to eq true
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
        expect(pool.completed_task_count).to eq 40
        expect(pool.length).to eq 2
      end

      it 'replaces a worker killed by an Exception without losing queued work' do
        pool = make_pool(threads: 2)
        blocker_gate = Concurrent::CountDownLatch.new(1)
        survivor_done = Concurrent::CountDownLatch.new(5)
        ready = Concurrent::CountDownLatch.new(1)

        pool.post { ready.count_down; blocker_gate.wait(5) }
        expect(ready.wait(5)).to eq true
        # Keep the queue shallow: one fatal task and five follow-ups only.
        fatal = Class.new(Exception)
        pool.post { raise fatal, 'fatal task' }
        5.times { pool.post { survivor_done.count_down } }
        expect(survivor_done.wait(10)).to eq true
        blocker_gate.count_down
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
        # The replacement kept the fixed pool size.
        expect(pool.length).to eq 2
      end
    end

    # --------------------------------------------------------------- shutdown

    describe 'shutdown semantics' do
      it 'drains every accepted task, including children forked during draining' do
        pool = make_pool(threads: 3)
        leaves = Concurrent::AtomicFixnum.new(0)

        25.times do
          pool.post do
            3.times do
              pool.post { leaves.increment }
            end
          end
        end
        pool.shutdown
        expect(pool.running?).to eq false
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
        expect(pool.shutdown?).to eq true
        expect(leaves.value).to eq 75
        expect(pool.completed_task_count).to eq pool.scheduled_task_count
        expect(pool.queue_length).to eq 0
      end

      it 'interleaves shutdown with concurrent submissions without losing accepted work' do
        pool = make_pool(threads: 4, fallback_policy: :discard)
        observed = Concurrent::AtomicFixnum.new(0)
        stop = false

        submitter = Thread.new do
          i = 0
          loop do
            break if stop
            pool.post(i) { observed.increment }
            i += 1
          end
        end

        sleep 0.2
        pool.shutdown
        stop = true
        submitter.join
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
        # Accepted == executed: no accepted work was lost.
        expect(observed.value).to eq pool.scheduled_task_count
      end

      it 'rejects new submissions after shutdown per fallback policy' do
        %i[abort discard caller_runs].each do |policy|
          pool = make_pool(threads: 1, fallback_policy: policy)
          pool.shutdown
          expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
          case policy
          when :abort
            expect { pool.post { } }.to raise_error(RejectedExecutionError)
          when :discard
            expect(pool.post { }).to eq false
          when :caller_runs
            ran = false
            pool.post { ran = true }
            expect(ran).to eq true
          end
        end
      end

      it 'kill dismisses queued work but allows in-flight tasks to finish' do
        pool = make_pool(threads: 2)
        gate = Concurrent::CountDownLatch.new(1)
        inflight = Concurrent::AtomicFixnum.new(0)
        dismissed = Concurrent::AtomicFixnum.new(0)
        ready = Concurrent::CountDownLatch.new(2)

        2.times do
          pool.post do
            inflight.increment
            ready.count_down
            gate.wait(5)
          end
        end
        expect(ready.wait(5)).to eq true
        50.times { pool.post { dismissed.increment } }
        pool.kill
        gate.count_down
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
        expect(inflight.value).to eq 2
        expect(dismissed.value).to eq 0
      end

      it 'wait_for_termination is false on timeout and true afterwards' do
        pool = make_pool(threads: 1)
        gate = Concurrent::CountDownLatch.new(1)
        pool.post { gate.wait(5) }
        sleep 0.1
        pool.shutdown
        expect(pool.wait_for_termination(0.05)).to eq false
        gate.count_down
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end
    end

    # --------------------------------------------------------- no busy waiting

    describe 'idle parking' do
      it 'parks idle threads (no busy-wait) and still reacts to late submissions' do
        pool = make_pool(threads: 3)
        sleep 0.3 # let all workers reach their park point

        threads = pool.instance_variable_get(:@workers).map(&:thread)
        expect(threads.map(&:status)).to all(eq('sleep'))

        latch = Concurrent::CountDownLatch.new(3)
        3.times { pool.post { latch.count_down } }
        expect(latch.wait(5)).to eq true
      end

      it 'terminates all worker threads after shutdown' do
        pool = make_pool(threads: 3)
        latch = Concurrent::CountDownLatch.new(5)
        5.times { pool.post { latch.count_down } }
        expect(latch.wait(5)).to eq true
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
        sleep 0.1
        threads = pool.instance_variable_get(:@workers).map(&:thread)
        expect(threads.map(&:status)).to all(eq false)
      end
    end

    # ---------------------------------------------------------- mixed stress

    describe 'mixed workload', :stress do
      it 'executes every accepted task exactly once across nested fan-outs and stealing' do
        pool = make_pool(threads: 4)
        counter = Concurrent::AtomicFixnum.new(0)
        # 250 roots, each forking 4 children, each forking one grandchild:
        # 250 + 1000 + 1000 = 2250 tasks; 1000 leaf results.
        leaf_done = Concurrent::CountDownLatch.new(1000)

        250.times do |r|
          pool.post(r) do |root|
            4.times do |a|
              pool.post([root, a]) do |branch|
                pool.post(branch) { counter.increment; leaf_done.count_down }
              end
            end
          end
        end

        expect(leaf_done.wait(60)).to eq true
        expect(counter.value).to eq 1000
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
        expect(pool.completed_task_count).to eq pool.scheduled_task_count
      end
    end
  end
end
