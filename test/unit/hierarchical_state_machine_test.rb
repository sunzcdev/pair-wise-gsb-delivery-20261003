# frozen_string_literal: true

require 'test_helper'

class HierarchicalStateMachineTest < StateMachinesTest
  def test_transition_between_sibling_states_only_exits_and_enters_specific_states
    klass = build_machine do |machine, log|
      machine.state :alpha, parent: :processing do
        on_exit { log << 'exit alpha' }
      end
      machine.state :beta, parent: :processing do
        on_enter { log << 'enter beta' }
      end
      machine.state :processing do
        on_enter { log << 'enter processing' }
        on_exit { log << 'exit processing' }

        event :move do
          transition alpha: :beta
        end
      end
    end

    object = klass.new
    object.state = 'alpha'

    assert object.move
    assert_equal 'beta', object.state
    assert_equal ['exit alpha', 'enter beta'], object.log
  end

  def test_transition_between_parents_skips_shared_ancestor
    klass = build_machine do |machine, log|
      machine.state :alpha, parent: :first do
        on_exit { log << 'exit alpha' }
      end
      machine.state :first, parent: :processing do
        on_exit { log << 'exit first' }
      end
      machine.state :processing do
        on_enter { log << 'enter processing' }
        on_exit { log << 'exit processing' }
      end
      machine.state :beta, parent: :second do
        on_enter { log << 'enter beta' }
      end
      machine.state :second, parent: :processing do
        on_enter { log << 'enter second' }
      end

      machine.event :move do
        transition alpha: :beta
      end
    end

    object = klass.new
    object.state = 'alpha'

    assert object.move
    assert_equal 'beta', object.state
    assert_equal ['exit alpha', 'exit first', 'enter second', 'enter beta'], object.log
  end

  def test_state_actions_run_around_transition_callbacks_and_action
    klass = build_machine do |machine, log|
      machine.state :alpha, parent: :processing do
        on_exit { log << 'exit alpha' }
      end
      machine.state :beta, parent: :processing do
        on_enter { log << 'enter beta' }
      end
      machine.state :processing

      machine.before_transition { log << 'before' }
      machine.after_transition { log << 'after' }

      machine.event :move do
        transition alpha: :beta
      end
    end

    klass.send(:define_method, :save) do
      log << 'action'
      true
    end

    machine = klass.state_machine
    machine.instance_variable_set(:@action, :save)
    object = klass.new
    object.state = 'alpha'

    assert object.move
    assert_equal ['before', 'exit alpha', 'action', 'enter beta', 'after'], object.log
  end

  def test_entry_actions_do_not_run_when_action_fails
    klass = build_machine do |machine, log|
      machine.state :alpha, parent: :processing do
        on_exit { log << 'exit alpha' }
      end
      machine.state :beta, parent: :processing do
        on_enter { log << 'enter beta' }
      end
      machine.state :processing

      machine.after_transition { log << 'after' }
      machine.after_failure { log << 'failure' }

      machine.event :move do
        transition alpha: :beta
      end
    end

    klass.send(:define_method, :save) do
      log << 'action'
      false
    end

    machine = klass.state_machine
    machine.instance_variable_set(:@action, :save)
    object = klass.new
    object.state = 'alpha'

    refute object.move
    assert_equal 'alpha', object.state
    assert_equal ['exit alpha', 'action', 'failure'], object.log
  end

  def test_parent_handler_that_cannot_guard_does_not_fire_exit_or_enter_actions
    klass = build_machine do |machine, log|
      machine.state :alpha, parent: :processing do
        on_exit { log << 'exit alpha' }
      end
      machine.state :beta, parent: :processing do
        on_enter { log << 'enter beta' }
      end
      machine.state :processing do
        on_exit { log << 'exit processing' }

        event :move do
          transition alpha: :beta, if: :allow_move?
        end
      end
      machine.after_failure(on: :move) { log << 'failure' }
    end

    object = klass.new
    object.state = 'alpha'

    refute object.move
    assert_equal 'alpha', object.state
    assert_equal ['failure'], object.log
  end

  def test_leaf_event_is_used_before_parent_event_without_relying_on_names
    klass = build_machine do |machine, log|
      machine.state :idle, parent: :processing
      machine.state :waiting, parent: :processing
      machine.state :done

      machine.state :processing do
        event :finish do
          transition idle: :waiting
        end
      end
      machine.event :finish do
        transition idle: :done
      end
    end

    object = klass.new
    object.state = 'idle'

    assert object.finish
    assert_equal 'waiting', object.state
  end

  def test_parent_failure_falls_back_to_machine_root_handler
    klass = build_machine do |machine, log|
      machine.state :idle, parent: :processing
      machine.state :waiting, parent: :processing
      machine.state :done

      machine.state :processing do
        event :finish do
          transition idle: :waiting, if: :allow_move?
        end
      end
      machine.event :finish do
        transition idle: :done
      end
    end

    object = klass.new
    object.state = 'idle'

    assert object.finish
    assert_equal 'done', object.state
  end

  def test_transition_callbacks_match_parent_state_declarations
    calls = []
    klass = build_machine do |machine, _log|
      machine.state :idle, parent: :processing
      machine.state :done

      machine.after_transition to: :processing do |_object, transition|
        calls << transition.to_name
      end
      machine.after_transition from: :processing do |_object, transition|
        calls << transition.from_name
      end

      machine.event :finish do
        transition idle: :done
      end
    end

    object = klass.new
    object.state = 'idle'

    assert object.finish
    assert_equal [:idle], calls
  end

  def test_name_prefixes_do_not_create_hierarchy
    klass = build_machine do |machine, log|
      machine.state :processing, value: 'processing' do
        on_exit { log << 'exit processing' }
      end
      machine.state :processing_alpha, parent: nil do
        on_enter { log << 'enter alpha' }
      end
      machine.state :processing_beta, parent: nil

      machine.state :processing do
        event :move do
          transition processing_alpha: :processing_beta
        end
      end

      machine.event :move do
        transition processing_alpha: :processing_beta
      end
    end

    object = klass.new
    object.state = 'processing_alpha'

    assert object.move
    assert_equal 'processing_beta', object.state
    assert_empty object.log
  end

  def test_parent_state_behavior_is_shared_by_leaf_states
    klass = build_machine do |machine, _log|
      machine.state :idle, parent: :processing
      machine.state :waiting, parent: :processing

      machine.state :processing do
        def status
          'busy'
        end
      end
    end

    object = klass.new
    object.state = 'waiting'

    assert_equal 'busy', object.status
  end

  private

  def build_machine
    log = []

    Class.new do
      attr_accessor :state
      attr_reader :log

      define_method(:initialize) do
        @log = log
      end

      define_method(:allow_move?) { false }

      define_singleton_method(:machine_definition_log) { log }
    end.tap do |klass|
      machine = StateMachines::Machine.new(klass, initial: nil, initialize: false)
      yield machine, log
    end
  end
end
