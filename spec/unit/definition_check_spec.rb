# frozen_string_literal: true

require 'spec_helper'

describe 'AASM::DefinitionCheck' do
  def check(klass, machine_name = nil)
    if machine_name
      AASM::DefinitionCheck.check(klass, machine_name)
    else
      AASM::DefinitionCheck.check_all(klass)
    end
  end

  describe 'a clean definition' do
    let(:clean_class) do
      Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :running
          state :finished

          event :start do
            transitions :from => :pending, :to => :running
          end

          event :finish do
            transitions :from => :running, :to => :finished
          end
        end
      end
    end

    it 'reports no issues' do
      report = check(clean_class)
      expect(report).to be_empty
      expect(report).to be_ok
      expect(report.errors).to be_empty
      expect(report.warnings).to be_empty
      expect(report.to_s).to eq('')
    end

    it 'treats a state without outgoing transitions as a normal terminal state' do
      terminal_class = Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :done

          event :finish do
            transitions :from => :pending, :to => :done
          end
        end
      end

      expect(check(terminal_class)).to be_empty
    end
  end

  describe 'undeclared states' do
    it 'reports a transition targeting an undeclared state as an error' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending, :initial => true

          event :start do
            transitions :from => :pending, :to => :running
          end
        end
      end

      report = check(klass)
      expect(report).not_to be_ok

      issue = report.issues.first
      expect(issue.severity).to eq(:error)
      expect(issue.code).to eq(:unknown_to_state)
      expect(issue.machine).to eq(:default)
      expect(issue.event).to eq(:start)
      expect(issue.transition_index).to eq(0)
      expect(issue.from).to eq(:pending)
      expect(issue.to).to eq(:running)
      expect(issue.state).to be_nil
      expect(issue.message).to include("'default'")
      expect(issue.message).to include("':start'")
      expect(issue.message).to include("':running'")
    end

    it 'reports a transition starting from an undeclared state as an error' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :running

          event :start do
            transitions :from => :ghost, :to => :running
          end
        end
      end

      issues = check(klass).errors
      expect(issues.map(&:code)).to eq([:unknown_from_state])
      expect(issues.first.from).to eq(:ghost)
      expect(issues.first.to).to eq(:running)
    end

    it 'reports every undeclared target of an array-valued :to' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :running

          event :start do
            transitions :from => :pending, :to => [:running, :missing_a, :missing_b]
          end
        end
      end

      issues = check(klass).errors
      expect(issues.map(&:to)).to eq([:missing_a, :missing_b])
      expect(issues.map(&:transition_index)).to all(eq(0))
    end

    it 'reports an undeclared static initial state as an error' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending
          initial_state :missing
        end
      end

      issue = check(klass).errors.first
      expect(issue.code).to eq(:unknown_initial_state)
      expect(issue.state).to eq(:missing)
      expect(issue.event).to be_nil
    end
  end

  describe 'unreachable states' do
    it 'warns about states that are structurally unreachable from the initial state' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :running
          state :orphan

          event :start do
            transitions :from => :pending, :to => :running
          end
        end
      end

      warnings = check(klass).warnings
      expect(warnings.map(&:code)).to eq([:unreachable_state])
      expect(warnings.first.state).to eq(:orphan)
      expect(warnings.first).to be_warning
    end

    it 'treats guarded edges as possible paths to a state' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :maybe

          event :try do
            transitions :from => :pending, :to => :maybe, :guard => :never_true
          end
        end

        def never_true
          false
        end
      end

      expect(check(klass).warnings).to be_empty
    end

    it 'lets transitions without a :from participate from any state' do
      klass = Class.new do
        include AASM
        aasm do
          state :open, :initial => true
          state :closed

          event :close do
            transitions :to => :closed
          end
        end
      end

      expect(check(klass)).to be_empty
    end
  end

  describe 'shadowed candidates' do
    it 'warns when an unconditional candidate is fully shadowed by an earlier one' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :running
          state :finished

          event :advance do
            transitions :from => :pending, :to => :running
            transitions :from => :pending, :to => :finished
          end
        end
      end

      warning = check(klass).warnings.first
      expect(warning.code).to eq(:shadowed_transition)
      expect(warning.event).to eq(:advance)
      expect(warning.transition_index).to eq(1)
      expect(warning.message).to include('transition #2')
      expect(warning.message).to include('transition #1')
    end

    it 'does not flag a guarded preferred transition followed by an unconditional fallback' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :running
          state :finished

          event :advance, :guard => :never_true do
            transitions :from => :pending, :to => :running
            transitions :from => :pending, :to => :finished
          end
        end

        def never_true
          false
        end
      end

      expect(check(klass).warnings).to be_empty
    end

    it 'does not use a guarded earlier transition as a blocker' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :running
          state :finished

          event :advance do
            transitions :from => :pending, :to => :running, :if => :never_true
            transitions :from => :pending, :to => :finished
          end
        end

        def never_true
          false
        end
      end

      expect(check(klass).warnings).to be_empty
    end

    it 'does not flag a candidate still reachable from one of several origins' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :running
          state :finished

          event :advance do
            transitions :from => :pending, :to => :finished
            transitions :from => :running, :to => :finished
            transitions :from => :pending, :to => :running
          end
        end
      end

      expect(check(klass).warnings.map(&:code)).to eq([:shadowed_transition])
      expect(check(klass).warnings.first.transition_index).to eq(2)
    end
  end

  describe 'dynamic initial state' do
    it 'marks unreachable-state analysis as insufficient and does not evaluate the proc' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending
          state :running

          initial_state Proc.new { raise 'dynamic initial state must not run' }

          event :start do
            transitions :from => :pending, :to => :running
          end
        end
      end

      warnings = check(klass).warnings
      expect(warnings.map(&:code)).to eq([:dynamic_initial_state])
      expect(warnings.first.message).to include('dynamic initial state')
      expect(warnings.first.message).to include('could not be performed statically')
    end
  end

  describe 'multiple named machines' do
    let(:multi_class) do
      Class.new do
        include AASM

        aasm(:move) do
          state :standing, :initial => true
          state :walking
          state :idle

          event :walk do
            transitions :from => :standing, :to => :walking
          end
        end

        aasm(:work) do
          state :sleeping, :initial => true

          event :start do
            transitions :from => :sleeping, :to => :processing
          end
        end
      end
    end

    it 'checks all machines and sorts the output by machine name' do
      issues = check(multi_class).issues
      expect(issues.map(&:machine)).to eq([:move, :work])
      expect(issues.map(&:code)).to eq([:unreachable_state, :unknown_to_state])
    end

    it 'can check a single named machine' do
      move_report = check(multi_class, :move)
      expect(move_report.errors).to be_empty
      expect(move_report.warnings.map(&:code)).to eq([:unreachable_state])
      expect(move_report.issues.first.state).to eq(:idle)

      work_report = check(multi_class, :work)
      expect(work_report.errors.map(&:code)).to eq([:unknown_to_state])
      expect(work_report.issues.first.machine).to eq(:work)
    end

    it 'returns an empty report for an unknown machine name' do
      expect(check(multi_class, :nope)).to be_empty
    end

    it 'is also available through the class-level entry point' do
      expect(multi_class.aasm_definition_check.issues.map(&:machine)).to eq([:move, :work])
      expect(multi_class.aasm_definition_check(:work).errors.size).to eq(1)
    end
  end

  describe 'inheritance' do
    let(:parent_class) do
      Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :running

          event :start do
            transitions :from => :pending, :to => :running
          end
        end
      end
    end

    it 'analyses inherited states and events together with subclass additions' do
      child_class = Class.new(parent_class) do
        aasm do
          state :extra
        end
      end

      child_warnings = check(child_class).warnings
      expect(child_warnings.map(&:code)).to eq([:unreachable_state])
      expect(child_warnings.first.state).to eq(:extra)

      expect(check(parent_class)).to be_empty
    end
  end

  describe 'stable ordering' do
    it 'orders findings by event declaration order and transition order' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :running

          event :first do
            transitions :from => :pending, :to => :bad_first
            transitions :from => :pending, :to => :bad_second
          end

          event :second do
            transitions :from => :pending, :to => :bad_third
          end
        end
      end

      issues = check(klass).errors
      expect(issues.map(&:event)).to eq([:first, :first, :second])
      expect(issues.map(&:to)).to eq([:bad_first, :bad_second, :bad_third])
      expect(issues.map(&:transition_index)).to eq([0, 1, 0])
    end
  end

  describe 'read-only behaviour' do
    it 'does not instantiate the checked class, run guards or fire callbacks' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :running

          event :start, :before => :explode do
            transitions :from => :pending, :to => :running,
                         :guard => :explode, :after => :explode
          end
        end

        def self.new(*)
          raise 'the checker must not instantiate business objects'
        end

        def explode
          raise 'no callbacks or guards may run'
        end
      end

      expect { check(klass) }.not_to raise_error
    end

    it 'leaves the original states, events and transitions untouched' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :running

          event :start do
            transitions :from => :pending, :to => :running
          end
        end
      end

      machine = klass.aasm.state_machine
      states_before = machine.states.map(&:name)
      events_before = machine.events.keys
      transitions_before = machine.events[:start].transitions.map { |t| [t.from, t.to] }

      check(klass)
      check(klass, :default)
      klass.aasm_definition_check

      expect(machine.states.map(&:name)).to eq(states_before)
      expect(machine.events.keys).to eq(events_before)
      expect(machine.events[:start].transitions.map { |t| [t.from, t.to] }).to eq(transitions_before)
    end

    it 'does not change real event behaviour' do
      klass = Class.new do
        include AASM
        aasm do
          state :pending, :initial => true
          state :running

          event :start do
            transitions :from => :pending, :to => :running
          end
        end
      end

      check(klass)

      object = klass.new
      expect(object).to be_may_start
      expect(object.start).to eq(true)
      expect(object.aasm.current_state).to eq(:running)
    end
  end
end
