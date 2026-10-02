require 'spec_helper'

describe 'nested event context' do
  class NestedEventContextRecord
    class CallbackError < StandardError; end

    include AASM

    attr_reader :log

    aasm do
      state :a, initial: true
      state :b, after_enter: :move_b_from_b
      state :c, after_enter: :move_c_from_c
      state :d
      state :e
      state :f
      state :g
      state :h
      state :i

      event :move_a, after: :log_event do
        transitions from: :a, to: :b, after: :log_transition
      end

      event :move_b, after: :log_event do
        transitions from: :b, to: :c, after: :log_transition
      end

      event :move_c, after: :log_event do
        transitions from: :c, to: :d, after: :log_transition
      end

      event :wrap_rejection, after: [:after_wrapped_rejection, :log_event] do
        transitions from: :d, to: :e, after: :log_transition
      end

      event :reject_reset, ensure: :log_ensure do
        transitions from: :e, to: :d, guard: :reject_guard
      end

      event :wrap_failure, after: [:start_failing_event, :log_event] do
        transitions from: :d, to: :f, after: :log_transition
      end

      event :fail, error: :handle_failure, ensure: :ensure_failure do
        transitions from: :f, to: :a, after: :fail_transition_callback
      end

      event :recover, after: :log_event do
        transitions from: :f, to: :g, after: :log_transition
      end

      event :move_after_error, after: :log_event do
        transitions from: :g, to: :h, after: :log_transition
      end

      event :move_after_ensure, after: :log_event do
        transitions from: :h, to: :i, after: :log_transition
      end
    end

    def initialize
      @log = []
    end

    def context(name)
      log << [
        name,
        aasm.current_event,
        aasm.from_state,
        aasm.to_state,
        aasm.current_state
      ]
    end

    def log_transition
      context(:transition)
    end

    def log_event
      context(:event)
    end

    def log_ensure
      context(:ensure)
    end

    def move_b_from_b
      context(:before_level_2)
      move_b
      context(:after_level_2)
    end

    def move_c_from_c
      context(:before_level_3)
      move_c
      context(:after_level_3)
    end

    def reject_guard
      context(:guard)
      false
    end

    def after_wrapped_rejection
      begin
        reject_reset
      rescue AASM::InvalidTransition
        context(:rejected)
      end
      context(:resumed_after_rejection)
    end

    def start_failing_event
      context(:before_failure)
      fail
      context(:resumed_after_failure)
    end

    def handle_failure(error)
      context(:error)
      log << [:error_class, error.class]
      move_after_error
      context(:after_error_event)
    end

    def ensure_failure
      context(:before_ensure_event)
      move_after_ensure
      context(:after_ensure_event)
    end

    def fail_transition_callback
      recover
      raise CallbackError, 'callback failed'
    end
  end

  class NamedMachineContextRecord
    include AASM

    attr_reader :log

    aasm(:left) do
      state :left_one, initial: true
      state :left_two, after_enter: :start_right_event
      state :left_three

      event :move_left_outer, after: :log_left_event do
        transitions from: :left_one, to: :left_two, after: :log_left_transition
      end

      event :move_left_inner, after: :log_left_event do
        transitions from: :left_two, to: :left_three, after: :log_left_transition
      end
    end

    aasm(:right) do
      state :right_one, initial: true
      state :right_two, after_enter: :start_left_inner

      event :move_right_outer, after: :log_right_event do
        transitions from: :right_one, to: :right_two, after: :log_right_transition
      end
    end

    def initialize
      @log = []
    end

    def context(machine, name)
      log << [
        name,
        aasm(machine).current_event,
        aasm(machine).from_state,
        aasm(machine).to_state,
        aasm(machine).current_state
      ]
    end

    def log_left_transition
      context(:left, :left_transition)
    end

    def log_left_event
      context(:left, :left_event)
    end

    def log_right_event
      context(:right, :right_event)
    end

    def log_right_transition
      context(:right, :right_transition)
    end

    def start_right_event
      context(:left, :before_right)
      move_right_outer
      context(:left, :after_right)
    end

    def start_left_inner
      context(:right, :before_left_inner)
      move_left_inner
      context(:right, :after_left_inner)
    end

  end

  it 'restores each event context without rolling back nested state changes' do
    record = NestedEventContextRecord.new

    record.move_a

    expected = [
      [:transition, :move_a, :a, :b, :a],
      [:before_level_2, :move_a, :a, :b, :b],
      [:transition, :move_b, :b, :c, :b],
      [:before_level_3, :move_b, :b, :c, :c],
      [:transition, :move_c, :c, :d, :c],
      [:event, :move_c, :c, :d, :d],
      [:after_level_3, :move_b, :b, :c, :d],
      [:event, :move_b, :b, :c, :d],
      [:after_level_2, :move_a, :a, :b, :d],
      [:event, :move_a, :a, :b, :d]
    ]

    expect(record.log).to eq(expected)
    expect(record.aasm.current_state).to eq(:d)
    expect(record.aasm.current_event).to eq(:move_a)
    expect(record.aasm.from_state).to eq(:a)
    expect(record.aasm.to_state).to eq(:b)
  end

  it 'restores the outer context after a nested guard rejection' do
    record = NestedEventContextRecord.new
    record.aasm.current_state = :d
    record.log.clear

    record.wrap_rejection

    expected = [
      [:transition, :wrap_rejection, :d, :e, :d],
      [:guard, :reject_reset, :e, :d, :e],
      [:ensure, :reject_reset, :e, nil, :e],
      [:rejected, :wrap_rejection, :d, :e, :e],
      [:resumed_after_rejection, :wrap_rejection, :d, :e, :e],
      [:event, :wrap_rejection, :d, :e, :e]
    ]

    expect(record.log).to eq(expected)
    expect(record.aasm.current_state).to eq(:e)
    expect(record.aasm.current_event).to eq(:wrap_rejection)
    expect(record.aasm.from_state).to eq(:d)
    expect(record.aasm.to_state).to eq(:e)
  end

  it 'keeps state changes made before a callback exception and restores error and ensure contexts' do
    record = NestedEventContextRecord.new
    record.aasm.current_state = :d
    record.log.clear

    record.wrap_failure

    expected = [
      [:transition, :wrap_failure, :d, :f, :d],
      [:before_failure, :wrap_failure, :d, :f, :f],
      [:transition, :recover, :f, :g, :f],
      [:event, :recover, :f, :g, :g],
      [:error, :fail, :f, :a, :g],
      [:error_class, NestedEventContextRecord::CallbackError],
      [:transition, :move_after_error, :g, :h, :g],
      [:event, :move_after_error, :g, :h, :h],
      [:after_error_event, :fail, :f, :a, :h],
      [:before_ensure_event, :fail, :f, :a, :h],
      [:transition, :move_after_ensure, :h, :i, :h],
      [:event, :move_after_ensure, :h, :i, :i],
      [:after_ensure_event, :fail, :f, :a, :i],
      [:resumed_after_failure, :wrap_failure, :d, :f, :i],
      [:event, :wrap_failure, :d, :f, :i]
    ]

    expect(record.log).to eq(expected)
    expect(record.aasm.current_state).to eq(:i)
    expect(record.aasm.current_event).to eq(:wrap_failure)
    expect(record.aasm.from_state).to eq(:d)
    expect(record.aasm.to_state).to eq(:f)
  end

  it 'keeps independent temporary context for named state machines' do
    record = NamedMachineContextRecord.new

    record.move_left_outer

    expected = [
      [:left_transition, :move_left_outer, :left_one, :left_two, :left_one],
      [:before_right, :move_left_outer, :left_one, :left_two, :left_two],
      [:right_transition, :move_right_outer, :right_one, :right_two, :right_one],
      [:before_left_inner, :move_right_outer, :right_one, :right_two, :right_two],
      [:left_transition, :move_left_inner, :left_two, :left_three, :left_two],
      [:left_event, :move_left_inner, :left_two, :left_three, :left_three],
      [:after_left_inner, :move_right_outer, :right_one, :right_two, :right_two],
      [:right_event, :move_right_outer, :right_one, :right_two, :right_two],
      [:after_right, :move_left_outer, :left_one, :left_two, :left_three],
      [:left_event, :move_left_outer, :left_one, :left_two, :left_three]
    ]

    expect(record.log).to eq(expected)
    expect(record.aasm(:left).current_state).to eq(:left_three)
    expect(record.aasm(:left).current_event).to eq(:move_left_outer)
    expect(record.aasm(:left).from_state).to eq(:left_one)
    expect(record.aasm(:left).to_state).to eq(:left_two)
    expect(record.aasm(:right).current_state).to eq(:right_two)
    expect(record.aasm(:right).current_event).to eq(:move_right_outer)
    expect(record.aasm(:right).from_state).to eq(:right_one)
    expect(record.aasm(:right).to_state).to eq(:right_two)
  end
end
