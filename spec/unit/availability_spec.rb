# frozen_string_literal: true

require 'spec_helper'

class AvailabilityGate
  include AASM

  attr_accessor :allow_alpha, :allow_beta, :blocked, :counter

  def initialize
    @allow_alpha = false
    @allow_beta = false
    @blocked = false
    @counter = 0
  end

  aasm do
    state :idle, initial: true
    state :alpha
    state :beta
    state :done

    event :choose do
      transitions from: :idle, to: :alpha, guard: :allow_alpha?
      transitions from: :idle, to: :beta, guard: :allow_beta?
    end

    event :finish do
      transitions from: :idle, to: :done, unless: :blocked?
    end

    event :stuck do
      transitions from: :idle, to: :done, guards: [:counting_guard, :second_guard]
    end
  end

  def allow_alpha?
    @allow_alpha
  end

  def allow_beta?
    @allow_beta
  end

  def blocked?
    @blocked
  end

  def counting_guard
    @counter += 1
    false
  end

  def second_guard
    @counter += 1
    true
  end
end

class AvailabilityShared
  include AASM

  attr_accessor :calls

  def initialize
    @calls = 0
  end

  aasm do
    state :idle, initial: true
    state :one
    state :two

    event :shared_event, guards: :shared_guard do
      transitions from: :idle, to: :one
      transitions from: :idle, to: :two
    end
  end

  def shared_guard
    @calls += 1
    true
  end
end

class AvailabilityNoCallbacks
  include AASM

  attr_reader :state_callbacks, :transition_callbacks

  def initialize
    @state_callbacks = []
    @transition_callbacks = []
  end

  aasm do
    state :idle, initial: true
    state :busy, before_enter: -> { record_state_callback(:busy_before_enter) },
                 after_enter: -> { record_state_callback(:busy_after_enter) },
                 before_exit: -> { record_state_callback(:busy_before_exit) }

    event :work do
      transitions from: :idle, to: :busy, after: -> { record_transition_callback }
    end
  end

  private

  def record_state_callback(name)
    @state_callbacks << name
  end

  def record_transition_callback
    @transition_callbacks << :transition_after
  end
end

class NamedAvailabilityMachine
  include AASM

  attr_accessor :allowed

  def initialize
    @allowed = false
  end

  aasm(:traffic) do
    state :red, initial: true
    state :green

    event :go do
      transitions from: :red, to: :green, guard: :allowed
    end
  end

  def allowed
    @allowed
  end
end

describe 'AASM::InstanceBase#check' do
  let(:gate) { AvailabilityGate.new }

  it 'collects allowed events, transitions and states in one result' do
    gate.allow_alpha = true

    result = gate.aasm.check

    expect(result.allowed_events.map(&:name)).to eq([:choose, :finish])
    expect(result.allowed_transitions).to eq([
      {event: :choose, state: :alpha},
      {event: :finish, state: :done}
    ])
    expect(result.allowed_states.map(&:name)).to eq([:alpha, :done])
  end

  it 'evaluates several candidate transitions consistently' do
    result = gate.aasm.check

    expect(result.allowed_events.map(&:name)).to eq([:finish])
    expect(result.allowed_transitions).to eq([{event: :finish, state: :done}])
    expect(result.allowed_states.map(&:name)).to eq([:done])
    expect(result.blocked_events.map(&:name)).to eq([:choose, :stuck])
    expect(result.rejections.keys).to eq([:choose, :stuck])

    choose = result.event(:choose)
    expect(choose.transitions.map(&:to)).to eq([:alpha, :beta])
    expect(choose.transitions).to all(be_blocked)
    expect(choose.transitions.flat_map { |t| t.blocked_by.map(&:subject) })
      .to eq([:allow_alpha?, :allow_beta?])

    stuck = result.event(:stuck)
    expect(stuck.blocked_transitions.first.blocked_by.map(&:subject))
      .to eq([:counting_guard])
  end

  it 'runs every candidate guard once and reports not_run on short circuit' do
    gate.aasm.check
    expect(gate.counter).to eq(1)

    stuck = gate.aasm.check.event(:stuck)
    guard_checks = stuck.transitions.first.guards
    expect(guard_checks.map(&:subject)).to eq([:counting_guard, :second_guard])
    expect(guard_checks.map(&:status)).to eq([:failed, :not_run])
    expect(guard_checks.map(&:invoked?)).to eq([true, false])

    expect(gate.counter).to eq(2)
  end

  it 'shares an event-level guard across its candidate transitions' do
    shared = AvailabilityShared.new

    result = shared.aasm.check

    expect(result.allowed_transitions).to eq([
      {event: :shared_event, state: :one},
      {event: :shared_event, state: :two}
    ])
    expect(shared.calls).to eq(1)

    checks = result.event(:shared_event).transitions.flat_map(&:guards)
    expect(checks.map(&:subject)).to eq([:shared_guard, :shared_guard])
    expect(checks.map(&:invoked?)).to eq([true, false])
    expect(checks.map(&:status)).to eq([:passed, :passed])
  end

  it 'understands unless guards and skips them when an if guard fails' do
    gate.blocked = true

    finish = gate.aasm.check.event(:finish)
    unless_check = finish.transitions.first.guards.first
    expect(unless_check.kind).to eq(:unless)
    expect(unless_check.status).to eq(:failed)
    expect(unless_check.invoked?).to eq(true)

    stuck = gate.aasm.check.event(:stuck)
    kinds = stuck.transitions.first.guards.map(&:kind)
    expect(kinds).to eq([:if, :if])
  end

  it 'skips the unless chain when the if chain fails and never fabricates failure' do
    machine = Class.new do
      include AASM

      attr_reader :unless_calls

      def initialize
        @unless_calls = 0
      end

      def aasm_read_state(_name = :default)
        @state || :idle
      end

      def aasm_write_state_without_persistence(state, _name = :default)
        @state = state
      end

      def failing_if
        false
      end

      def counting_unless
        @unless_calls += 1
        false
      end

      aasm do
        state :idle, initial: true
        state :done

        event :move do
          transitions from: :idle, to: :done,
                      guard: :failing_if, unless: :counting_unless
        end
      end
    end

    object = machine.new
    checks = object.aasm.check.event(:move).transitions.first.guards

    expect(checks.map(&:kind)).to eq([:if, :unless])
    expect(checks.map(&:status)).to eq([:failed, :not_run])
    expect(checks.map(&:invoked?)).to eq([true, false])
    expect(object.unless_calls).to eq(0)
  end

  it 'runs a guard shared across events only once per check' do
    machine = Class.new do
      include AASM

      attr_reader :calls

      def initialize
        @calls = 0
      end

      def shared_guard
        @calls += 1
        true
      end

      aasm do
        state :idle, initial: true
        state :one
        state :two

        event :first, guards: :shared_guard do
          transitions from: :idle, to: :one
        end

        event :second, guards: :shared_guard do
          transitions from: :idle, to: :two
        end
      end
    end

    object = machine.new
    result = object.aasm.check

    expect(result.allowed_transitions).to eq([
      {event: :first, state: :one},
      {event: :second, state: :two}
    ])
    expect(object.calls).to eq(1)
  end

  it 're-evaluates guards on consecutive checks instead of caching authorization' do
    first = gate.aasm.check
    expect(first.may_fire?(:choose)).to eq(false)

    gate.allow_alpha = true

    second = gate.aasm.check
    expect(second.may_fire?(:choose)).to eq(true)
    expect(first.may_fire?(:choose)).to eq(false)
    expect(second.allowed_events.map(&:name)).to eq([:choose, :finish])
  end

  it 'does not execute transition actions or state callbacks' do
    no_callbacks = AvailabilityNoCallbacks.new

    result = no_callbacks.aasm.check

    expect(result.allowed_events.map(&:name)).to eq([:work])
    expect(no_callbacks.aasm.current_state).to eq(:idle)
    expect(no_callbacks.state_callbacks).to be_empty
    expect(no_callbacks.transition_callbacks).to be_empty
  end

  it 'supports named state machines' do
    machine = NamedAvailabilityMachine.new

    result = machine.aasm(:traffic).check
    expect(result.state_machine_name).to eq(:traffic)
    expect(result.current_state).to eq(:red)
    expect(result.allowed_events).to be_empty

    machine.allowed = true

    result = machine.aasm(:traffic).check
    expect(result.allowed_events.map(&:name)).to eq([:go])
    expect(result.allowed_transitions).to eq([{event: :go, state: :green}])
    expect(result.allowed_states.map(&:name)).to eq([:green])
  end

  it 'keeps rejection reasons isolated between objects' do
    gate.allow_alpha = true

    first = gate.aasm.check
    other = AvailabilityGate.new
    second = other.aasm.check

    expect(first.rejections.keys).to eq([:stuck])
    expect(second.rejections.keys).to eq([:choose, :stuck])
    expect(second.rejections[:choose].map(&:to)).to eq([:alpha, :beta])
    expect(first.rejections).not_to have_key(:choose)
  end

  it 'passes check arguments through to guards' do
    argument_gate = Class.new do
      include AASM

      def aasm_read_state(_name = :default)
        @state || :idle
      end

      def aasm_write_state_without_persistence(state, _name = :default)
        @state = state
      end

      def allowed?(value)
        value
      end

      aasm do
        state :idle, initial: true
        state :done

        event :finish do
          transitions from: :idle, to: :done, guard: :allowed?
        end
      end
    end

    object = argument_gate.new

    expect(object.aasm.check(false).may_fire?(:finish)).to eq(false)
    expect(object.aasm.check(true).may_fire?(:finish)).to eq(true)
  end

  it 'only lists events with a candidate transition from the current state' do
    gate.allow_alpha = true
    gate.allow_beta = true
    gate.choose!

    result = gate.aasm.check
    expect(result.events.map(&:name)).to be_empty
    expect(result.allowed_events).to be_empty
    expect(result.rejections).to be_empty
  end

  it 'expands an array of target states while running its guard once' do
    calls = 0
    machine = Class.new do
      include AASM

      def initialize(calls)
        @calls = calls
      end

      def aasm_read_state(_name = :default)
        @state || :open
      end

      def aasm_write_state_without_persistence(state, _name = :default)
        @state = state
      end

      def allow?
        @calls << true
        true
      end

      aasm do
        state :open, initial: true
        state :a
        state :b

        event :split do
          transitions from: :open, to: [:a, :b], guard: :allow?
        end
      end
    end

    calls = []
    object = machine.new(calls)
    result = object.aasm.check

    expect(result.allowed_transitions).to eq([
      {event: :split, state: :a},
      {event: :split, state: :b}
    ])
    expect(result.allowed_states.map(&:name)).to eq([:a, :b])
    expect(calls.size).to eq(1)
  end
end
