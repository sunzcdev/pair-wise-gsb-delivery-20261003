require 'concurrent/executor/thread_pool_executor'
require 'concurrent/executor/immediate_executor'
require 'concurrent/executor/simple_executor_service'
require 'concurrent/atomic/count_down_latch'
require 'concurrent/atomic/event'

module Concurrent

  RSpec.describe 'ExecutorService#post_with_receipt' do

    # A pool with a single worker and room for exactly one queued task makes
    # the three admission paths deterministic: the first task occupies the
    # worker, the second one fills the queue and the third one hits the
    # fallback policy.
    def new_pool(fallback_policy)
      ThreadPoolExecutor.new(
        min_threads: 1,
        max_threads: 1,
        idletime:   60,
        max_queue:  1,
        fallback_policy: fallback_policy
      )
    end

    def shutdown!(pool)
      pool.shutdown unless pool.shutdown?
      expect(pool.wait_for_termination(pool_termination_timeout)).to eq true
    end

    # Occupies the only worker until +gate+ is set, and waits until the worker
    # actually started running the task.
    def occupy_worker(pool, gate)
      started = CountDownLatch.new(1)
      pool.post { started.count_down; gate.wait }
      expect(started.wait(pool_termination_timeout)).to eq true
    end

    # Fills the single queue slot and waits until the task is really queued.
    def fill_queue(pool, gate)
      latch = CountDownLatch.new(1)
      pool.post { gate.wait; latch.count_down }
      eventually { expect(pool.queue_length).to eq 1 }
      latch
    end

    def eventually(timeout: pool_termination_timeout)
      deadline = Concurrent.monotonic_time + timeout
      begin
        yield
      rescue RSpec::Expectations::ExpectationNotMetError
        raise if Concurrent.monotonic_time > deadline
        sleep 0.01
        retry
      end
    end

    describe 'the :queued path' do
      let(:pool) { new_pool(:abort) }
      after(:each) { shutdown!(pool) }

      it 'reports :queued and completes with the task value' do
        caller  = Thread.current
        ran_on  = nil
        receipt = pool.post_with_receipt(2, 3) { |a, b| ran_on = Thread.current; a + b }

        expect(receipt).to be_a(SubmissionReceipt)
        expect(receipt.admission).to eq :queued
        expect(receipt).to be_queued
        expect(receipt.wait(pool_termination_timeout)).to eq true
        expect(receipt.status).to eq :completed
        expect(receipt).to be_completed
        expect(receipt).to be_terminal
        expect(receipt.value).to eq 5
        expect(receipt.error).to be_nil
        expect(ran_on).not_to eq caller
      end

      it 'reports :queued for a task which had to enter the work queue' do
        gate    = Event.new
        occupy_worker(pool, gate)
        receipt = pool.post_with_receipt { :queued_task }

        expect(receipt.admission).to eq :queued
        expect(pool.queue_length).to eq 1

        gate.set
        expect(receipt.wait(pool_termination_timeout)).to eq true
        expect(receipt.status).to eq :completed
        expect(receipt.value).to eq :queued_task
      end

      it 'records a failure raised by the task' do
        error   = Class.new(StandardError)
        receipt = pool.post_with_receipt { raise error, 'boom' }

        expect(receipt.wait(pool_termination_timeout)).to eq true
        expect(receipt.status).to eq :failed
        expect(receipt).to be_failed
        expect(receipt.error).to be_a(error)
        expect(receipt.error.message).to eq 'boom'
        expect(receipt.value).to be_nil
      end
    end

    describe 'the :caller_runs path' do
      let(:pool) { new_pool(:caller_runs) }
      after(:each) { shutdown!(pool) }

      it 'reports :caller_runs and runs the task on the submitting thread' do
        gate    = Event.new
        occupy_worker(pool, gate)
        fill_queue(pool, gate)

        caller  = Thread.current
        ran_on  = nil
        receipt = pool.post_with_receipt(:value) { |v| ran_on = Thread.current; v }

        expect(receipt.admission).to eq :caller_runs
        expect(receipt).to be_caller_runs
        expect(ran_on).to eq caller
        expect(receipt.status).to eq :completed
        expect(receipt.value).to eq :value

        gate.set
      end

      it 'records a failure raised by the fallback task' do
        gate    = Event.new
        occupy_worker(pool, gate)
        fill_queue(pool, gate)

        error   = Class.new(StandardError)
        receipt = pool.post_with_receipt { raise error, 'fallback boom' }

        expect(receipt.admission).to eq :caller_runs
        expect(receipt.status).to eq :failed
        expect(receipt.error).to be_a(error)
        expect(receipt.error.message).to eq 'fallback boom'

        gate.set
      end

      it 'does not hold the pool lock while running the caller_runs block' do
        gate = Event.new
        occupy_worker(pool, gate)
        fill_queue(pool, gate)

        started = Event.new
        release = Event.new
        receipt = nil
        caller  = in_thread do
          receipt = pool.post_with_receipt do
            started.set
            release.wait(pool_termination_timeout)
          end
        end

        expect(started.wait(pool_termination_timeout)).to eq true

        # While the user block is blocked on the submitting thread the pool
        # lock must be free, so another thread can inspect/submit.
        observer = in_thread { [pool.queue_length, pool.running?] }
        expect(observer.join(2)).not_to be_nil, 'pool lock was held while running the user block'
        expect(observer.value).to eq [1, true]

        release.set
        expect(caller.join(2)).not_to be_nil
        expect(receipt.admission).to eq :caller_runs
        expect(receipt.status).to eq :completed

        gate.set
      end

      it 'allows the fallback task to submit to the same pool again' do
        gate = Event.new
        occupy_worker(pool, gate)
        fill_queue(pool, gate)

        inner = nil
        outer = pool.post_with_receipt do
          inner = pool.post_with_receipt { :inner }
          :outer
        end

        expect(outer.admission).to eq :caller_runs
        expect(outer.status).to eq :completed
        expect(outer.value).to eq :outer
        # the pool is still saturated, so the nested submission falls back too
        expect(inner.admission).to eq :caller_runs
        expect(inner.status).to eq :completed
        expect(inner.value).to eq :inner

        gate.set
      end
    end

    describe 'the :rejected path' do
      it 'reports :rejected without raising for the :abort policy' do
        pool = new_pool(:abort)
        begin
          gate = Event.new
          occupy_worker(pool, gate)
          fill_queue(pool, gate)

          executed = false
          receipt  = pool.post_with_receipt { executed = true }

          expect(receipt.admission).to eq :rejected
          expect(receipt).to be_rejected
          expect(receipt.status).to eq :rejected
          expect(receipt.error).to be_a(RejectedExecutionError)
          expect(executed).to eq false

          # the original contract of #post is unchanged
          expect { pool.post { nil } }.to raise_error(RejectedExecutionError)

          gate.set
        ensure
          shutdown!(pool)
        end
      end

      it 'reports :rejected without error for the :discard policy' do
        pool = new_pool(:discard)
        begin
          gate = Event.new
          occupy_worker(pool, gate)
          fill_queue(pool, gate)

          executed = false
          receipt  = pool.post_with_receipt { executed = true }

          expect(receipt.admission).to eq :rejected
          expect(receipt.status).to eq :rejected
          expect(receipt.error).to be_nil
          expect(executed).to eq false

          # the original contract of #post is unchanged
          expect(pool.post { nil }).to eq false

          gate.set
        ensure
          shutdown!(pool)
        end
      end
    end

    describe 'accounting' do
      it 'counts a queued execution once as scheduled and once as completed' do
        pool = new_pool(:abort)
        begin
          expect(pool.post_with_receipt { nil }.wait(pool_termination_timeout)).to eq true
          eventually { expect(pool.completed_task_count).to eq 1 }
          expect(pool.scheduled_task_count).to eq 1
        ensure
          shutdown!(pool)
        end
      end

      it 'does not count caller_runs executions as queued or completed' do
        pool = new_pool(:caller_runs)
        begin
          gate = Event.new
          occupy_worker(pool, gate)
          fill_queue(pool, gate)

          expect(pool.scheduled_task_count).to eq 2

          outer = pool.post_with_receipt { pool.post_with_receipt { nil }; nil }
          expect(outer.admission).to eq :caller_runs
          expect(pool.scheduled_task_count).to eq 2

          gate.set
          shutdown!(pool)

          # one execution per task which actually entered the pool
          expect(pool.scheduled_task_count).to eq 2
          expect(pool.completed_task_count).to eq 2
        end
      end

      it 'does not change the counters for a rejected execution' do
        pool = new_pool(:discard)
        begin
          gate = Event.new
          occupy_worker(pool, gate)
          fill_queue(pool, gate)

          expect(pool.post_with_receipt { nil }.admission).to eq :rejected
          expect(pool.scheduled_task_count).to eq 2

          gate.set
          shutdown!(pool)
          expect(pool.completed_task_count).to eq 2
        end
      end
    end

    describe 'the shutdown boundary' do
      it 'executes a task admitted before shutdown' do
        pool    = new_pool(:discard)
        begin
          gate    = Event.new
          occupy_worker(pool, gate)
          receipt = pool.post_with_receipt { :admitted }

          expect(receipt.admission).to eq :queued
          pool.shutdown
          gate.set

          expect(receipt.wait(pool_termination_timeout)).to eq true
          expect(receipt.status).to eq :completed
          expect(receipt.value).to eq :admitted
        ensure
          shutdown!(pool)
        end
      end

      it 'falls back to caller_runs for a submission which loses the race' do
        pool = new_pool(:caller_runs)
        begin
          pool.shutdown

          caller  = Thread.current
          ran_on  = nil
          receipt = pool.post_with_receipt { ran_on = Thread.current; :after_shutdown }

          expect(receipt.admission).to eq :caller_runs
          expect(receipt.status).to eq :completed
          expect(ran_on).to eq caller
        ensure
          shutdown!(pool)
        end
      end

      it 'rejects a submission which loses the race' do
        pool = new_pool(:abort)
        begin
          pool.shutdown
          receipt = pool.post_with_receipt { nil }

          expect(receipt.admission).to eq :rejected
          expect(receipt.error).to be_a(RejectedExecutionError)
        ensure
          shutdown!(pool)
        end
      end
    end

    describe 'argument validation' do
      it 'raises an ArgumentError when no block is given' do
        pool = new_pool(:abort)
        begin
          expect { pool.post_with_receipt } .to raise_error(ArgumentError)
        ensure
          shutdown!(pool)
        end
      end
    end

    describe 'preserving the #post contract' do
      it 'leaves #post raising for :abort and returning false for :discard' do
        abort_pool = new_pool(:abort)
        discard_pool = new_pool(:discard)
        begin
          gate = Event.new
          occupy_worker(abort_pool, gate)
          fill_queue(abort_pool, gate)
          occupy_worker(discard_pool, gate)
          fill_queue(discard_pool, gate)

          expect { abort_pool.post { nil } }.to raise_error(RejectedExecutionError)
          expect(discard_pool.post { nil }).to eq false

          gate.set
        ensure
          shutdown!(abort_pool)
          shutdown!(discard_pool)
        end
      end

      it 'runs a :caller_runs task inline, as #post does' do
        pool = new_pool(:caller_runs)
        begin
          gate = Event.new
          occupy_worker(pool, gate)
          fill_queue(pool, gate)

          executed = false
          receipt  = pool.post_with_receipt { executed = :inline }

          expect(executed).to eq :inline
          expect(receipt.admission).to eq :caller_runs
          expect(receipt.status).to eq :completed

          gate.set
        ensure
          shutdown!(pool)
        end
      end

      it 'does not let a task failure escape on the :queued path' do
        pool = new_pool(:abort)
        begin
          receipt = pool.post_with_receipt { raise 'boom' }
          expect { receipt.wait(pool_termination_timeout) }.not_to raise_error
          expect(receipt.status).to eq :failed
        ensure
          shutdown!(pool)
        end
      end
    end

    describe 'other executors' do
      it 'reports :queued for an executor which spawns a thread' do
        executor = SimpleExecutorService.new
        begin
          receipt = executor.post_with_receipt(1, 2) { |a, b| a + b }
          expect(receipt.admission).to eq :queued
          expect(receipt.wait(pool_termination_timeout)).to eq true
          expect(receipt.status).to eq :completed
          expect(receipt.value).to eq 3
        ensure
          executor.shutdown
          executor.wait_for_termination(pool_termination_timeout)
        end
      end

      it 'reports :caller_runs for an executor which runs inline' do
        executor = ImmediateExecutor.new
        begin
          caller  = Thread.current
          ran_on  = nil
          receipt = executor.post_with_receipt { ran_on = Thread.current; :inline }
          expect(receipt.admission).to eq :caller_runs
          expect(receipt.status).to eq :completed
          expect(receipt.value).to eq :inline
          expect(ran_on).to eq caller
        ensure
          executor.shutdown
        end
      end

      it 'reports :rejected for an executor which refuses the task' do
        executor = ImmediateExecutor.new
        executor.shutdown
        receipt = executor.post_with_receipt { nil }
        expect(receipt.admission).to eq :rejected
        expect(receipt.status).to eq :rejected
      end

      it 'records a failure of an inline task without letting it escape' do
        executor = ImmediateExecutor.new
        begin
          error   = Class.new(StandardError)
          receipt = nil
          expect { receipt = executor.post_with_receipt { raise error, 'inline boom' } }.not_to raise_error
          expect(receipt.admission).to eq :caller_runs
          expect(receipt.status).to eq :failed
          expect(receipt.error).to be_a(error)
        ensure
          executor.shutdown
        end
      end

      it 'records a failure of a spawned task' do
        executor = SimpleExecutorService.new
        begin
          error   = Class.new(StandardError)
          receipt = executor.post_with_receipt { raise error, 'spawned boom' }
          expect(receipt.wait(pool_termination_timeout)).to eq true
          expect(receipt.status).to eq :failed
          expect(receipt.error).to be_a(error)
        ensure
          executor.shutdown
          executor.wait_for_termination(pool_termination_timeout)
        end
      end
    end
  end
end
