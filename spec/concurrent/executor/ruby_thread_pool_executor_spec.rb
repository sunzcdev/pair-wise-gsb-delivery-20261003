require_relative 'thread_pool_executor_shared'
require 'concurrent/executor/thread_pool_executor'

module Concurrent

  RSpec.describe RubyThreadPoolExecutor, :type=>:mri do

    after(:each) do
      subject.shutdown
      expect(subject.wait_for_termination(pool_termination_timeout)).to eq true
    end

    subject do
      RubyThreadPoolExecutor.new(
        min_threads: 2,
        max_threads: 5,
        idletime: 60,
        max_queue: 10,
        fallback_policy: :discard
      )
    end

    it_should_behave_like :thread_pool

    it_should_behave_like :thread_pool_executor

    context :prune, if: !Concurrent.on_jruby? do # pruning is flaky on JRuby
      subject do
        RubyThreadPoolExecutor.new(idletime: 5, min_threads: 2, max_threads: 10)
      end

      Group = Struct.new :waiting_threads, :threads, :mutex, :cond

      def prepare_thread_group(size)
        cond = ConditionVariable.new
        mutex = Mutex.new
        threads = []
        size.times do
          subject.post do
            mutex.synchronize do
              threads << Thread.current
              cond.wait(mutex)
              threads.delete(Thread.current)
            end
          end
        end
        eventually(mutex: mutex) { expect(threads).to have_attributes(size: size) }
        Group.new(threads, threads.dup, mutex, cond)
      end

      def wakeup_thread_group(group)
        group.cond.broadcast
        eventually(mutex: group.mutex) do
          expect(group.waiting_threads).to have_attributes(size: 0)
        end
      end

      before(:each) do
        @group1 = prepare_thread_group(5)
        @group2 = prepare_thread_group(5)
      end

      def eventually(mutex: nil, timeout: 5, &block)
        start = Time.now
        while Time.now - start < timeout
          begin
            if mutex
              mutex.synchronize do
                return yield
              end
            else
              return yield
            end
          rescue Exception => last_failure
          end
          Thread.pass
        end
        raise last_failure
      end

      it "triggers pruning if the thread idletimes have elapsed" do
        wakeup_thread_group(@group1)
        sleep 6
        wakeup_thread_group(@group2)

        eventually { expect(@group1.threads).to all(have_attributes(status: false)) }
        eventually { expect(@group2.threads).to all(have_attributes(status: 'sleep')) }
      end

      it "does not trigger pruning if the thread idletimes have not elapsed " do
        wakeup_thread_group(@group1)
        wakeup_thread_group(@group2)

        eventually { expect(@group1.threads).to all(have_attributes(status: 'sleep')) }
        eventually { expect(@group2.threads).to all(have_attributes(status: 'sleep')) }
      end

      it "keeps at least min_length workers" do
        wakeup_thread_group(@group1)
        wakeup_thread_group(@group2)
        sleep 6
        all_threads = @group1.threads + @group2.threads
        eventually do
          finished_threads = all_threads.find_all { |t| !t.status }
          expect(finished_threads).to have_attributes(size: 8)
        end
      end
    end

    context '#remaining_capacity' do

      let!(:expected_max){ 100 }
      let(:latch) { Concurrent::CountDownLatch.new }

      subject do
        RubyThreadPoolExecutor.new(
          min_threads: 10,
          max_threads: 20,
          idletime: 60,
          max_queue: expected_max,
          fallback_policy: :discard
        )
      end

      it 'returns :max_length when no tasks are enqueued' do
        5.times{ subject.post{ nil } }
        subject.post { latch.count_down }
        latch.wait(0.1)
        expect(subject.remaining_capacity).to eq expected_max
      end

      it 'returns the remaining capacity when tasks are enqueued' do
        block = Concurrent::CountDownLatch.new
        100.times{ subject.post{ block.wait } }
        subject.post { latch.count_down }
        latch.wait(0.1)
        expect(subject.remaining_capacity).to be < expected_max
        block.count_down
      end
    end

    context 'threads naming' do
      subject do
        opts = { min_threads: 2 }
        opts[:name] = pool_name if pool_name
        described_class.new(opts)
      end

      let(:names) do
        require 'concurrent/set'
        Concurrent::Set.new
      end

      before do
        subject.post(names) { |names| names << Thread.current.name }
        subject.post(names) { |names| names << Thread.current.name }
        subject.shutdown
        subject.wait_for_termination(pool_termination_timeout)
        expect(names.size).to eq 2
      end

      context 'without pool name' do
        let(:pool_name) { }
        it 'sets counted name' do
          expect(names.all? { |name| name =~ /^worker-\d+$/ }).to be true
        end
      end

      context 'with pool name' do
        let(:pool_name) { 'MyExecutor' }
        it 'sets counted name' do
          expect(names.all? { |name| name =~ /^MyExecutor-worker-\d+$/ }).to be true
        end
      end
    end

    context '#managed_block' do

      # repeatedly evaluate until it holds, useful for racing worker scheduling
      def wait_for(timeout = 5)
        deadline = Concurrent.monotonic_time + timeout
        loop do
          break if yield
          raise 'condition not met in time' if Concurrent.monotonic_time > deadline
          Thread.pass
        end
      end

      context 'options' do
        it 'defaults the compensation limit to zero' do
          pool = described_class.new(max_threads: 2)
          expect(pool.max_compensation_length).to eq 0
          pool.shutdown
          pool.wait_for_termination(pool_termination_timeout)
        end

        it 'exposes max_threads and the compensation limit separately' do
          pool = described_class.new(max_threads: 3, max_compensation_threads: 2)
          expect(pool.max_length).to eq 3
          expect(pool.max_compensation_length).to eq 2
          pool.shutdown
          pool.wait_for_termination(pool_termination_timeout)
        end

        it 'rejects a negative compensation limit' do
          expect {
            described_class.new(max_threads: 1, max_compensation_threads: -1)
          }.to raise_error(ArgumentError)
        end

        it 'rejects limits which together overflow the maximum' do
          expect {
            described_class.new(
              max_threads: described_class::DEFAULT_MAX_POOL_SIZE,
              max_compensation_threads: 1
            )
          }.to raise_error(ArgumentError)
        end
      end

      it 'runs the block and returns its value on an external thread without compensation' do
        pool = described_class.new(max_threads: 1, max_compensation_threads: 2)
        expect(pool.managed_block { :done }).to eq :done
        expect(pool.blocked_count).to eq 0
        expect(pool.length).to eq 0
        pool.shutdown
        pool.wait_for_termination(pool_termination_timeout)
      end

      it 'does not let an external (caller-runs) thread claim a worker slot' do
        pool = described_class.new(
          min_threads: 0,
          max_threads: 1,
          max_queue: 1,
          max_compensation_threads: 2,
          fallback_policy: :caller_runs
        )

        block_worker = Concurrent::CountDownLatch.new(1)
        external_ran = Concurrent::CountDownLatch.new(1)
        observed_length = Concurrent::AtomicFixnum.new(-1)

        # occupy the single real worker
        pool.post { block_worker.wait }

        # wait until the worker exists and is busy
        wait_for { pool.length == 1 }

        # the next task fills the single-slot queue
        queued_started = Concurrent::CountDownLatch.new(1)
        pool.post { queued_started.wait }

        # the pool is saturated (1 busy worker + 1 queued task), so this task
        # runs on this external thread via the caller_runs fallback;
        # managed_block must behave as a plain passthrough and never grow the pool
        caller = in_thread do
          pool.post do
            observed_length.value = pool.length
            external_ran.count_down
            pool.managed_block { }
          end
        end

        expect(external_ran.wait(5)).to be_truthy
        expect(observed_length.value).to eq 1 # no compensation thread was created
        expect(pool.blocked_count).to eq 0

        block_worker.count_down
        queued_started.count_down
        caller.join(5)
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end

      it 'counts nested declarations from the same worker only once' do
        pool = described_class.new(max_threads: 1, max_compensation_threads: 2)
        inner_started = Concurrent::CountDownLatch.new(1)
        can_finish    = Concurrent::CountDownLatch.new(1)
        nesting_seen  = Concurrent::AtomicFixnum.new(-1)
        compensation  = Concurrent::AtomicFixnum.new(-1)

        pool.post do
          pool.managed_block do
            inner_started.count_down
            can_finish.wait
            pool.managed_block do
              nesting_seen.value = pool.blocked_count
              compensation.value = pool.length
            end
          end
        end

        inner_started.wait(5)
        # one blocked worker -> at most one compensation thread
        wait_for { pool.length == 2 }
        expect(pool.blocked_count).to eq 1

        can_finish.count_down
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true

        expect(nesting_seen.value).to eq 1
        expect(compensation.value).to eq 2 # never a third thread
      end

      it 'restores the accounting after the block raises' do
        pool = described_class.new(max_threads: 1, max_compensation_threads: 1)
        started   = Concurrent::CountDownLatch.new(1)
        unblock   = Concurrent::CountDownLatch.new(1)
        raised    = Concurrent::AtomicBoolean.new(false)

        pool.post do
          begin
            pool.managed_block do
              started.count_down
              unblock.wait
              raise 'boom'
            end
          rescue RuntimeError
            raised.make_true
          end
        end

        started.wait(5)
        wait_for { pool.blocked_count == 1 && pool.length == 2 }

        unblock.count_down
        wait_for { raised.true? }
        wait_for { pool.blocked_count == 0 }
        expect(pool.blocked_count).to eq 0
        # the compensation worker becomes idle and is reaped back to max_threads
        wait_for { pool.length == 1 }

        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end

      it 'never creates more than max_threads + max_compensation_threads threads' do
        pool = described_class.new(max_threads: 2, max_compensation_threads: 2)
        unblock = Concurrent::CountDownLatch.new(1)
        started = Concurrent::CountDownLatch.new(4)

        4.times do
          pool.post do
            pool.managed_block do
              started.count_down
              unblock.wait
            end
          end
        end

        started.wait(5)
        wait_for { pool.length == 4 }
        expect(pool.length).to eq 4 # capped at 2 + 2 despite 4 blocked workers
        10.times { expect(pool.length).to be <= 4; Thread.pass }

        unblock.count_down
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end

      it 'does not grow beyond max_threads when the compensation limit is zero' do
        pool = described_class.new(max_threads: 1, max_compensation_threads: 0)
        unblock = Concurrent::CountDownLatch.new(1)
        started = Concurrent::CountDownLatch.new(1)

        pool.post do
          pool.managed_block do
            started.count_down
            unblock.wait
          end
        end

        started.wait(5)
        sleep 0.1
        expect(pool.length).to eq 1
        expect(pool.blocked_count).to eq 1

        unblock.count_down
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end

      it 'completes parent and child tasks in a single-worker pool without deadlock' do
        pool = described_class.new(
          min_threads: 1,
          max_threads: 1,
          max_compensation_threads: 1
        )

        result      = Queue.new
        child_done  = Concurrent::CountDownLatch.new(1)
        parent_done = Concurrent::CountDownLatch.new(1)

        pool.post do # parent task on the sole worker
          pool.managed_block do
            pool.post do # child task must run on the compensation thread
              child_done.count_down
              result << :child
            end
            child_done.wait(5)
          end
          result << :parent
          parent_done.count_down
        end

        expect(parent_done.wait(5)).to be_truthy
        expect(child_done.wait(0.01)).to be_truthy
        expect(result.length).to eq 2
        expect([result.pop, result.pop]).to contain_exactly(:child, :parent)

        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end

      it 'finishes shutdown started while workers are blocked, per the shutdown protocol' do
        pool = described_class.new(
          min_threads: 1,
          max_threads: 1,
          max_compensation_threads: 1
        )

        parent_blocked  = Concurrent::Event.new
        child_started   = Concurrent::Event.new
        release_child   = Concurrent::Event.new
        child_finished  = Concurrent::Event.new
        parent_finished = Concurrent::Event.new

        pool.post do # parent blocks (managed) waiting on a child in the same pool
          pool.managed_block do
            pool.post do # child picked up by the compensation worker
              child_started.set
              release_child.wait
              child_finished.set
            end
            parent_blocked.set
            child_finished.wait(5)
          end
          parent_finished.set
        end

        # parent is blocked and the compensation worker exists, child is
        # queued or just starting
        expect(parent_blocked.wait(5)).to be_truthy
        wait_for { pool.length == 2 }

        # begin orderly shutdown *while* the parent is blocked. The graceful
        # protocol must keep the queued/in-progress child and the blocked
        # parent alive until they naturally finish.
        pool.shutdown
        expect(pool).to be_shuttingdown

        # the external event the child waits on fires shortly after shutdown
        releaser = in_thread do
          expect(child_started.wait(5)).to be_truthy
          release_child.set
        end

        # every thread (parent + compensation) ends under the original
        # shutdown protocol, nothing is leaked or force-killed
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
        expect(child_finished.wait(1)).to be_truthy
        expect(parent_finished.wait(1)).to be_truthy
        releaser.join(5)
        expect(pool).to be_shutdown
        expect(pool.length).to eq 0
      end
    end
  end
end
