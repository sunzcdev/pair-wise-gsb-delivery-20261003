# frozen_string_literal: true

module SolidQueue
  class Batch < Record
    class AlreadyFinished < StandardError; end

    class PendingMigrations < StandardError
      def initialize(message = "The batches schema hasn't been installed yet. Run `bin/rails solid_queue:update` to copy the pending migrations to your application, and then `bin/rails db:migrate` to run them")
        super
      end
    end

    include Callbacks, Status
    include Clearable, Sweepable

    has_many :jobs
    has_many :batch_executions, dependent: :destroy

    # A batch enqueued inside another batch's context (its enqueue block or one
    # of its jobs' perform) becomes a child: the parent won't finish until all
    # its children have, and a failed child fails the parent.
    belongs_to :parent_batch, class_name: "SolidQueue::Batch", optional: true
    has_many :child_batches, class_name: "SolidQueue::Batch", foreign_key: :parent_batch_id, inverse_of: :parent_batch

    store :metadata, coder: JSON

    # Join-free so update_all keeps this condition in the completion update's own WHERE
    scope :without_executions, -> { where.not(id: BatchExecution.select(:batch_id)) }

    # The derived table lets MySQL update the same table the subquery reads.
    # NULL parent ids must be filtered out: NOT IN doesn't match anything when
    # the subquery yields NULLs.
    scope :without_pending_child_batches, -> {
      where.not(id: Batch.unscoped.select(:parent_batch_id).from(
        Batch.unfinished.where.not(parent_batch_id: nil).select(:parent_batch_id), :pending_child_batches
      ))
    }

    # Provider-agnostic batch identifier, analogous to jobs.active_job_id.
    before_create :set_active_job_batch_id
    after_commit :start, on: :create, unless: -> { ActiveRecord.respond_to?(:after_all_transactions_commit) }

    class << self
      # The batches schema ships as an optional migration in Solid Queue 1.x
      # and becomes part of the base schema in 2.0. Until the app has run the
      # migration, jobs enqueue without any batch bookkeeping and batches
      # themselves can't be used.
      def migrated?
        @migrated ||= table_exists? && BatchExecution.table_exists? && Job.column_names.include?("batch_id") && column_names.include?("parent_batch_id")
      end

      def enqueue(description: nil, on_success: nil, on_failure: nil, on_finish: nil, metadata: nil, **extra_metadata, &block)
        raise PendingMigrations unless migrated?

        new.tap do |batch|
          batch.assign_attributes(description:, on_success:, on_failure:, on_finish:, metadata: (metadata || {}).merge(extra_metadata))
          batch.enqueue(&block)
        end
      end

      def current_batch_id
        ActiveSupport::IsolatedExecutionState[:current_batch_id]
      end

      def wrap_in_batch_context(batch_id)
        previous_batch_id = current_batch_id.presence
        ActiveSupport::IsolatedExecutionState[:current_batch_id] = batch_id
        yield
      ensure
        ActiveSupport::IsolatedExecutionState[:current_batch_id] = previous_batch_id
      end

      # The batch whose job is currently performing, if any. Batches enqueued
      # from inside a job become children of that job's batch. Unlike the
      # enqueue block context, this doesn't make plain jobs join the batch.
      def performing_batch_id
        ActiveSupport::IsolatedExecutionState[:performing_batch_id]
      end

      def wrap_in_performing_batch_context(batch_id)
        previous_batch_id = performing_batch_id.presence
        ActiveSupport::IsolatedExecutionState[:performing_batch_id] = batch_id
        yield
      ensure
        ActiveSupport::IsolatedExecutionState[:performing_batch_id] = previous_batch_id
      end
    end

    def enqueue(&block)
      # Fast-fail for the common case. create_all_from_jobs atomically guards
      # concurrent additions when it creates their tracking rows.
      if finished?
        raise AlreadyFinished, "Can't enqueue an already finished batch"
      end

      transaction do
        if new_record?
          # The innermost active batch becomes the parent: an enclosing
          # batch's enqueue block first, then the batch being performed.
          self.parent_batch_id ||= self.class.current_batch_id.presence || self.class.performing_batch_id.presence
          save!
        end

        self.class.wrap_in_batch_context(id) { block&.call(self) }

        if ActiveRecord.respond_to?(:after_all_transactions_commit)
          ActiveRecord.after_all_transactions_commit { start }
        end
      end
    end

    def metadata
      (super || {}).with_indifferent_access
    end

    def start
      mark_as_enqueued

      # Refresh enqueued_at after marking as enqueued, and let a batch that started
      # with no jobs finish right away
      reload
      finish
    end

    def finish
      return if finished? || !enqueued?
      return if batch_executions.exists?
      return if child_batches.unfinished.exists?

      finalized = false
      transaction do
        updated = Batch.where(id: id).unfinished.enqueued.without_executions.without_pending_child_batches.update_all(finished_at: Time.current)
        finalized = finalize if updated > 0
      end

      # Unblock the parent's own completion check outside this transaction, so
      # it reads this batch's finished state as committed. Completion walks up
      # the tree one level per finished batch, never holding a worker thread.
      parent_batch&.finish if finalized
    end

    private
      def set_active_job_batch_id
        self.active_job_batch_id ||= SecureRandom.uuid
      end

      def mark_as_enqueued
        Batch.where(id: id, enqueued_at: nil).update_all(enqueued_at: Time.current)
      end

      def finalize
        reload

        # PostgreSQL can let a blocked CAS win from a stale NOT EXISTS snapshot:
        # after a lock wait, READ COMMITTED re-checks the target row's conditions
        # against the latest data but keeps the original snapshot for subqueries.
        # Re-check in a new statement, which gets a fresh snapshot while this
        # transaction's row lock keeps adders out, since they increment before
        # inserting their executions. MySQL doesn't need this: it reads DML
        # subqueries from the latest committed data, so its CAS can't win wrongly.
        raise ActiveRecord::Rollback if batch_executions.exists? || child_batches.unfinished.exists?

        SolidQueue.instrument(:finish_batch, batch_id: id) do |payload|
          failed_jobs = jobs.failed.count
          # A failed child batch fails its parent, without shifting the
          # parent's own job counters
          failed_at = Time.current if failed_jobs > 0 || child_batches.failed.exists?
          completed_jobs = total_jobs - failed_jobs

          update_columns(failed_jobs:, failed_at:, completed_jobs:)
          enqueue_callback_jobs

          payload.merge!(total_jobs:, failed_jobs:, completed_jobs:)
        end

        true
      end
  end
end
