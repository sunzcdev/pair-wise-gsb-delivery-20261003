# frozen_string_literal: true

require 'spec_helper'

class AvailabilityExample
  include AASM

  attr_accessor :allow_run, :allow_finish, :block_finish, :finish_guard_ok
  attr_reader :calls

  def initialize
    @calls = Hash.new(0)
    @allow_run = false
    @allow_finish = false
    @block_finish = false
    @finish_guard_ok = true
    super
  end

  aasm do
    state :idle, initial: true
    state :running
    state :cleaning
    state :done

    # An event level guard shared by both candidate transitions, plus per
    # transition guards, so the short-circuit and reuse paths are exercised.
    event :run, :guards => [:event_guard] do
      transitions :from => :idle, :to => :running,  :guard => :can_run
      transitions :from => :idle, :to => :cleaning, :guard => :can_clean
    end

    event :finish do
      transitions :from => :idle, :to => :done,
                  # :allow_finish_value is listed twice on purpose: it must
                  # still be invoked only once within this candidate.
                  :guard => [:allow_finish_value, :allow_finish_value, :finish_guard],
                  :unless => [:block_finish_value, :second_unless]
    end

    event :restart do
      transitions :from => :done, :to => :idle
    end

    event :after do
      transitions :from => :idle, :to => :done, :after => :after_callback
    end
  end

  def event_guard
    @calls[:event_guard] += 1
    true
  end

  def can_run
    @calls[:can_run] += 1
    allow_run
  end

  def can_clean
    @calls[:can_clean] += 1
    false
  end

  def allow_finish_value
    @calls[:allow_finish] += 1
    allow_finish
  end

  def finish_guard
    @calls[:finish_guard] += 1
    finish_guard_ok
  end

  def block_finish_value
    @calls[:block_finish] += 1
    block_finish
  end

  def second_unless
    @calls[:second_unless] += 1
    false
  end

  def after_callback
    @calls[:after_callback] += 1
  end
end

class NamedAvailabilityExample
  include AASM

  attr_accessor :allowed
  attr_reader :calls

  def initialize
    @calls = Hash.new(0)
    @allowed = false
    super
  end

  aasm(:workflow) do
    state :pending, initial: true
    state :approved

    event :approve do
      transitions :from => :pending, :to => :approved, :guard => :approved?
    end
  end

  def approved?
    @calls[:approved?] += 1
    allowed
  end
end

class SharedBranchAvailabilityExample
  include AASM

  attr_accessor :value
  attr_reader :calls

  def initialize
    @calls = Hash.new(0)
    @value = true
    super
  end

  aasm do
    state :idle, initial: true
    state :done

    event :go do
      transitions :from => :idle, :to => :done,
                  :guard => [:always_true, :check_value],
                  :unless => :check_value
    end

    event :stop do
      transitions :from => :idle, :to => :done, :unless => :check_value
    end
  end

  def always_true
    @calls[:always_true] += 1
    true
  end

  # Used both as a guard and as an :unless callback of the same candidate.
  def check_value
    @calls[:check_value] += 1
    value
  end
end

describe 'AASM::InstanceBase#availability' do
  let(:example) { AvailabilityExample.new }

  it 'returns permitted events and transitions in one result' do
    result = example.aasm.availability

    expect(result).to be_a(AASM::Availability)
    expect(result.permitted_events).to contain_exactly(:after)
    expect(result.events.map(&:name)).to contain_exactly(:after)
    expect(result.transitions).to contain_exactly(
      :event => :after,
      :state => :done,
      :transition => an_instance_of(AASM::Core::Transition)
    )
  end

  it 'only lists events that have a candidate transition from the current state' do
    result = example.aasm.availability
    events = result.candidates.map { |candidate| candidate[:event] }.uniq

    expect(events).to contain_exactly(:run, :finish, :after)
    expect(events).not_to include(:restart)
  end

  context 'guard invocation counts' do
    it 'invokes an event level guard only once across all candidate transitions' do
      example.aasm.availability

      expect(example.calls[:event_guard]).to eq(1)
      expect(example.calls[:can_run]).to eq(1)
      expect(example.calls[:can_clean]).to eq(1)
    end

    it 'invokes the same guard only once within one candidate transition' do
      example.allow_finish = true
      example.aasm.availability

      expect(example.calls[:allow_finish]).to eq(1)
      expect(example.calls[:finish_guard]).to eq(1)
    end

    it 'marks the shared event guard as reused on the second candidate' do
      result = example.aasm.availability

      run_candidates = result.candidates.select { |candidate| candidate[:event] == :run }
      statuses = run_candidates.map do |candidate|
        candidate[:checks].find { |check| check[:subject] == :event_guard }[:status]
      end

      expect(statuses).to eq([:invoked, :reused])
    end

    it 're-runs every guard on a second inspection instead of caching the first result' do
      example.allow_run = true
      first = example.aasm.availability
      expect(first.permitted_events).to contain_exactly(:run, :after)

      example.allow_run = false
      second = example.aasm.availability
      expect(second.permitted_events).to contain_exactly(:after)
      expect(example.calls[:event_guard]).to eq(2)
      expect(example.calls[:can_run]).to eq(2)
    end
  end

  context 'short-circuit rules' do
    it 'evaluates guards before unless guards and marks unevaluated unless guards as skipped' do
      # allow_finish_value fails, so never_passes and the whole :unless list
      # must never run.
      result = example.aasm.availability

      rejection = result.rejections.find { |entry| entry[:event] == :finish }
      expect(rejection[:guard]).to eq(:allow_finish_value)
      expect(rejection[:reason]).to eq(:allow_finish_value)

      checks = rejection[:checks]
      expect(checks.select { |check| check[:subject] == :allow_finish_value }.map { |check| check[:status] })
        .to eq([:invoked, :skipped])
      expect(example.calls[:allow_finish]).to eq(1)
      expect(checks.find { |check| check[:subject] == :finish_guard }[:status]).to eq(:skipped)
      expect(checks.find { |check| check[:subject] == :block_finish_value }[:status]).to eq(:skipped)
      expect(checks.find { |check| check[:subject] == :second_unless }[:status]).to eq(:skipped)

      expect(example.calls[:finish_guard]).to eq(0)
      expect(example.calls[:block_finish]).to eq(0)
      expect(example.calls[:second_unless]).to eq(0)
    end

    it 'short-circuits the guard list on the first failing guard' do
      example.allow_finish = true
      example.finish_guard_ok = false
      result = example.aasm.availability

      rejection = result.rejections.find { |entry| entry[:event] == :finish }
      allow_check = rejection[:checks].find { |check| check[:subject] == :allow_finish_value }
      finish_check = rejection[:checks].find { |check| check[:subject] == :finish_guard }
      unless_check = rejection[:checks].find { |check| check[:subject] == :block_finish_value }

      expect(allow_check[:status]).to eq(:invoked)
      expect(allow_check[:result]).to eq(true)
      expect(finish_check[:status]).to eq(:invoked)
      expect(finish_check[:result]).to eq(false)
      # Guards failed, so unless guards are skipped (Transition#allowed? rule).
      expect(unless_check[:status]).to eq(:skipped)
      expect(unless_check[:result]).to eq(:not_evaluated)
    end

    it 'rejects when an unless callback is truthy and skips following unless callbacks' do
      example.allow_finish = true
      example.block_finish = true
      result = example.aasm.availability

      rejection = result.rejections.find { |entry| entry[:event] == :finish }
      expect(rejection[:guard]).to eq(:block_finish_value)
      block_check = rejection[:checks].find { |check| check[:subject] == :block_finish_value }
      second_check = rejection[:checks].find { |check| check[:subject] == :second_unless }
      expect(block_check[:status]).to eq(:invoked)
      expect(block_check[:result]).to eq(true)
      expect(second_check[:status]).to eq(:skipped)
      expect(second_check[:result]).to eq(:not_evaluated)
      expect(example.calls[:second_unless]).to eq(0)
    end

    it 'never records a fabricated false result for skipped guards' do
      result = example.aasm.availability

      result.candidates.each do |candidate|
        candidate[:checks].each do |check|
          if check[:status] == :skipped
            expect(check[:result]).to eq(:not_evaluated)
          end
        end
      end
    end
  end

  context 'rejection reasons' do
    it 'reports one rejection per rejected candidate transition' do
      result = example.aasm.availability

      expect(result.rejections.map { |entry| [entry[:event], entry[:state]] }).to contain_exactly(
        [:run, :running],
        [:run, :cleaning],
        [:finish, :done]
      )
    end

    it 'reports the reused event guard as the reason only when it fails' do
      result = example.aasm.availability

      run_running = result.rejections.find { |entry| entry[:event] == :run && entry[:state] == :running }
      run_cleaning = result.rejections.find { |entry| entry[:event] == :run && entry[:state] == :cleaning }

      expect(run_running[:reason]).to eq(:can_run)
      expect(run_cleaning[:reason]).to eq(:can_clean)
    end

    it 'does not mix records from another object' do
      other = AvailabilityExample.new

      result = example.aasm.availability
      other_result = other.aasm.availability

      # Both objects are inspected independently and get their own rejection
      # records, even though they share the same class-level transitions.
      expect(other_result.rejections.size).to eq(3)
      expect(result.rejections.size).to eq(3)
      expect(result.rejections).not_to equal(other_result.rejections)

      other.allow_run = true
      changed_result = other.aasm.availability

      # A later inspection of another object cannot leak back into a snapshot
      # that was already returned.
      run_rejection = result.rejections_for(:run).find { |entry| entry[:state] == :running }
      expect(run_rejection[:reason]).to eq(:can_run)
      expect(run_rejection[:checks].find { |check| check[:subject] == :can_run }[:result])
        .to eq(false)

      changed_run = changed_result.candidates.find do |candidate|
        candidate[:event] == :run && candidate[:state] == :running
      end
      expect(changed_run[:allowed]).to eq(true)

      # Inspections use local failure buffers instead of Transition#failures.
      changed_result.candidates.each do |candidate|
        expect(candidate[:transition].failures).to eq([])
      end
    end
  end

  context 'side effects' do
    it 'does not fire transition or state callbacks' do
      example.aasm.availability

      expect(example.aasm.current_state).to eq(:idle)
      expect(example.calls[:after_callback]).to eq(0)
    end

    it 'does not treat the snapshot as authorization for a later fire' do
      result = example.aasm.availability
      expect(result.permitted_events).to contain_exactly(:after)

      expect { example.run! }.to raise_error(AASM::InvalidTransition)
      expect(example.aasm.current_state).to eq(:idle)
    end
  end

  context 'named state machines' do
    let(:named) { NamedAvailabilityExample.new }

    it 'inspects the named machine' do
      expect(named.aasm(:workflow).availability.permitted_events).to eq([])

      named.allowed = true
      result = named.aasm(:workflow).availability
      expect(result.permitted_events).to eq([:approve])
      expect(result.transitions.first[:state]).to eq(:approved)
    end

    it 'runs the named machine guards again on a consecutive inspection' do
      named.aasm(:workflow).availability
      named.aasm(:workflow).availability

      expect(named.calls[:'approved?']).to eq(2)
    end
  end

  context 'guards shared between branches' do
    let(:shared) { SharedBranchAvailabilityExample.new }

    it 'invokes a guard used as both guard and unless callback only once' do
      # check_value returns true: it passes the guard branch and rejects the
      # candidate through the :unless branch, using the memoized result.
      result = shared.aasm.availability

      expect(shared.calls[:check_value]).to eq(1)
      checks = result.candidates.first[:checks].select do |check|
        check[:subject] == :check_value
      end
      expect(checks.map { |check| check[:status] }).to eq([:invoked, :reused])
      expect(result.permitted_events).to eq([])
      expect(result.rejections.find { |entry| entry[:event] == :go }[:reason])
        .to eq(:check_value)

      # Flipping the object re-evaluates everything on the next inspection.
      shared.value = false
      changed = shared.aasm.availability
      # One invocation for go's guard branch; stop's :unless reuses it.
      expect(shared.calls[:check_value]).to eq(2)
      expect(changed.permitted_events).to eq([:stop])
    end
  end

  context 'guard arguments' do
    it 'passes inspection arguments through to guards' do
      parametrised = Class.new do
        include AASM

        attr_reader :seen

        aasm do
          state :idle, initial: true
          state :done

          event :go do
            transitions :from => :idle, :to => :done,
                        :guard => ->(allowed = nil) { allowed }
          end
        end
      end

      instance = parametrised.new
      expect(instance.aasm.availability(true).permitted_events).to eq([:go])
      expect(instance.aasm.availability(false).permitted_events).to eq([])
    end

    it 'handles nested guard arrays with the same short-circuit and reuse rules' do
      nested = Class.new do
        include AASM

        attr_reader :calls

        def initialize
          @calls = Hash.new(0)
          super
        end

        aasm do
          state :idle, initial: true
          state :done

          event :go, :guards => [[:nested_ok, :nested_ok]] do
            transitions :from => :idle, :to => :done,
                        :guard => [[:always_passes, :always_passes]]
          end
        end

        def nested_ok
          @calls[:nested_ok] += 1
          true
        end

        def always_passes
          @calls[:always_passes] += 1
          true
        end
      end

      instance = nested.new
      result = instance.aasm.availability

      expect(result.permitted_events).to eq([:go])
      expect(instance.calls[:nested_ok]).to eq(1)
      expect(instance.calls[:always_passes]).to eq(1)
    end
  end
end
