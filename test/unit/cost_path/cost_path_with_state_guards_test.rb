# frozen_string_literal: true

require 'test_helper'

# State guards (if_state / unless_state) only read the current state of
# other machines, so they are evaluated safely during the search and used
# to prune impossible transitions.
class CostPathWithStateGuardsTest < StateMachinesTest
  def setup
    @klass = Class.new do
      attr_accessor :state, :alarm

      def initialize
        @state = 'parked'
        @alarm = 'off'
        super
      end

      state_machine :state, initial: :parked do
        event :ignite do
          transition parked: :idling, if_state: { alarm: :off }
        end
        event :tow do
          transition parked: :scrapyard, if_state: { alarm: :on }
        end
      end

      state_machine :alarm, initial: :off do
        event :sound do
          transition off: :on
        end
      end
    end

    @machine = @klass.state_machine(:state)
    @object = @klass.new
  end

  def test_should_keep_transition_when_state_guard_passes
    path = StateMachines::CostPath.new(@object, @machine, to: :idling)

    assert path.reachable?
    assert_equal [:ignite], path.events
  end

  def test_should_report_state_guard_as_verified
    path = StateMachines::CostPath.new(@object, @machine, to: :idling)

    assert_equal [
      { event: :ignite, guard: :if_state, condition: { alarm: :off }, status: :verified }
    ], path.guards
    assert_empty path.pending_guards
  end

  def test_should_prune_transition_when_state_guard_fails
    path = StateMachines::CostPath.new(@object, @machine, to: :scrapyard)

    assert path.unreachable?
  end

  def test_should_reevaluate_pruning_when_other_machine_state_changes
    @object.alarm = 'on'

    assert StateMachines::CostPath.new(@object, @machine, to: :scrapyard).reachable?
    assert StateMachines::CostPath.new(@object, @machine, to: :idling).unreachable?
  end
end
