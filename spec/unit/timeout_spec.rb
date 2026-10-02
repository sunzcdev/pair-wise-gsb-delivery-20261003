require 'spec_helper'

describe 'state timeouts' do
  # Forces the initial state (and therefore the initial-state deadline) to be
  # entered at the clock's starting value, so boundaries are deterministic.
  def build(klass, start = 100.0)
    clock = ManualMonotonicClock.new(start)
    object = klass.new(clock)
    object.aasm(:work).time_remaining if klass == TimeoutNamedMachineExample
    object.aasm.time_remaining unless klass == TimeoutNamedMachineExample
    [object, clock]
  end

  describe 'declaration' do
    it 'stores a timeout duration and timeout event on the state' do
      state = TimeoutExample.aasm.states.find { |s| s.name == :waiting }
      expect(state.timeout).to eq(5)
      expect(state.timeout_event).to eq(:expire)
    end

    it 'defaults the timeout event to :expire' do
      state = TimeoutWithoutEventExample.aasm.states.find { |s| s.name == :waiting }
      expect(state.timeout_event).to eq(:expire)
    end

    it 'allows a zero duration (used for zero-wait states)' do
      state = TimeoutExample.aasm.states.find { |s| s.name == :running }
      expect(state.timeout).to eq(0)
      expect(state.timeout?).to eq(true)
    end

    it 'reports no timeout for states without a declaration' do
      state = TimeoutExample.aasm.states.find { |s| s.name == :completed }
      expect(state.timeout?).to eq(false)
      expect(state.timeout).to be_nil
    end

    it 'rejects a non-numeric timeout at declaration time' do
      dsl = TimeoutExample.aasm
      expect { dsl.send(:validate_timeout_options!, timeout: 'soon') }.to raise_error(ArgumentError, /invalid timeout/)
    end

    it 'rejects a negative timeout at declaration time' do
      dsl = TimeoutExample.aasm
      expect { dsl.send(:validate_timeout_options!, timeout: -1) }.to raise_error(ArgumentError, /non-negative/)
    end
  end

  describe 'remaining time (read-only)' do
    it 'reports the remaining monotonic time of the current deadline' do
      job, clock = build(TimeoutExample)
      expect(job.aasm.time_remaining(clock.now)).to eq(5)
      clock.advance(2)
      expect(job.aasm.time_remaining(clock.now)).to eq(3)
    end

    it 'floors remaining time at zero past the deadline' do
      job, clock = build(TimeoutExample)
      clock.advance(8)
      expect(job.aasm.time_remaining(clock.now)).to eq(0)
    end

    it 'returns nil when the current state has no deadline' do
      job, clock = build(TimeoutExample)
      clock.advance(5)
      job.aasm.time_out(clock.now) # waiting -> completed, no timeout declared
      expect(job.aasm.time_remaining(clock.now)).to be_nil
    end

    it 'does not fire any event when reading remaining time' do
      job, clock = build(TimeoutExample)
      clock.advance(100)
      expect { job.aasm.time_remaining(clock.now) }.not_to change { job.aasm.current_state }
      expect(job.aasm.current_state).to eq(:waiting)
    end

    it 'does not fire any event when asking whether a timeout is due' do
      job, clock = build(TimeoutExample)
      clock.advance(100)
      expect(job.aasm.timeouts_due?(clock.now)).to eq(true)
      expect(job.aasm.current_state).to eq(:waiting)
    end
  end

  describe 'advancing' do
    it 'does nothing before the deadline' do
      job, clock = build(TimeoutExample)
      result = job.aasm.time_out(clock.now + 4.999)
      expect(result).to be_none
      expect(job.aasm.current_state).to eq(:waiting)
    end

    it 'fires the timeout event exactly at the deadline (<=)' do
      job, clock = build(TimeoutExample)
      result = job.aasm.time_out(clock.now + 5)
      expect(result).to be_fired
      expect(result.from_state).to eq(:waiting)
      expect(result.to_state).to eq(:completed)
      expect(result.event).to eq(:expire)
      expect(result.state).to eq(:waiting)
      expect(job.aasm.current_state).to eq(:completed)
    end

    it 'fires after the deadline' do
      job, clock = build(TimeoutExample)
      result = job.aasm.time_out(clock.now + 5.5)
      expect(result).to be_fired
    end

    it 'returns :none when the state has no timeout' do
      job, clock = build(TimeoutExample)
      clock.advance(5)
      job.aasm.time_out(clock.now)
      result = job.aasm.time_out(clock.now + 100)
      expect(result).to be_none
    end

    it 'passes extra arguments through to the event and guards' do
      object = TimeoutExtraArgsExample.new
      object.aasm.time_remaining
      result = object.aasm.time_out(1, :extra, 42)
      expect(result).to be_fired
    end

    it 'passes keyword arguments through to the event' do
      object = TimeoutKeywordArgumentsExample.new
      object.aasm.time_remaining
      result = object.aasm.time_out(1, :positional, key: 7)
      expect(result).to be_fired
      expect(object.received_key).to eq(7)
      expect(object.received_args).to eq([:positional])
    end

    it 'uses the default monotonic clock when no time is given' do
      job = TimeoutExample.new
      job.aasm.time_remaining # enter initial state now
      result = job.aasm.time_out # not enough wall-clock time elapsed
      expect(result).to be_none
      expect(job.aasm.current_state).to eq(:waiting)
    end
  end

  describe 'no spinning within one advance' do
    it 'does not handle a zero-wait deadline armed during the same advance' do
      job, clock = build(TimeoutExample)
      clock.advance(5)
      result = job.aasm.time_out(clock.now)
      expect(result.to_state).to eq(:completed)
      # waiting -> completed, not into running; use a machine that ends there
    end

    it 'leaves a zero-wait target state for the next advance' do
      clock = ManualMonotonicClock.new(100.0)
      object = TimeoutZeroWaitChainExample.new(clock)
      object.aasm.time_remaining

      clock.advance(5)
      first = object.aasm.time_out(clock.now)
      expect(first).to be_fired
      expect(object.aasm.current_state).to eq(:running)

      # the zero-wait running state is due but only handled on a new advance
      clock.advance(1)
      second = object.aasm.time_out(clock.now)
      expect(second).to be_fired
      expect(second.from_state).to eq(:running)
      expect(object.aasm.current_state).to eq(:done)
    end
  end

  describe 'deadline ownership and re-entry' do
    it 'voids the deadline when the state is left through a normal event' do
      job, clock = build(TimeoutExample)
      clock.advance(2)
      job.start # waiting -> running (zero-wait)
      expect(job.aasm.time_remaining(clock.now)).to eq(0)
      # old waiting deadline (t=105) must never fire
      clock.advance(10)
      result = job.aasm.time_out(clock.now)
      # running zero-wait fires to :stuck, proving the schedule belongs to running
      expect(result.from_state).to eq(:running)
      expect(job.aasm.current_state).to eq(:stuck)
    end

    it 'arms a fresh deadline when re-entering the same state name' do
      object = TimeoutReentryExample.new
      object.aasm.time_remaining

      object.t = 3          # remaining 2
      object.leave          # waiting -> other, old deadline voided
      object.t = 13
      object.come_back      # re-enter waiting at t=13, fresh deadline t=18
      result = object.aasm.time_out(17)
      expect(result).to be_none
      result = object.aasm.time_out(18)
      expect(result).to be_fired
    end

    it 're-arms on a self-transition and the old due record cannot retrigger' do
      object = TimeoutInlineSelfTransitionExample.new
      object.aasm.time_remaining # deadline t=0 -> t=5

      object.t = 3
      object.poke # self-transition: fresh deadline t=3 -> t=8

      expect(object.aasm.time_out(5)).to be_none # old record must not retrigger
      expect(object.aasm.time_remaining(5)).to eq(3)

      result = object.aasm.time_out(8)
      expect(result).to be_fired
    end

    it 'binds the deadline to a successful entry only' do
      job, clock = build(TimeoutExample)
      # :start! persists (plain persistence returns true), non-available events
      # do not disturb the deadline
      expect(job.aasm.time_remaining(clock.now)).to eq(5)
    end
  end

  describe 'failed transitions' do
    it 'returns a rejected result when a guard blocks the timeout event' do
      job, clock = build(TimeoutGuardedExample)
      clock.advance(10)
      result = job.aasm.time_out(clock.now)
      expect(result).to be_rejected
      expect(result).to be_retryable
      expect(result.event).to eq(:expire)
      expect(result.error).to be_a(AASM::InvalidTransition)
      expect(job.aasm.current_state).to eq(:waiting)
    end

    it 'keeps the deadline after rejection and still reports it as due' do
      job, clock = build(TimeoutGuardedExample)
      clock.advance(10)
      job.aasm.time_out(clock.now)
      expect(job.aasm.timeouts_due?(clock.now)).to eq(true)
      expect(job.aasm.time_remaining(clock.now)).to eq(0)
    end

    it 'advancing again attempts the pending deadline again (explicit re-evaluation)' do
      job, clock = build(TimeoutGuardedExample)
      clock.advance(10)
      expect(job.aasm.time_out(clock.now)).to be_rejected
      job.allow_expire = true
      expect(job.aasm.time_out(clock.now)).to be_fired
      expect(job.aasm.current_state).to eq(:completed)
    end

    it 'allows an explicit retry after changing conditions' do
      job, clock = build(TimeoutGuardedExample)
      clock.advance(10)
      expect(job.aasm.time_out(clock.now)).to be_rejected
      job.allow_expire = true
      result = job.aasm.retry_timeout(clock.now)
      expect(result).to be_fired
      expect(job.aasm.current_state).to eq(:completed)
    end

    it 'reports a rejection (without raising) when whiny_transitions is disabled' do
      job, clock = build(TimeoutQuietGuardedExample)
      clock.advance(10)
      result = job.aasm.time_out(clock.now)
      expect(result).to be_rejected
      expect(result.error).to be_nil
      expect(job.aasm.current_state).to eq(:waiting)
    end

    it 'starts a fresh deadline schedule when the object is duplicated' do
      job, _clock = build(TimeoutExample)
      copy = job.dup
      expect(copy.aasm.current_state).to eq(:waiting)
      expect(copy.aasm.timeout_schedule.entry.token).to eq(1)
      expect(copy.aasm.timeout_schedule.entry).not_to equal(job.aasm.timeout_schedule.entry)
      # cancelling on one object must not affect the independent copy
      copy.aasm.cancel_timeout
      expect(job.aasm.time_remaining(200.0)).to eq(0)
    end

    it 'refuses to retry before the deadline' do
      job, clock = build(TimeoutGuardedExample)
      expect { job.aasm.retry_timeout(clock.now + 1) }.to raise_error(
        AASM::InvalidTimeoutOperation, /before its deadline/
      )
    end

    it 'returns an error result when a callback raises, keeping the deadline' do
      job, clock = build(TimeoutRaisingExample)
      clock.advance(10)
      result = job.aasm.time_out(clock.now)
      expect(result).to be_error
      expect(result).to be_retryable
      expect(result.error).to be_a(StandardError)
      expect(job.aasm.current_state).to eq(:waiting)
    end

    it 'returns an error result when the timeout event is not defined' do
      job, clock = build(TimeoutMissingEventExample)
      clock.advance(10)
      result = job.aasm.time_out(clock.now)
      expect(result).to be_error
      expect(result.error).to be_a(AASM::InvalidTimeoutOperation)
      expect(job.aasm.current_state).to eq(:waiting)
    end

    it 'keeps moving forward when an after-callback raises after the state is entered' do
      job, clock = build(TimeoutAfterRaisesExample)
      clock.advance(10)
      result = job.aasm.time_out(clock.now)
      expect(result).to be_error
      expect(result.error).to be_a(StandardError)
      # state was entered before the after callback raised, so the fresh
      # deadline belongs to :completed, not the old :waiting record
      expect(job.aasm.current_state).to eq(:completed)
      expect(job.aasm.time_remaining(clock.now)).to eq(10)
    end
  end

  describe 'cancellation' do
    it 'cancels the outstanding deadline without leaving the state' do
      job, clock = build(TimeoutExample)
      result = job.aasm.cancel_timeout
      expect(result).to be_cancelled
      expect(result.state).to eq(:waiting)
      expect(job.aasm.current_state).to eq(:waiting)
      expect(job.aasm.time_remaining(clock.now + 100)).to be_nil
      expect(job.aasm.time_out(clock.now + 100)).to be_none
    end

    it 'returns :none when cancelling twice' do
      job, _clock = build(TimeoutExample)
      expect(job.aasm.cancel_timeout).to be_cancelled
      expect(job.aasm.cancel_timeout).to be_none
    end
  end

  describe 'named state machines' do
    it 'arms and advances the deadline independently per machine name' do
      clock = ManualMonotonicClock.new(0)
      job = TimeoutNamedMachineExample.new(clock)
      job.aasm(:work).time_remaining
      clock.advance(2)
      result = job.aasm(:work).time_out(clock.now)
      expect(result).to be_fired
      expect(job.aasm(:work).current_state).to eq(:completed)
    end
  end
end
