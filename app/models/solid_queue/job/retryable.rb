# frozen_string_literal: true

module SolidQueue
  class Job
    module Retryable
      extend ActiveSupport::Concern

      included do
        has_one :failed_execution

        scope :failed, -> { includes(:failed_execution).where.not(failed_execution: { id: nil }) }
      end

      def retry(retry_batch: nil, round: nil)
        target_batch = retry_batch || round
        return failed_execution&.retry if target_batch.nil?

        failed_execution&.retry(retry_batch: target_batch)
      end

      def attach_to_retry_round(retry_batch)
        retry_batch = SolidQueue::Batch.find(retry_batch.to_i) unless retry_batch.is_a?(SolidQueue::Batch)

        self.batch_id = retry_batch.id
        if arguments.is_a?(Hash)
          arguments["batch_id"] = retry_batch.id
          self.arguments = arguments
        end
        save!

        BatchExecution.create_or_find_by!(batch_id: retry_batch.id, job_id: id)
        self
      end
      alias move_to_retry_round attach_to_retry_round
      alias rebind_batch_to_retry_round attach_to_retry_round

      def failed_with(exception)
        FailedExecution.transaction(requires_new: true) do
          FailedExecution.create!(job_id: id, exception: exception)
        end
      rescue ActiveRecord::RecordNotUnique
        if (failed_execution = FailedExecution.find_by(job_id: id))
          failed_execution.exception = exception
          failed_execution.save!
        else
          retry
        end
      end

      def reset_execution_counters
        arguments["executions"] = 0
        arguments["exception_executions"] = {}
        save!
      end
    end
  end
end
