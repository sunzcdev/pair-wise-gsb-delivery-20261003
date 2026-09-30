# frozen_string_literal: true

require 'test_helper'

class CallbackAfterBeingCopiedTest < StateMachinesTest
  def setup
    @callback = StateMachines::Callback.new(:before, parked: :idling, on: :ignite, do: -> {})
    @copied_callback = @callback.dup
  end

  def test_should_copy_methods
    refute_same @callback.send(:instance_variable_get, :@methods),
                @copied_callback.send(:instance_variable_get, :@methods)
  end

  def test_should_copy_branch
    refute_same @callback.branch, @copied_callback.branch
  end
end
