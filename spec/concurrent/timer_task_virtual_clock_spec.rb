require 'concurrent/atomic/atomic_fixnum'
require 'concurrent/atomic/event'
require 'concurrent/timer_task'
require_relative '../support/virtual_scheduler'

module Concurrent

  RSpec.describe TimerTask do
    include TimerTaskTest

    # Build a task wired to an injectable virtual clock and a manual executor
    # so that neither timing nor task execution depend on real wall-clock
    # waits. Nothing runs until the example drives `clock` and `executor`.
    def build_task(**opts, &block)
      @clock = TimerTaskTest::VirtualScheduler.new
      @executor = TimerTaskTest::ManualExecutor.new
      defaults = {
        execution_interval: 10,
        run_now: true,
        interval_type: :fixed_rate,
        scheduler: @clock,
        executor: @executor
      }
      @task = TimerTask.new(defaults.merge(opts), &block)
    end

    attr_reader :clock, :executor, :task

    after(:each) do
      task.kill if task && !task.shutdown?
    end

    def statuses
      task.tick_history.map { |record| [record[:tick], record[:status], record[:ran_as]] }
    end

    # Run a single dispatched execution on a background thread, blocking until
    # the task registers it as the in-flight tick.
    def run_in_flight
      thread = Thread.new { executor.run_one }
      Thread.pass until task.running_tick
      thread
    end

    describe 'overlap policy configuration' do
      it 'defaults to :coalesce' do
        build_task { nil }
        expect(task.overlap_policy).to eq TimerTask::COALESCE_OVERLAP
      end

      it 'accepts each documented policy' do
        %i[skip coalesce queue].each do |policy|
          t = TimerTask.new(overlap_policy: policy) { nil }
          expect(t.overlap_policy).to eq policy
          t.kill
        end
      end

      it 'rejects an unknown overlap policy' do
        expect { TimerTask.new(overlap_policy: :nope) { nil } }
          .to raise_error(ArgumentError, /overlap_policy/)
      end

      it 'defaults max_queue and rejects a non-positive value' do
        t = TimerTask.new { nil }
        expect(t.max_queue).to eq TimerTask::DEFAULT_MAX_QUEUE
        t.kill

        expect { TimerTask.new(max_queue: 0) { nil } }
          .to raise_error(ArgumentError, /max_queue/)
      end
    end

    describe 'tick identity and the scheduled/executed mapping' do
      it 'assigns a stable monotonic identity to every scheduled tick' do
        counter = AtomicFixnum.new(0)
        build_task { counter.increment }
        task.execute
        clock.run_pending
        expect(executor.run_all).to eq 1
        expect(task.last_tick).to eq 0

        clock.advance(10)
        clock.advance(10)
        expect(executor.run_all).to eq 2
        expect(task.last_tick).to eq 2
        expect(task.tick_history.map { |r| r[:tick] }).to eq [0, 1, 2]
        expect(task.statistics).to include(scheduled: 3, ran: 3)
      end

      it 'keeps earlier tick identities when the interval is made smaller' do
        build_task { nil }
        task.execute
        clock.run_pending
        executor.run_all
        expect(task.last_tick).to eq 0

        task.execution_interval = 1
        # Advancing by the *old* interval still produces exactly one further
        # tick; the already assigned identity 0 is never rewritten or reused.
        clock.advance(10)
        executor.run_all
        expect(task.tick_history.map { |r| r[:tick] }).to eq [0, 1]
        expect(task.tick_history.first[:tick]).to eq 0
      end

      it 'records scheduled_at/due_at/started_at/finished_at per tick' do
        build_task { nil }
        task.execute
        clock.run_pending
        executor.run_all
        record = task.tick_history.first
        expect(record[:scheduled_at]).to be_a Float
        expect(record[:due_at]).to be_a Float
        expect(record[:started_at]).to be_a Float
        expect(record[:finished_at]).to be_a Float
        expect(record[:finished_at]).to be >= record[:started_at]
      end
    end

    describe ':skip overlap policy' do
      it 'drops ticks that fire while an execution is in flight' do
        gate = Event.new
        build_task(overlap_policy: :skip) { gate.wait(2) }
        task.execute
        clock.run_pending
        runner = run_in_flight

        clock.advance(30) # ticks 1, 2, 3 all fire while tick 0 runs
        expect(task.pending_ticks).to eq []
        expect(statuses).to eq [
          [0, :scheduled, nil],
          [1, :skipped, nil],
          [2, :skipped, nil],
          [3, :skipped, nil]
        ]

        gate.set
        runner.join(2)
        expect(statuses.first).to eq [0, :ran, nil]
        expect(task.statistics).to include(scheduled: 4, ran: 1, skipped: 3)
      end
    end

    describe ':coalesce overlap policy' do
      it 'merges overlapping ticks into one catch-up execution keeping the earliest id' do
        gate = Event.new
        build_task(overlap_policy: :coalesce) { gate.wait(2) }
        task.execute
        clock.run_pending
        runner = run_in_flight

        clock.advance(30) # ticks 1, 2, 3 fire while tick 0 runs
        expect(task.pending_ticks).to eq [1]
        expect(statuses).to eq [
          [0, :scheduled, nil],
          [1, :scheduled, nil],
          [2, :coalesced, 1],
          [3, :coalesced, 1]
        ]

        gate.set
        runner.join(2)
        executor.run_all # the single catch-up execution for tick 1
        expect(statuses.map(&:first)).to eq [0, 1, 2, 3]
        expect(task.tick_history[1][:status]).to eq :ran
        expect(task.statistics).to include(scheduled: 4, ran: 2, coalesced: 2, skipped: 0)
      end
    end

    describe ':queue overlap policy' do
      it 'queues overlapping ticks in order and bounds the queue' do
        gate = Event.new
        build_task(overlap_policy: :queue, max_queue: 2) { gate.wait(2) }
        task.execute
        clock.run_pending
        runner = run_in_flight

        clock.advance(30) # ticks 1, 2 queued; tick 3 dropped (queue full)
        expect(task.pending_ticks).to eq [1, 2]
        expect(statuses).to eq [
          [0, :scheduled, nil],
          [1, :scheduled, nil],
          [2, :scheduled, nil],
          [3, :skipped, nil]
        ]

        gate.set
        runner.join(2)
        expect(executor.run_all).to eq 2 # ticks 1 and 2, never concurrently
        expect(task.statistics).to include(scheduled: 4, ran: 3, skipped: 1, coalesced: 0)
        executed = task.tick_history.select { |r| r[:status] == :ran }.map { |r| r[:tick] }
        expect(executed).to eq [0, 1, 2]
      end

      it 'never accumulates an unbounded number of waiting ticks' do
        gate = Event.new
        build_task(overlap_policy: :queue, max_queue: 1) { gate.wait(2) }
        task.execute
        clock.run_pending
        runner = run_in_flight

        clock.advance(100) # many ticks fire, queue must stay bounded
        expect(task.pending_ticks.size).to be <= 1
        gate.set
        runner.join(2)
        executor.run_all
        expect(task.statistics[:skipped]).to be >= 1
      end
    end

    describe 'shutdown and kill' do
      it 'rejects ticks fired after shutdown and cancels queued work' do
        gate = Event.new
        build_task(overlap_policy: :queue, max_queue: 5) { gate.wait(2) }
        task.execute
        clock.run_pending
        runner = run_in_flight
        clock.advance(10) # tick 1 queues

        task.shutdown
        expect(task.running?).to eq false

        clock.advance(100) # must not schedule or queue anything new
        expect(executor.pending_count).to eq 1 # only the in-flight tick 0
        expect(task.statistics[:cancelled]).to eq 1 # queued tick 1

        gate.set
        runner.join(2)
        # The in-flight execution still finishes and is attributed to tick 0.
        expect(task.tick_history.first[:status]).to eq :ran
        expect(task.statistics[:ran]).to eq 1
      end

      it 'lets an in-flight execution complete on its own after kill' do
        gate = Event.new
        counter = AtomicFixnum.new(0)
        build_task { gate.wait(2); counter.increment }
        task.execute
        clock.run_pending
        runner = run_in_flight

        task.kill
        expect(task.running?).to eq false
        gate.set
        runner.join(2)
        expect(counter.value).to eq 1
        expect(task.tick_history.first[:status]).to eq :ran
      end

      it 'does not run stale ticks twice after a restart' do
        counter = AtomicFixnum.new(0)
        build_task { counter.increment }
        task.execute
        clock.run_pending
        executor.run_all
        expect(counter.value).to eq 1

        task.shutdown
        task.execute
        clock.run_pending
        executor.run_all
        expect(counter.value).to eq 2
        executed = task.tick_history.select { |r| r[:status] == :ran }
        expect(executed.size).to eq 2
        expect(executed.map { |r| r[:tick] }).to eq [0, 1]
      end
    end

    describe 'fault tolerance' do
      it 'keeps producing ticks when the block raises' do
        failures = AtomicFixnum.new(0)
        build_task(interval_type: :fixed_rate) { failures.increment; raise 'boom' }
        task.execute
        5.times { clock.advance(10); executor.run_all }

        expect(failures.value).to eq 6 # run_now tick + five advances
        expect(task.statistics[:ran]).to eq 6
        expect(task.running?).to eq true
      end

      it 'reports the exception through observers without stopping the stream' do
        observed = []
        build_task(interval_type: :fixed_rate) { raise ArgumentError, 'x' }
        task.add_observer { |_time, _value, reason| observed << reason }
        task.execute
        clock.advance(10)
        executor.run_all
        expect(observed.size).to eq 2 # run_now tick plus the advanced tick
        expect(observed.first).to be_a ArgumentError
        expect(task.running?).to eq true
      end

      it 'survives an executor rejection and keeps the tick stream alive' do
        rejecting = Class.new do
          def running?; true; end
          def post(*); raise Concurrent::RejectedExecutionError; end
        end.new
        clock = TimerTaskTest::VirtualScheduler.new
        t = TimerTask.new(execution_interval: 10, run_now: true,
                          interval_type: :fixed_rate,
                          scheduler: clock, executor: rejecting) { nil }
        t.execute
        expect { clock.run_pending }.not_to raise_error
        expect(t.running?).to eq true
        expect(t.statistics[:skipped]).to eq 1
        # The periodic stream still produces the next tick.
        expect { clock.advance(10) }.not_to raise_error
        t.kill
      end
    end

    describe 'interval types' do
      it 'uses a fixed delay: the next tick is armed only after completion' do
        build_task(interval_type: :fixed_delay) { nil }
        task.execute
        clock.run_pending
        expect(clock.pending.size).to eq 0 # nothing armed while the job is queued
        executor.run_one
        expect(clock.pending.size).to eq 1
        expect(clock.pending.first.due_at).to eq 10.0
      end

      it 'uses a fixed rate: ticks keep their phase during an overrun' do
        build_task(interval_type: :fixed_rate, overlap_policy: :skip) { nil }
        task.execute
        clock.run_pending
        executor.run_one
        # The periodic entry stays armed at a constant 10s phase regardless of
        # how long the execution took.
        expect(clock.pending.first.due_at).to eq 10.0
        clock.advance(10)
        expect(clock.pending.first.due_at).to eq 20.0
      end
    end

    describe 'history retention' do
      it 'caps the tick history instead of growing without bound' do
        stub_const('Concurrent::TimerTask::HISTORY_LIMIT', 5)
        build_task(interval_type: :fixed_rate) { nil }
        task.execute
        8.times { clock.advance(10); executor.run_all }
        expect(task.tick_history.size).to eq 5
        # Identities stay monotonic even though old records were trimmed.
        expect(task.tick_history.map { |r| r[:tick] }).to eq [4, 5, 6, 7, 8]
      end
    end
  end
end
