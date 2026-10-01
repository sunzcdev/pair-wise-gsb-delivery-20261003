require 'concurrent/atomic/count_down_latch'
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
      # A pool with exactly one normal worker and one compensation slot is
      # the minimal configuration in which a task waiting on a same-pool
      # subtask can deadlock without managed blocking.
      def single_worker_pool(cap)
        described_class.new(
          min_threads: 1,
          max_threads: 1,
          max_queue: -1,
          max_blocking_compensation: cap,
          fallback_policy: :abort
        )
      end

      it 'compensates a blocked parent so its same-pool child can run' do
        pool = single_worker_pool(1)
        completed = Concurrent::Event.new

        pool.post do
          pool.managed_block do
            child_result = Queue.new
            pool.post { child_result << :child_result }
            result = child_result.pop
            raise 'unexpected child value' unless result == :child_result
          end
          completed.set
        end

        expect(completed.wait(10)).to be_truthy
        expect(pool.blocking_count).to eq 0
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end

      it 'ends orderly when shutdown is initiated while a worker is blocked' do
        pool = single_worker_pool(1)
        entered = Concurrent::Event.new
        release = Concurrent::Event.new

        pool.post do
          pool.managed_block do
            entered.set
            release.wait
          end
        end

        expect(entered.wait(10)).to be_truthy
        repeat_until_success { expect(pool.blocking_count).to eq 1 }

        pool.shutdown
        repeat_until_success { expect(pool.shuttingdown?).to be true }
        expect(pool.shutdown?).to be false

        release.set
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
        expect(pool.length).to eq 0
      end

      it 'does not grant compensation to an external thread' do
        pool = single_worker_pool(1)
        external_block = Concurrent::Event.new

        thread = in_thread do
          pool.managed_block { external_block.wait }
        end
        sleep 0.2

        expect(pool.blocking_count).to eq 0
        expect(pool.length).to eq 0

        external_block.set
        expect(thread.join(10)).not_to be_nil
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end

      it 'counts nested declarations on the same worker only once' do
        pool = single_worker_pool(1)
        checked = Concurrent::Event.new

        pool.post do
          pool.managed_block do
            pool.managed_block do
              raise 'nested block must still count as one blocked worker' unless pool.blocking_count == 1
            end
            raise 'outer block must still count as one blocked worker' unless pool.blocking_count == 1
            checked.set
          end
        end

        expect(checked.wait(10)).to be_truthy
        repeat_until_success { expect(pool.blocking_count).to eq 0 }
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end

      it 'restores the accounting when the block raises' do
        pool = single_worker_pool(1)
        raised = Concurrent::Event.new

        pool.post do
          begin
            pool.managed_block { raise 'block failure' }
          rescue RuntimeError
            raised.set
          end
        end

        expect(raised.wait(10)).to be_truthy
        repeat_until_success { expect(pool.blocking_count).to eq 0 }

        # the slot must be usable again after the failure
        reusable = Concurrent::Event.new
        pool.post do
          pool.managed_block { reusable.set }
        end
        expect(reusable.wait(10)).to be_truthy

        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end

      it 'never creates more than max_threads + max_blocking_compensation threads' do
        pool = described_class.new(
          min_threads: 2,
          max_threads: 2,
          max_queue: -1,
          max_blocking_compensation: 1,
          fallback_policy: :abort
        )
        release    = Concurrent::Event.new
        entered    = Concurrent::CountDownLatch.new(3)
        observed   = Concurrent::AtomicFixnum.new(0)

        task = proc do
          pool.managed_block do
            observed.increment
            entered.count_down
            release.wait
          end
        end

        # first both normal workers block; entering the second block spawns
        # the single allowed compensation worker
        2.times { pool.post(&task) }
        repeat_until_success { expect(pool.blocking_count).to eq 2 }
        repeat_until_success { expect(pool.length).to eq 3 }

        # the compensation worker is now available and can block as well, but
        # no fourth thread may ever be created
        pool.post(&task)
        expect(entered.wait(10)).to be_truthy
        repeat_until_success { expect(observed.value).to eq 3 }
        expect(pool.length).to eq 3
        expect(pool.largest_length).to eq 3
        expect(pool.blocking_count).to eq 3

        release.set
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end

      it 'does not compensate when max_blocking_compensation is zero' do
        pool = single_worker_pool(0)
        entered = Concurrent::Event.new
        release = Concurrent::Event.new

        pool.post do
          pool.managed_block do
            entered.set
            release.wait
          end
        end

        expect(entered.wait(10)).to be_truthy
        repeat_until_success { expect(pool.blocking_count).to eq 1 }
        expect(pool.length).to eq 1

        release.set
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end

      it 'retains the compensation worker while blocked and prunes it afterwards' do
        pool = described_class.new(
          min_threads: 1,
          max_threads: 1,
          max_queue: -1,
          max_blocking_compensation: 1,
          idletime: 1
        )
        entered = Concurrent::Event.new
        release = Concurrent::Event.new

        pool.post do
          pool.managed_block do
            entered.set
            release.wait
          end
        end

        expect(entered.wait(10)).to be_truthy
        repeat_until_success { expect(pool.length).to eq 2 }

        # wait beyond one idletime, the extra capacity must still be present
        sleep 1.5
        expect(pool.length).to eq 2

        release.set
        repeat_until_success { expect(pool.blocking_count).to eq 0 }
        repeat_until_success(5) { expect(pool.length).to eq 1 }

        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end

      it 'finishes #wait_for_termination when killed while a worker is blocked' do
        pool = single_worker_pool(1)
        entered = Concurrent::Event.new
        release = Concurrent::Event.new

        pool.post do
          pool.managed_block do
            entered.set
            release.wait
          end
        end

        expect(entered.wait(10)).to be_truthy
        repeat_until_success { expect(pool.length).to eq 2 }

        pool.kill
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
        expect(pool.length).to eq 0
        expect(pool.shutdown?).to be true
        release.set
      end

      it 'raises ArgumentError without a block' do
        pool = single_worker_pool(1)
        expect { pool.managed_block }.to raise_error(ArgumentError)
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end
    end

    context ':max_blocking_compensation option' do
      it 'defaults to zero' do
        pool = described_class.new
        expect(pool.max_blocking_compensation).to eq 0
        expect(pool.blocking_count).to eq 0
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end

      it 'is separately observable from max_length' do
        pool = described_class.new(max_threads: 3, max_blocking_compensation: 2)
        expect(pool.max_length).to eq 3
        expect(pool.max_blocking_compensation).to eq 2
        pool.shutdown
        expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
      end

      it 'rejects a negative value' do
        expect { described_class.new(max_blocking_compensation: -1) }.to raise_error(ArgumentError)
      end
    end
  end
end
