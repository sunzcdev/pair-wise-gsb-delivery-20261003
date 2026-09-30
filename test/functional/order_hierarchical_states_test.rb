# frozen_string_literal: true

require 'test_helper'
require 'files/models/order'

class OrderHierarchicalStatesTest < StateMachinesTest
  def setup
    @order = Order.new
  end

  # -- Hierarchy structure ---------------------------------------------------

  def test_states_should_reference_real_parent_states_not_name_prefixes
    machine = Order.state_machine

    validating = machine.states[:validating]
    processing = machine.states[:processing]

    assert_same processing, validating.parent
    assert_equal [processing], validating.ancestors
    assert_includes processing.children, validating
    assert_predicate processing, :root?
    refute_predicate processing, :leaf?
    assert_predicate validating, :leaf?
  end

  def test_should_start_in_initial_state
    assert_sm_state(@order, :pending)
  end

  # -- Event lookup falls back from leaf to parent ---------------------------

  def test_event_defined_on_parent_should_fire_from_child_leaf_state
    @order.start

    assert_sm_can_transition(@order, :finish)
    assert_sm_transition(@order, :finish, :archived)
  end

  def test_parent_event_transition_should_start_from_actual_leaf_state
    @order.start

    transition = @order.finish_transition

    assert_equal :validating, transition.from_name
    assert_equal :archived, transition.to_name
  end

  def test_leaf_branches_should_take_precedence_over_parent_branches
    @order.start
    @order.advance

    # :redo_validation is only defined on the :saving leaf, :finish falls back
    # to the :processing parent -- both resolve from the same current state
    assert_sm_can_transition(@order, :redo_validation)
    assert_sm_can_transition(@order, :finish)
  end

  # -- Entering a hierarchy runs enter callbacks outermost-first -------------

  def test_entering_nested_state_should_run_enter_callbacks_from_parent_down_to_leaf
    @order.start

    assert_equal %i[enter_processing enter_validating], @order.log
    assert_equal 'validating', @order.state
  end

  # -- Cross-sibling transitions ---------------------------------------------

  def test_cross_sibling_transition_should_not_retrigger_common_ancestor_callbacks
    @order.start
    @order.log.clear

    @order.advance

    assert_equal %i[exit_validating enter_saving], @order.log
    assert_equal 'saving', @order.state
  end

  def test_cross_sibling_transition_back_should_not_retrigger_common_ancestor_callbacks
    @order.start
    @order.advance
    @order.log.clear

    @order.redo_validation

    assert_equal %i[exit_saving enter_validating], @order.log
    assert_equal 'validating', @order.state
  end

  def test_cross_sibling_transition_should_compute_exit_and_enter_chains
    @order.start

    transition = @order.advance_transition

    assert_equal %i[validating], transition.exit_states.map(&:name)
    assert_equal %i[saving], transition.enter_states.map(&:name)
  end

  # -- Cross-parent transitions ----------------------------------------------

  def test_cross_parent_transition_should_run_exit_leaf_up_then_enter_parent_down
    @order.start
    @order.advance
    @order.log.clear

    @order.finish

    assert_equal %i[exit_saving exit_processing enter_done enter_archived], @order.log
    assert_equal 'archived', @order.state
  end

  def test_cross_parent_transition_should_compute_exit_and_enter_chains
    @order.start

    transition = @order.finish_transition

    assert_equal %i[validating processing], transition.exit_states.map(&:name)
    assert_equal %i[done archived], transition.enter_states.map(&:name)
  end

  # -- Parent-level interception failure -------------------------------------

  def test_parent_level_guard_failure_should_not_run_exit_or_enter_callbacks
    @order.start
    @order.log.clear
    @order.cancellation_allowed = false

    result = @order.cancel

    refute result
    assert_equal 'validating', @order.state
    assert_equal %i[cancel_failed], @order.log
  end

  def test_parent_level_guard_failure_should_raise_on_bang_event
    @order.start
    @order.cancellation_allowed = false

    assert_raises(StateMachines::InvalidTransition) { @order.cancel! }
    assert_equal 'validating', @order.state
  end

  def test_parent_level_guard_success_should_run_hierarchical_callbacks_in_order
    @order.start
    @order.advance
    @order.log.clear
    @order.cancellation_allowed = true

    @order.cancel

    assert_equal %i[exit_saving exit_processing enter_cancelled], @order.log
    assert_equal 'cancelled', @order.state
  end

  # -- Persistence ------------------------------------------------------------

  def test_persisted_value_should_always_be_a_single_leaf_state_value
    @order.start
    assert_equal 'validating', @order.state

    @order.advance
    assert_equal 'saving', @order.state

    @order.finish
    assert_equal 'archived', @order.state
  end

  def test_current_state_should_match_leaf_state_after_parent_event
    @order.start
    @order.finish

    assert_sm_state(@order, :archived)
    assert_predicate @order, :archived?
  end

  # -- Flat state machines remain unchanged -----------------------------------

  def test_flat_machine_states_should_have_no_hierarchy
    machine = Order.state_machine

    pending = machine.states[:pending]

    assert_nil pending.parent
    assert_predicate pending, :root?
    assert_predicate pending, :leaf?
    assert_empty pending.ancestors
    assert_empty pending.children
  end

  def test_flat_transition_should_exit_from_and_enter_to_state_only
    klass = Class.new do
      attr_accessor :state

      state_machine initial: :off do
        event :flip do
          transition off: :on
        end
      end
    end

    object = klass.new
    transition = object.flip_transition

    assert_equal %i[off], transition.exit_states.map(&:name)
    assert_equal %i[on], transition.enter_states.map(&:name)
    assert object.flip
    assert_equal 'on', object.state
  end

  def test_loopback_should_not_run_exit_or_enter_callbacks
    @order.start
    @order.log.clear

    machine = Order.state_machine
    machine.event :touch do
      transition validating: same
    end

    @order.touch

    assert_empty @order.log
    assert_equal 'validating', @order.state
  end
end
