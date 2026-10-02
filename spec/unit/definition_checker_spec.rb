require 'spec_helper'

class DefinitionCheckerClean
  include AASM

  aasm do
    state :idle, :initial => true
    state :running
    state :done

    event :start do
      transitions :from => :idle, :to => :running
    end

    event :finish do
      transitions :from => :running, :to => :done
    end
  end
end

class DefinitionCheckerProblematic
  include AASM

  aasm do
    state :idle, :initial => true
    state :running
    state :orphan

    event :start do
      transitions :from => :idle, :to => :running
      transitions :from => :idle, :to => :running
    end

    event :reset do
      transitions :from => :running, :to => :idle
      transitions :from => :idle, :to => :limbo
    end

    event :revive do
      transitions :from => :ghost, :to => :idle
    end
  end
end

class DefinitionCheckerGuarded
  include AASM

  aasm do
    state :pending, :initial => true
    state :fast_tracked
    state :approved

    event :approve do
      transitions :from => :pending, :to => :fast_tracked, :guard => :vip?
      transitions :from => :pending, :to => :approved
    end
  end

  def vip?
    false
  end
end

class DefinitionCheckerAnyState
  include AASM

  aasm do
    state :active, :initial => true
    state :suspended
    state :archived

    event :suspend do
      transitions :from => :active, :to => :suspended
    end

    event :archive do
      transitions :to => :archived
    end
  end
end

class DefinitionCheckerAnyStateShadow
  include AASM

  aasm do
    state :active, :initial => true
    state :archived

    event :archive do
      transitions :to => :archived
      transitions :from => :active, :to => :active
    end
  end
end

class DefinitionCheckerDynamicInitial
  include AASM

  aasm do
    state :retired
    state :working
    initial_state Proc.new { :working }

    event :retire do
      transitions :from => :working, :to => :retired
    end
  end
end

class DefinitionCheckerMulti
  include AASM

  aasm :zebra do
    state :a, :initial => true
    state :b

    event :go do
      transitions :from => :a, :to => :b
    end
  end

  aasm :alpha do
    state :x, :initial => true
    state :y

    event :move do
      transitions :from => :x, :to => :missing
    end
  end
end

class DefinitionCheckerParent
  include AASM

  aasm do
    state :idle, :initial => true
    state :orphan
  end
end

class DefinitionCheckerChild < DefinitionCheckerParent
end

describe AASM::DefinitionChecker do
  def definition_snapshot(klass)
    store = AASM::StateMachineStore.fetch(klass, true)
    store.machine_names.sort.map do |name|
      machine = store.machine(name)
      [
        name,
        machine.initial_state,
        machine.states.map(&:name),
        machine.events.map do |event_name, event|
          [event_name, event.transitions.map { |t| [t.from, t.to] }]
        end
      ]
    end
  end

  def behavior_sample(klass, events)
    obj = klass.new
    [obj.aasm.current_state] + events.map do |event|
      obj.send(event)
      obj.aasm.current_state
    end
  end

  it 'reports no issues for a clean machine with a terminal state' do
    report = AASM::DefinitionChecker.check(DefinitionCheckerClean)
    expect(report.issues).to be_empty
    expect(report.errors).to be_empty
    expect(report.warnings).to be_empty
    expect(report).to be_ok
  end

  it 'reports transitions referencing undeclared states as errors' do
    report = AASM::DefinitionChecker.check(DefinitionCheckerProblematic)
    expect(report).not_to be_ok

    errors = report.errors
    expect(errors.map(&:type)).to eq([:undeclared_state, :undeclared_state])
    expect(errors.map(&:event_name)).to eq([:reset, :revive])
    expect(errors[0].message).to include(':limbo')
    expect(errors[1].message).to include(':ghost')
  end

  it 'reports states unreachable from the initial state as warnings' do
    report = AASM::DefinitionChecker.check(DefinitionCheckerProblematic)
    unreachable = report.warnings.select { |i| i.type == :unreachable_state }
    expect(unreachable.size).to eq(1)
    expect(unreachable.first.message).to include(':orphan')
    expect(unreachable.first.message).to include(':idle')
  end

  it 'reports candidates fully shadowed by earlier unconditional transitions' do
    report = AASM::DefinitionChecker.check(DefinitionCheckerProblematic)
    shadowed = report.warnings.select { |i| i.type == :shadowed_transition }
    expect(shadowed.size).to eq(1)
    expect(shadowed.first.event_name).to eq(:start)
  end

  it 'orders issues by machine name, event declaration order and transition order' do
    report = AASM::DefinitionChecker.check(DefinitionCheckerProblematic)
    expect(report.issues.map { |i| [i.severity, i.type, i.event_name] }).to eq([
      [:warning, :shadowed_transition, :start],
      [:error, :undeclared_state, :reset],
      [:error, :undeclared_state, :revive],
      [:warning, :unreachable_state, nil]
    ])
  end

  it 'treats a conditional preferred transition as passable and non-shadowing' do
    report = AASM::DefinitionChecker.check(DefinitionCheckerGuarded)
    expect(report.issues).to be_empty
    expect(report).to be_ok
  end

  it 'includes transitions from any state in the analysis' do
    report = AASM::DefinitionChecker.check(DefinitionCheckerAnyState)
    expect(report.issues).to be_empty
  end

  it 'reports transitions shadowed by an earlier unconditional any-state transition' do
    report = AASM::DefinitionChecker.check(DefinitionCheckerAnyStateShadow)
    expect(report.errors).to be_empty
    shadowed = report.warnings.select { |i| i.type == :shadowed_transition }
    expect(shadowed.size).to eq(1)
    expect(shadowed.first.event_name).to eq(:archive)
  end

  it 'marks the reachability check as inconclusive for dynamic initial states' do
    report = AASM::DefinitionChecker.check(DefinitionCheckerDynamicInitial)
    expect(report.errors).to be_empty
    expect(report.issues.size).to eq(1)
    issue = report.issues.first
    expect(issue).to be_warning
    expect(issue.type).to eq(:dynamic_initial_state)
  end

  it 'checks every named machine, ordered by machine name' do
    report = AASM::DefinitionChecker.check(DefinitionCheckerMulti)
    expect(report.issues.map(&:machine_name)).to eq([:alpha, :alpha])
    expect(report.issues.map(&:type)).to eq([:undeclared_state, :unreachable_state])
    expect(report.issues.first.event_name).to eq(:move)
  end

  it 'checks inherited state machine definitions' do
    report = AASM::DefinitionChecker.check(DefinitionCheckerChild)
    expect(report.errors).to be_empty
    expect(report.issues.size).to eq(1)
    expect(report.issues.first.type).to eq(:unreachable_state)
    expect(report.issues.first.message).to include(':orphan')
  end

  it 'returns an empty report for classes without state machines' do
    report = AASM::DefinitionChecker.check(String)
    expect(report.issues).to be_empty
    expect(report).to be_ok
  end

  it 'renders issues as readable strings' do
    report = AASM::DefinitionChecker.check(DefinitionCheckerProblematic)
    lines = report.issues.map(&:to_s)
    expect(lines.first).to include('WARNING')
    expect(lines.first).to include('machine :default')
    expect(lines.first).to include('event :start')
    expect(lines.join("\n")).to include('ERROR')
  end

  it 'leaves definitions and real event behavior unchanged' do
    before_snapshot = definition_snapshot(DefinitionCheckerProblematic)
    before_behavior = behavior_sample(DefinitionCheckerProblematic, [:start, :reset])

    AASM::DefinitionChecker.check(DefinitionCheckerProblematic)

    expect(definition_snapshot(DefinitionCheckerProblematic)).to eq(before_snapshot)
    expect(behavior_sample(DefinitionCheckerProblematic, [:start, :reset])).to eq(before_behavior)
    expect(before_behavior).to eq([:idle, :running, :idle])
  end

  it 'does not evaluate guards while checking' do
    before_behavior = behavior_sample(DefinitionCheckerGuarded, [:approve])

    report = AASM::DefinitionChecker.check(DefinitionCheckerGuarded)

    expect(report).to be_ok
    expect(behavior_sample(DefinitionCheckerGuarded, [:approve])).to eq(before_behavior)
    expect(before_behavior).to eq([:pending, :approved])
  end
end
