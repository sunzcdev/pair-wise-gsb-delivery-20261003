# frozen_string_literal: true

require 'test_helper'

class EventAfterBeingCopiedTest < StateMachinesTest
  def setup
    @machine = StateMachines::Machine.new(Class.new)
    @machine.events << @event = StateMachines::Event.new(@machine, :ignite)
    @event.transition(parked: :idling)
    @copied_event = @event.dup
  end

  def test_should_not_have_the_same_collection_of_branches
    refute_same @event.branches, @copied_event.branches
  end

  def test_should_not_have_the_same_collection_of_known_states
    refute_same @event.known_states, @copied_event.known_states
  end

  def test_should_copy_each_branch
    refute_same @event.branches.first, @copied_event.branches.first
  end
end
