# frozen_string_literal: true

module SolidQueue
  class Job
    module Recurrable
      extend ActiveSupport::Concern

      included do
        # Detaching runs before the association's own `dependent: :destroy`
        # callback, which is registered when `has_one` is declared below.
        before_destroy :detach_recurring_execution, if: :recurring_execution_detachable?

        has_one :recurring_execution, dependent: :destroy
      end

      private
        # Deleting a job would otherwise take its recurring execution with it,
        # and that row is the dedupe evidence for the run. Detach it first so the
        # run stays accounted for, and let the recurring-execution cleanup fold
        # old history instead. Bulk deletes (finished-job cleanup uses
        # +delete_all+) skip callbacks, so they rely on the foreign key being
        # +on_delete: :nullify+ in the folding schema.
        def recurring_execution_detachable?
          RecurringExecution.detachable?
        end

        def detach_recurring_execution
          return unless recurring_execution

          RecurringExecution.where(id: recurring_execution.id).update_all(job_id: nil)
          association(:recurring_execution).reset
        end
    end
  end
end
