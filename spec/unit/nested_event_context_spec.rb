require 'spec_helper'

describe 'nested event context' do
  class NestedEventContextObject
    include AASM

    attr_reader :log
    attr_accessor :attempt_invalid_nested_event

    def initialize
      @log = []
    end

    def entry(name, machine = :default)
      context = aasm(machine)
      @log << [
        name,
        context.current_event,
        context.from_state,
        context.to_state,
        context.current_state
      ]
    end

    aasm do
      after_all_events { entry(:global_after_all_events) }
      ensure_on_all_events { entry(:global_ensure) }

      state :pending, initial: true
      state :running,
            before_enter: -> { entry(:running_before_enter) },
            enter: -> { entry(:running_enter) },
            after_enter: -> { entry(:running_after_enter) }
      state :finishing,
            before_enter: -> { entry(:finishing_before_enter) },
            enter: -> { entry(:finishing_enter) },
            after_enter: -> { entry(:finishing_after_enter) }
      state :done,
            before_enter: -> { entry(:done_before_enter) },
            enter: -> { entry(:done_enter) },
            after_enter: -> { entry(:done_after_enter) }
      state :archived,
            before_enter: -> { entry(:archived_before_enter) },
            enter: -> { entry(:archived_enter) },
            after_enter: -> { entry(:archived_after_enter) }

      event :run,
            before: -> { entry(:run_before) },
            after: -> { entry(:run_after) },
            ensure: -> { entry(:run_ensure) } do
        transitions from: :pending, to: :running,
                    after: -> { entry(:run_transition_after) },
                    success: -> { entry(:run_transition_success) }
      end

      event :finish,
            before: -> { entry(:finish_before) },
            after: :finish_after_callback,
            ensure: -> { entry(:finish_ensure) } do
        transitions from: :running, to: :finishing,
                    after: -> { entry(:finish_transition_after) },
                    success: -> { entry(:finish_transition_success) }
      end

      event :finalize,
            before: -> { entry(:finalize_before) },
            after: -> { entry(:finalize_after); archive },
            ensure: -> { entry(:finalize_ensure) } do
        transitions from: :finishing, to: :done,
                    after: -> { entry(:finalize_transition_after) },
                    success: -> { entry(:finalize_transition_success) }
      end

      event :archive,
            before: -> { entry(:archive_before) },
            after: -> { entry(:archive_after) },
            ensure: -> { entry(:archive_ensure) } do
        transitions from: :done, to: :archived,
                    after: -> { entry(:archive_transition_after) },
                    success: -> { entry(:archive_transition_success) }
      end

      event :reject,
            before: -> { entry(:reject_before) },
            ensure: -> { entry(:reject_ensure) } do
        transitions from: :finishing,
                    to: :done,
                    guard: :reject_guard
      end

      event :recover,
            before: -> { entry(:recover_before) },
            after: -> { entry(:recover_after) },
            ensure: -> { entry(:recover_ensure) } do
        transitions from: :running, to: :pending,
                    after: -> { entry(:recover_transition_after) },
                    success: -> { entry(:recover_transition_success) }
      end

      event :break_it,
            before: -> { entry(:break_it_before) },
            error: ->(error) { entry(:break_it_error); raise error },
            ensure: -> { entry(:break_it_ensure) } do
        transitions from: :running,
                    to: :done,
                    after: -> { entry(:break_it_transition_after); raise 'boom' }
      end

      event :attempt_recovery,
            before: -> { entry(:attempt_recovery_before) },
            error: :recover_from_attempt_recovery_error,
            ensure: -> { entry(:attempt_recovery_ensure) } do
        transitions from: :running,
                    to: :done,
                    after: -> { entry(:attempt_recovery_transition_after); raise 'recoverable boom' }
      end
    end

    def finish_after_callback
      entry(:finish_after)

      if attempt_invalid_nested_event
        begin
          reject
        rescue AASM::InvalidTransition
          entry(:nested_guard_rejected)
        end
      else
        finalize
      end
    end

    def reject_guard
      entry(:reject_guard)
      false
    end

    def recover_from_attempt_recovery_error(_error)
      entry(:attempt_recovery_error)
      recover
    end
  end

  def entry(name, event, from_state, to_state, current_state)
    [name, event, from_state, to_state, current_state]
  end

  it 'restores the outer context while keeping nested state changes' do
    object = NestedEventContextObject.new

    object.run
    object.finish!

    expected_log = [
      entry(:run_before, :run, :pending, nil, :pending),
      entry(:run_transition_after, :run, :pending, :running, :pending),
      entry(:running_before_enter, :run, :pending, :running, :pending),
      entry(:running_enter, :run, :pending, :running, :pending),
      entry(:running_after_enter, :run, :pending, :running, :running),
      entry(:run_after, :run, :pending, :running, :running),
      entry(:global_after_all_events, :run, :pending, :running, :running),
      entry(:run_ensure, :run, :pending, :running, :running),
      entry(:global_ensure, :run, :pending, :running, :running),
      entry(:finish_before, :"finish!", :running, nil, :running),
      entry(:finish_transition_after, :"finish!", :running, :finishing, :running),
      entry(:finishing_before_enter, :"finish!", :running, :finishing, :running),
      entry(:finishing_enter, :"finish!", :running, :finishing, :running),
      entry(:finish_transition_success, :"finish!", :running, :finishing, :finishing),
      entry(:finishing_after_enter, :"finish!", :running, :finishing, :finishing),
      entry(:finish_after, :"finish!", :running, :finishing, :finishing),
      entry(:finalize_before, :finalize, :finishing, nil, :finishing),
      entry(:finalize_transition_after, :finalize, :finishing, :done, :finishing),
      entry(:done_before_enter, :finalize, :finishing, :done, :finishing),
      entry(:done_enter, :finalize, :finishing, :done, :finishing),
      entry(:done_after_enter, :finalize, :finishing, :done, :done),
      entry(:finalize_after, :finalize, :finishing, :done, :done),
      entry(:archive_before, :archive, :done, nil, :done),
      entry(:archive_transition_after, :archive, :done, :archived, :done),
      entry(:archived_before_enter, :archive, :done, :archived, :done),
      entry(:archived_enter, :archive, :done, :archived, :done),
      entry(:archived_after_enter, :archive, :done, :archived, :archived),
      entry(:archive_after, :archive, :done, :archived, :archived),
      entry(:global_after_all_events, :archive, :done, :archived, :archived),
      entry(:archive_ensure, :archive, :done, :archived, :archived),
      entry(:global_ensure, :archive, :done, :archived, :archived),
      entry(:global_after_all_events, :finalize, :finishing, :done, :archived),
      entry(:finalize_ensure, :finalize, :finishing, :done, :archived),
      entry(:global_ensure, :finalize, :finishing, :done, :archived),
      entry(:global_after_all_events, :"finish!", :running, :finishing, :archived),
      entry(:finish_ensure, :"finish!", :running, :finishing, :archived),
      entry(:global_ensure, :"finish!", :running, :finishing, :archived)
    ]

    expect(object.log).to eq(expected_log)

    expect(object.aasm.current_state).to eq(:archived)
    expect(object.aasm.current_event).to eq(:"finish!")
    expect(object.aasm.from_state).to eq(:running)
    expect(object.aasm.to_state).to eq(:finishing)
  end

  it 'restores the outer context after a nested guard rejection' do
    object = NestedEventContextObject.new

    object.run
    object.attempt_invalid_nested_event = true

    object.finish

    expect(object.log).to include(
      entry(:nested_guard_rejected, :finish, :running, :finishing, :finishing),
      entry(:reject_before, :reject, :finishing, nil, :finishing),
      entry(:reject_guard, :reject, :finishing, :done, :finishing),
      entry(:reject_ensure, :reject, :finishing, nil, :finishing),
      entry(:global_ensure, :reject, :finishing, nil, :finishing),
      entry(:finish_ensure, :finish, :running, :finishing, :finishing),
      entry(:global_ensure, :finish, :running, :finishing, :finishing)
    )

    expect(object.aasm.current_state).to eq(:finishing)
    expect(object.aasm.current_event).to eq(:finish)
    expect(object.aasm.from_state).to eq(:running)
    expect(object.aasm.to_state).to eq(:finishing)
  end

  class EnsureNestedEventContextObject
    include AASM

    attr_reader :log

    def initialize
      @log = []
    end

    def entry(name, machine = :default)
      context = aasm(machine)
      @log << [
        name,
        context.current_event,
        context.from_state,
        context.to_state,
        context.current_state
      ]
    end

    aasm do
      state :pending, initial: true
      state :active
      state :completed
      state :reset

      event :run,
            after: -> { entry(:run_after); complete },
            ensure: -> { entry(:run_ensure) } do
        transitions from: :pending, to: :active
      end

      event :complete,
            after: -> { entry(:complete_after) },
            ensure: :ensure_complete do
        transitions from: :active, to: :completed
      end

      event :reset,
            after: -> { entry(:reset_after) },
            ensure: -> { entry(:reset_ensure) } do
        transitions from: :completed, to: :reset
      end
    end

    def ensure_complete
      entry(:complete_ensure)
    end
  end

  it 'restores the outer context after an event triggered from ensure' do
    object = EnsureNestedEventContextObject.new

    def object.ensure_complete
      entry(:complete_ensure)
      reset
    end

    object.run

    expect(object.log).to include(
      entry(:complete_ensure, :complete, :active, :completed, :completed),
      entry(:reset_after, :reset, :completed, :reset, :reset),
      entry(:reset_ensure, :reset, :completed, :reset, :reset),
      entry(:run_ensure, :run, :pending, :active, :reset)
    )

    expect(object.log.last(3)).to eq(
      [
        entry(:reset_after, :reset, :completed, :reset, :reset),
        entry(:reset_ensure, :reset, :completed, :reset, :reset),
        entry(:run_ensure, :run, :pending, :active, :reset)
      ]
    )

    expect(object.aasm.current_state).to eq(:reset)
    expect(object.aasm.current_event).to eq(:run)
    expect(object.aasm.from_state).to eq(:pending)
    expect(object.aasm.to_state).to eq(:active)
  end

  it 'restores the outer context after an error callback triggers an event' do
    object = NestedEventContextObject.new

    object.aasm.current_state = :running

    object.attempt_recovery

    expect(object.log).to include(
      entry(:attempt_recovery_transition_after, :attempt_recovery, :running, :done, :running),
      entry(:attempt_recovery_error, :attempt_recovery, :running, :done, :running),
      entry(:recover_before, :recover, :running, nil, :running),
      entry(:recover_ensure, :recover, :running, :pending, :pending),
      entry(:attempt_recovery_ensure, :attempt_recovery, :running, :done, :pending),
      entry(:global_ensure, :attempt_recovery, :running, :done, :pending)
    )

    expect(object.aasm.current_state).to eq(:pending)
    expect(object.aasm.current_event).to eq(:attempt_recovery)
    expect(object.aasm.from_state).to eq(:running)
    expect(object.aasm.to_state).to eq(:done)
  end

  it 'keeps exception propagation after nested context restoration' do
    object = NestedEventContextObject.new

    object.run

    expect { object.break_it }.to raise_error(RuntimeError, 'boom')

    expect(object.log).to include(
      entry(:break_it_transition_after, :break_it, :running, :done, :running),
      entry(:break_it_error, :break_it, :running, :done, :running),
      entry(:break_it_ensure, :break_it, :running, :done, :running)
    )

    expect(object.aasm.current_state).to eq(:running)
    expect(object.aasm.current_event).to eq(:break_it)
    expect(object.aasm.from_state).to eq(:running)
    expect(object.aasm.to_state).to eq(:done)
  end

  class NamedNestedEventContextObject
    include AASM

    attr_reader :log

    def initialize
      @log = []
    end

    def entry(name, machine)
      context = aasm(machine)
      @log << [
        name,
        machine,
        context.current_event,
        context.from_state,
        context.to_state,
        context.current_state
      ]
    end

    aasm :first do
      state :pending, initial: true
      state :active

      event :first_run,
            after: -> { entry(:first_after, :first); second_run },
            ensure: -> { entry(:first_ensure, :first) } do
        transitions from: :pending, to: :active
      end
    end

    aasm :second do
      state :idle, initial: true
      state :busy

      event :second_run,
            after: -> { entry(:second_after, :second) },
            ensure: -> { entry(:second_ensure, :second) } do
        transitions from: :idle, to: :busy
      end
    end
  end

  it 'keeps temporary event fields separate for named state machines' do
    object = NamedNestedEventContextObject.new

    object.first_run

    expect(object.log).to eq(
      [
        [:first_after, :first, :first_run, :pending, :active, :active],
        [:second_after, :second, :second_run, :idle, :busy, :busy],
        [:second_ensure, :second, :second_run, :idle, :busy, :busy],
        [:first_ensure, :first, :first_run, :pending, :active, :active]
      ]
    )

    expect(object.aasm(:first).current_state).to eq(:active)
    expect(object.aasm(:first).current_event).to eq(:first_run)
    expect(object.aasm(:first).from_state).to eq(:pending)
    expect(object.aasm(:first).to_state).to eq(:active)

    expect(object.aasm(:second).current_state).to eq(:busy)
    expect(object.aasm(:second).current_event).to eq(:second_run)
    expect(object.aasm(:second).from_state).to eq(:idle)
    expect(object.aasm(:second).to_state).to eq(:busy)
  end
end
