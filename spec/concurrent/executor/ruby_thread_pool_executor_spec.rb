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

    context 'worker lifecycle' do

      let(:pool) do
        RubyThreadPoolExecutor.new(min_threads: 1, max_threads: 1, max_queue: 10, fallback_policy: :discard)
      end

      after(:each) do
        pool.shutdown
        pool.wait_for_termination(pool_termination_timeout)
      end

      def eventually(timeout: 5, &block)
        deadline = Concurrent.monotonic_time + timeout
        begin
          return yield
        rescue RSpec::Expectations::ExpectationNotMetError, RuntimeError
          raise if Concurrent.monotonic_time > deadline
          sleep 0.01
          retry
        end
      end

      context 'when a task calls Thread.exit' do

        it 'does not keep reporting the dead worker as a live thread' do
          exited = Queue.new
          pool.post { exited << Thread.current; Thread.exit }
          dead = exited.pop

          eventually { expect(dead.alive?).to be false }
          eventually { expect(pool.length).to eq 1 } # the replacement
          expect(pool.active_count).to eq 0
        end

        it 'replaces the worker so queued tasks still run' do
          pool.post { Thread.exit }
          done = Concurrent::CountDownLatch.new(3)
          3.times { pool.post { done.count_down } }
          expect(done.wait(5)).to be true
        end

        it 'does not re-run the task which called Thread.exit' do
          executions = Concurrent::AtomicFixnum.new(0)
          pool.post { executions.increment; Thread.exit }
          sleep 0.2
          # started exactly once; the pool must not replay it
          expect(executions.value).to eq 1
        end
      end

      context 'when a task raises a non-StandardError' do

        it 'does not re-run the already started task' do
          executions = Concurrent::AtomicFixnum.new(0)
          pool.post { executions.increment; raise Exception, 'fatal' }
          sleep 0.2
          expect(executions.value).to eq 1
        end

        it 'replaces the worker so queued tasks still run' do
          pool.post { raise Exception, 'fatal' }
          done = Concurrent::CountDownLatch.new(3)
          3.times { pool.post { done.count_down } }
          expect(done.wait(5)).to be true
        end

        it 'keeps a StandardError from killing the worker' do
          pool.post { raise StandardError, 'ordinary failure' }
          done = Concurrent::CountDownLatch.new(1)
          pool.post { done.count_down }
          expect(done.wait(5)).to be true
          expect(pool.length).to eq 1
        end
      end

      context 'when a worker dies while the pool is shutting down' do

        it 'still terminates instead of waiting for the vanished worker' do
          gate = Concurrent::Event.new
          started = Concurrent::CountDownLatch.new(1)
          pool.post { started.count_down; gate.wait; Thread.exit }
          expect(started.wait(5)).to be true

          done = Concurrent::CountDownLatch.new(1)
          pool.post { done.count_down }
          pool.shutdown
          gate.set

          expect(done.wait(5)).to be true
          expect(pool.wait_for_termination(pool_termination_timeout)).to be true
        end
      end

      context 'when a replacement worker cannot be started' do

        it 'counts and reports the failure instead of pretending success' do
          worker_class = RubyThreadPoolExecutor.const_get(:Worker)
          allow(worker_class).to receive(:new).and_raise(ThreadError, 'cannot create Thread')

          pool.post { Thread.exit }
          eventually { expect(pool.worker_spawn_failures).to be > 0 }
          expect(pool.length).to eq 0

          # A stranded queue must not hang an orderly shutdown.
          pool.post { nil }
          pool.shutdown
          expect(pool.wait_for_termination(pool_termination_timeout)).to be true
        end
      end
    end

    context 'worker terminal states' do

      let(:worker_class) { RubyThreadPoolExecutor.const_get(:Worker) }

      it 'exposes starting, idle, busy and terminal states' do
        expect(worker_class::STATES).to include(:starting, :idle, :busy, :stopped, :killed, :died)
        expect(worker_class::TERMINAL_STATES).to contain_exactly(:stopped, :killed, :died)
      end

      it 'makes a worker no longer busy once it is terminal' do
        worker = worker_class.allocate
        worker.instance_variable_set(:@id, 1)
        worker.instance_variable_set(:@state, :starting)

        expect(worker.terminal?).to be false
        expect(worker.terminate!(:stopped)).to be true
        expect(worker.state).to eq :stopped
        expect(worker.terminal?).to be true
        expect(worker.busy?).to be false
      end

      it 'only leaves a terminal state once' do
        worker = worker_class.allocate
        worker.instance_variable_set(:@id, 1)
        worker.instance_variable_set(:@state, :starting)

        expect(worker.die!).to be true
        expect(worker.die!).to be false
        expect(worker.state).to eq :died
      end

      it 'rejects non-terminal states' do
        worker = worker_class.allocate
        worker.instance_variable_set(:@id, 1)
        worker.instance_variable_set(:@state, :starting)

        expect { worker.terminate!(:busy) }.to raise_error(ArgumentError)
      end
    end

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
  end
end
