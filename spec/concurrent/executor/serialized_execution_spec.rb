require 'concurrent/executor/serialized_execution_delegator'
require 'concurrent/executor/immediate_executor'
require_relative 'executor_service_shared'

module Concurrent

  RSpec.describe SerializedExecution do

    it 'raises an exception when no block is given' do
      expect {
        subject.post(ImmediateExecutor.new)
      }.to raise_error(ArgumentError, 'no block given')
    end
  end

  RSpec.describe SerializedExecutionDelegator do

    subject { SerializedExecutionDelegator.new(ImmediateExecutor.new) }

    it_should_behave_like :executor_service, immediate_type: true
  end
end
