# frozen_string_literal: true

require 'test_helper'

class StateWithParentTest < StateMachinesTest
  def setup
    @machine = StateMachines::Machine.new(Class.new)
    @machine.states << @parent = StateMachines::State.new(@machine, :processing)
    @machine.states << @state = StateMachines::State.new(@machine, :validating, parent: :processing)
  end

  def test_should_have_a_parent_name
    assert_equal :processing, @state.parent_name
  end

  def test_should_resolve_parent_from_machine_states
    assert_same @parent, @state.parent
  end

  def test_should_not_be_root
    refute_predicate @state, :root?
  end

  def test_parent_should_be_root
    assert_predicate @parent, :root?
  end

  def test_should_be_leaf_without_children
    assert_predicate @state, :leaf?
  end

  def test_parent_should_not_be_leaf
    refute_predicate @parent, :leaf?
  end

  def test_should_list_children_on_parent
    assert_equal [@state], @parent.children
  end

  def test_should_have_no_children_on_leaf
    assert_empty @state.children
  end

  def test_should_list_ancestors_from_nearest_parent_up
    @machine.states << grandchild = StateMachines::State.new(@machine, :saving, parent: :validating)

    assert_equal [@state, @parent], grandchild.ancestors
  end

  def test_should_have_no_ancestors_on_root
    assert_empty @parent.ancestors
  end

  def test_should_raise_when_state_is_its_own_parent
    assert_raises(ArgumentError) { @state.parent_name = :validating }
  end

  def test_should_raise_on_cyclic_hierarchy
    @parent.parent_name = :validating

    assert_raises(ArgumentError) { @state.ancestors }
  end

  def test_should_treat_unknown_parent_as_root
    @state.parent_name = :unknown

    assert_nil @state.parent
    assert_predicate @state, :root?
  end

  def test_should_allow_reassigning_parent
    @machine.states << other = StateMachines::State.new(@machine, :done)
    @state.parent_name = :done

    assert_same other, @state.parent
    assert_empty @parent.children
  end

  def test_should_have_no_enter_callbacks_by_default
    assert_empty @state.enter_callbacks
  end

  def test_should_have_no_exit_callbacks_by_default
    assert_empty @state.exit_callbacks
  end

  def test_should_register_enter_callback_block
    callback = ->(object) {}
    @state.on_enter(&callback)

    assert_equal [callback], @state.enter_callbacks
  end

  def test_should_register_exit_callback_method
    @state.on_exit(:reset)

    assert_equal [:reset], @state.exit_callbacks
  end

  def test_should_dup_callbacks_when_copied
    @state.on_enter { nil }
    copy = @state.dup
    copy.on_enter { nil }

    assert_equal 1, @state.enter_callbacks.length
    assert_equal 2, copy.enter_callbacks.length
  end
end
