# frozen_string_literal: true

require 'test_helper'

class BranchAfterBeingCopiedTest < StateMachinesTest
  def setup
    @branch = StateMachines::Branch.new(
      parked: :idling,
      on: :ignite,
      if: :ready?,
      if_state: { alarm: :active }
    )
    @copied_branch = @branch.dup
  end

  def test_should_copy_state_requirements
    refute_same @branch.state_requirements, @copied_branch.state_requirements
    refute_same(
      @branch.state_requirements.first[:from],
      @copied_branch.state_requirements.first[:from]
    )
  end

  def test_should_copy_event_requirement
    refute_same @branch.event_requirement, @copied_branch.event_requirement
  end

  def test_should_copy_state_guards
    refute_same @branch.state_guards, @copied_branch.state_guards
    refute_same @branch.state_guards[:if_state], @copied_branch.state_guards[:if_state]
  end
end
