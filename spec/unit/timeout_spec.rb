require 'spec_helper'

# A monotonic clock fully controlled by the caller: the library itself
# never reads the wall clock for timeouts when one is configured.
class ManualTimeoutClock
  attr_accessor :now

  def initialize
    @now = 0.0
  end

  def call
    @now
  end
end

class TimeoutExample
  include AASM

  def self.clock
    @clock ||= ManualTimeoutClock.new
  end

  attr_accessor :allow_expire

  def initialize
    @allow_expire = true
  end

  def review_timeout
    15
  end

  aasm :timeout_clock => lambda { TimeoutExample.clock.now } do
    state :waiting, :initial => true, :expires_after => 30, :on_expire => :expire
    state :processing
    state :expired
    state :reviewing, :expires_after => :review_timeout, :on_expire => :close_review
    state :review_closed
    state :doomed, :expires_after => 5, :on_expire => :explode
    state :exploded

    event :expire do
      transitions :from => :waiting, :to => :expired, :guard => :allow_expire
    end

    event :start do
      transitions :from => :waiting, :to => :processing
    end

    event :pause do
      transitions :from => :processing, :to => :waiting
    end

    event :refresh do
      transitions :from => :waiting, :to => :waiting
    end

    event :review do
      transitions :from => :processing, :to => :reviewing
    end

    event :close_review do
      transitions :from => :reviewing, :to => :review_closed
    end

    event :doom do
      transitions :from => :waiting, :to => :doomed
    end

    event :explode do
      transitions :from => :doomed, :to => :exploded
      before do
        raise 'boom'
      end
    end
  end
end

class ZeroWaitTimeoutExample
  include AASM

  def self.clock
    @clock ||= ManualTimeoutClock.new
  end

  aasm :timeout_clock => lambda { ZeroWaitTimeoutExample.clock.now } do
    state :waiting, :initial => true, :expires_after => 10, :on_expire => :expire
    state :expired
    state :alerting, :expires_after => 0, :on_expire => :escalate
    state :escalated

    event :expire do
      transitions :from => :waiting, :to => :expired
      after do
        alert
      end
    end

    event :alert do
      transitions :from => :expired, :to => :alerting
    end

    event :escalate do
      transitions :from => :alerting, :to => :escalated
    end
  end
end

class NamedTimeoutExample
  include AASM

  def self.clock
    @clock ||= ManualTimeoutClock.new
  end

  aasm(:review, :timeout_clock => lambda { NamedTimeoutExample.clock.now }) do
    state :open, :initial => true, :expires_after => 5, :on_expire => :expire
    state :stale

    event :expire do
      transitions :from => :open, :to => :stale
    end
  end
end

class NonWhinyTimeoutExample
  include AASM

  def self.clock
    @clock ||= ManualTimeoutClock.new
  end

  aasm :whiny_transitions => false, :timeout_clock => lambda { NonWhinyTimeoutExample.clock.now } do
    state :waiting, :initial => true, :expires_after => 10, :on_expire => :expire
    state :expired

    event :expire do
      transitions :from => :waiting, :to => :expired, :guard => lambda { false }
    end
  end
end

describe 'state timeouts' do
  let(:clock) { TimeoutExample.clock }
  let(:example) { TimeoutExample.new }

  before { clock.now = 1000.0 }

  describe 'declaration' do
    it 'requires :on_expire together with :expires_after' do
      expect {
        Class.new do
          include AASM
          aasm do
            state :broken, :expires_after => 1
          end
        end
      }.to raise_error(ArgumentError, /expires_after/)
    end

    it 'requires :expires_after together with :on_expire' do
      expect {
        Class.new do
          include AASM
          aasm do
            state :broken, :on_expire => :expire
          end
        end
      }.to raise_error(ArgumentError, /on_expire/)
    end
  end

  describe 'deadline ownership' do
    it 'arms the deadline when the initial state is entered' do
      expect(example.aasm.current_state).to eq(:waiting)
      record = example.aasm.current_timeout
      expect(record.state).to eq(:waiting)
      expect(record.event).to eq(:expire)
      expect(record.armed_at).to eq(1000.0)
      expect(record.deadline).to eq(1030.0)
    end

    it 'voids the deadline when the state is left' do
      example.aasm.current_state
      example.start
      expect(example.aasm.current_timeout).to be_nil
      expect(example.aasm.advance_timeouts(1030.0)).to eq([])
      expect(example.aasm.current_state).to eq(:processing)
    end

    it 'arms a new deadline on re-entry and never fires the old record' do
      example.aasm.current_state # deadline 1030
      clock.now = 1010.0
      example.start
      clock.now = 1020.0
      example.pause # re-entered :waiting at 1020, deadline 1050

      results = example.aasm.advance_timeouts(1035.0)
      expect(results).to eq([])
      expect(example.aasm.current_state).to eq(:waiting)

      results = example.aasm.advance_timeouts(1050.0)
      expect(results.map(&:status)).to eq([:fired])
      expect(example.aasm.current_state).to eq(:expired)
    end

    it 'arms a new deadline on a self transition' do
      example.aasm.current_state # deadline 1030
      clock.now = 1010.0
      example.refresh

      record = example.aasm.current_timeout
      expect(record.armed_at).to eq(1010.0)
      expect(record.deadline).to eq(1040.0)

      expect(example.aasm.advance_timeouts(1030.0)).to eq([])
      expect(example.aasm.current_state).to eq(:waiting)
      expect(example.aasm.advance_timeouts(1040.0).map(&:status)).to eq([:fired])
    end

    it 'keeps the existing deadline on a failed transition' do
      example.aasm.current_state # deadline 1030
      example.allow_expire = false
      clock.now = 1005.0
      expect { example.expire }.to raise_error(AASM::InvalidTransition)

      record = example.aasm.current_timeout
      expect(record.deadline).to eq(1030.0)

      example.allow_expire = true
      expect(example.aasm.advance_timeouts(1030.0).map(&:status)).to eq([:fired])
    end

    it 'supports a method name as :expires_after' do
      example.aasm.current_state
      example.start
      clock.now = 1012.0
      example.review
      expect(example.aasm.current_timeout.deadline).to eq(1027.0)
    end
  end

  describe 'advancement' do
    it 'does not fire before the deadline' do
      example.aasm.current_state
      expect(example.aasm.advance_timeouts(1029.999)).to eq([])
      expect(example.aasm.current_state).to eq(:waiting)
    end

    it 'fires exactly at the deadline' do
      example.aasm.current_state
      results = example.aasm.advance_timeouts(1030.0)
      expect(results.size).to eq(1)
      expect(results.first).to be_fired
      expect(results.first.state_machine).to eq(:default)
      expect(results.first.state).to eq(:waiting)
      expect(results.first.event).to eq(:expire)
      expect(example.aasm.current_state).to eq(:expired)
    end

    it 'returns an explicit rejection result and keeps the record armed' do
      example.aasm.current_state
      example.allow_expire = false

      results = example.aasm.advance_timeouts(1030.0)
      expect(results.size).to eq(1)
      expect(results.first).to be_rejected
      expect(results.first.exception).to be_a(AASM::InvalidTransition)
      expect(example.aasm.current_state).to eq(:waiting)
      expect(example.aasm.current_timeout.deadline).to eq(1030.0)
    end

    it 'allows an explicit retry after a rejection' do
      example.aasm.current_state
      example.allow_expire = false
      example.aasm.advance_timeouts(1030.0)

      example.allow_expire = true
      results = example.aasm.advance_timeouts(1031.0)
      expect(results.map(&:status)).to eq([:fired])
      expect(example.aasm.current_state).to eq(:expired)
    end

    it 'allows an explicit cancel after a rejection' do
      example.aasm.current_state
      example.allow_expire = false
      example.aasm.advance_timeouts(1030.0)

      cancelled = example.aasm.cancel_timeout
      expect(cancelled.state).to eq(:waiting)
      expect(example.aasm.current_timeout).to be_nil
      expect(example.aasm.advance_timeouts(2000.0)).to eq([])
      expect(example.aasm.current_state).to eq(:waiting)
    end

    it 'returns an explicit error result when the event raises' do
      example.aasm.current_state
      example.doom # deadline 1005

      results = example.aasm.advance_timeouts(1005.0)
      expect(results.size).to eq(1)
      expect(results.first).to be_error
      expect(results.first.exception.message).to eq('boom')
      expect(example.aasm.current_state).to eq(:doomed)
      expect(example.aasm.current_timeout.deadline).to eq(1005.0)
    end

    it 'uses the configured clock when no time is passed' do
      example.aasm.current_state
      clock.now = 1030.0
      expect(example.aasm.advance_timeouts.map(&:status)).to eq([:fired])
    end

    it 'reports rejections without exception when whiny_transitions is off' do
      NonWhinyTimeoutExample.clock.now = 0.0
      non_whiny = NonWhinyTimeoutExample.new
      non_whiny.aasm.current_state

      results = non_whiny.aasm.advance_timeouts(10.0)
      expect(results.size).to eq(1)
      expect(results.first).to be_rejected
      expect(results.first.exception).to be_nil
      expect(non_whiny.aasm.current_state).to eq(:waiting)
      expect(non_whiny.aasm.current_timeout).not_to be_nil
    end
  end

  describe 'single pass semantics' do
    let(:zero_clock) { ZeroWaitTimeoutExample.clock }
    let(:zero) { ZeroWaitTimeoutExample.new }

    before { zero_clock.now = 100.0 }

    it 'leaves zero-wait states entered from callbacks to the next pass' do
      zero.aasm.current_state # deadline 110
      zero_clock.now = 110.0

      results = zero.aasm.advance_timeouts
      expect(results.size).to eq(1)
      expect(results.first.event).to eq(:expire)
      expect(zero.aasm.current_state).to eq(:alerting)

      # :alerting was armed (with a zero wait) during the pass, so it was
      # not processed even though its deadline is already due
      record = zero.aasm.current_timeout
      expect(record.state).to eq(:alerting)
      expect(record.deadline).to eq(110.0)

      results = zero.aasm.advance_timeouts
      expect(results.map(&:event)).to eq([:escalate])
      expect(zero.aasm.current_state).to eq(:escalated)
    end
  end

  describe 'queries' do
    it 'reports the remaining time without firing anything' do
      example.aasm.current_state
      expect(example.aasm.timeout_remaining(1010.0)).to eq(20.0)
      expect(example.aasm.timeout_remaining(1030.0)).to eq(0)
      expect(example.aasm.timeout_remaining(2000.0)).to eq(0)
      expect(example.aasm.current_state).to eq(:waiting)
    end

    it 'reports expiry without firing anything' do
      example.aasm.current_state
      expect(example.aasm.timeout_expired?(1029.999)).to be(false)
      expect(example.aasm.timeout_expired?(1030.0)).to be(true)
      expect(example.aasm.current_state).to eq(:waiting)
    end

    it 'reports no timeout for states without a declaration' do
      example.aasm.current_state
      example.start
      expect(example.aasm.timeout_remaining(1010.0)).to be_nil
      expect(example.aasm.timeout_expired?(9999.0)).to be(false)
    end
  end

  describe 'named state machines' do
    let(:named_clock) { NamedTimeoutExample.clock }
    let(:named) { NamedTimeoutExample.new }

    before { named_clock.now = 50.0 }

    it 'tracks deadlines per state machine' do
      expect(named.aasm(:review).current_state).to eq(:open)
      expect(named.aasm(:review).timeout_remaining(52.0)).to eq(3.0)

      results = named.aasm(:review).advance_timeouts(55.0)
      expect(results.size).to eq(1)
      expect(results.first).to be_fired
      expect(results.first.state_machine).to eq(:review)
      expect(named.aasm(:review).current_state).to eq(:stale)
    end
  end
end
