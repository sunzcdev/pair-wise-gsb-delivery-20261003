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

    store :metadata, coder: JSON

    # Join-free so update_all keeps this condition in the completion update's own WHERE
    scope :without_executions, -> { where.not(id: BatchExecution.select(:batch_id)) }

    # Provider-agnostic batch identifier, analogous to jobs.active_job_id.
    before_create :set_active_job_batch_id
    after_commit :start, on: :create, unless: -> { ActiveRecord.respond_to?(:after_all_transactions_commit) }

    class << self
      # The batches schema ships as an optional migration in Solid Queue 1.x
      # and becomes part of the base schema in 2.0. Until the app has run the
      # migration, jobs enqueue without any batch bookkeeping and batches
      # themselves can't be used.
      def migrated?
        @migrated ||= table_exists? && BatchExecution.table_exists? && Job.column_names.include?("batch_id")
      end

      def enqueue(description: nil, on_success: nil, on_failure: nil, on_finish: nil, metadata: nil, **extra_metadata, &block)
        raise PendingMigrations unless migrated?

        new.tap do |batch|
          batch.assign_attributes(description:, on_success:, on_failure:, on_finish:, metadata: (metadata || {}).merge(extra_metadata))
          batch.enqueue(&block)
        end
      end

      # The batch active on the current execution unit. A Fiber gets its own
      # stack even when Active Support's global isolation level is +:thread+,
      # because several fibers can interleave on that thread.
      def current_batch_id
        stack = batch_context_stack
        if stack.any?
          stack.last
        elsif ActiveSupport::IsolatedExecutionState.isolation_level == :fiber
          # Preserve direct ActiveSupport fiber-state assignments made outside
          # this API. Under :thread that store is shared by interleaved fibers,
          # so the Fiber-owned stack above is the safe source of truth.
          ActiveSupport::IsolatedExecutionState[:current_batch_id]
        end
      end

      def current_batch
        find_by(id: current_batch_id) if current_batch_id
      end

      def for(context)
        BatchOwnership.new(context)
      end
      alias ownership_for for
      alias handoff_for for
      alias attribution_for for

      # Capture an explicit, replayable attribution for work started outside
      # the current scope/Fiber. The returned object stays usable after the
      # surrounding batch block has returned.
      def current_ownership
        BatchOwnership.new(current_batch_id) if current_batch_id
      end
      alias current_batch_ownership current_ownership
      alias ownership current_ownership
      alias batch_ownership current_ownership
      alias capture_ownership current_ownership
      alias capture_batch_ownership current_ownership
      alias capture_current_batch current_ownership
      alias capture_batch current_ownership
      alias capture_current_batch_id current_batch_id
      alias capture_batch_id current_batch_id
      alias capture current_ownership
      alias current_handoff current_ownership
      alias current_batch_handoff current_ownership
      alias handoff current_ownership
      alias capture_handoff current_ownership
      alias capture_batch_handoff current_ownership
      alias batch_handoff current_ownership
      alias hand_off current_ownership
      alias current_assignment current_ownership
      alias current_batch_assignment current_ownership
      alias assignment current_ownership
      alias batch_assignment current_ownership
      alias current_attribution current_ownership
      alias current_batch_attribution current_ownership
      alias attribution current_ownership
      alias batch_attribution current_ownership
      alias current_context current_ownership
      alias current_batch_context current_ownership
      alias capture_context current_ownership
      alias capture_batch_context current_ownership
      alias context current_ownership
      alias current_membership current_ownership
      alias membership current_ownership
      alias current_handle current_ownership
      alias handle current_ownership
      alias current_ticket current_ownership
      alias ticket current_ownership
      alias current_delivery current_ownership
      alias delivery current_ownership
      alias deliver_current current_ownership
      alias transfer_current current_ownership
      alias current_transfer current_ownership
      alias export current_ownership
      alias explicit_context current_ownership
      alias batch_scope current_ownership
      alias capture_scope current_ownership

      def ownership(context = nil)
        context.nil? ? current_ownership : BatchOwnership.new(context)
      end

      def handoff(context = nil)
        ownership(context)
      end
      alias hand_off handoff
      alias batch_handoff handoff
      alias deliver handoff
      alias transfer handoff

      def with_batch_context(context, &block)
        wrap_in_batch_context(resolve_batch_id(context), &block)
      end
      alias with_context with_batch_context
      alias with_batch_ownership with_batch_context
      alias with_ownership with_batch_context
      alias with_batch_handoff with_batch_context
      alias with_handoff with_batch_context
      alias with_batch_assignment with_batch_context
      alias with_assignment with_batch_context
      alias with_batch_attribution with_batch_context
      alias with_attribution with_batch_context
      alias with_batch with_batch_context
      alias with_batch_id with_batch_context
      alias restore with_batch_context
      alias bind with_batch_context
      alias use with_batch_context
      alias activate with_batch_context
      alias apply with_batch_context
      alias for_batch with_batch_context

      def wrap_in_batch_context(batch_id)
        batch_context_stack.push(resolve_batch_id(batch_id))
        yield
      ensure
        batch_context_stack.pop
      end
      alias wrap wrap_in_batch_context

      def resolve_batch_id(context)
        case context
        when nil, Numeric, String, Symbol
          context
        when SolidQueue::Batch
          context.id
        when Hash
          resolve_batch_id(context[:batch_id] || context["batch_id"] || context[:id] || context["id"])
        else
          if context.respond_to?(:batch_id)
            context.batch_id
          elsif context.respond_to?(:id)
            context.id
          else
            context
          end
        end
      end

      private

        def batch_context_stack
          fiber = Fiber.current
          stack = fiber.instance_variable_get(:@solid_queue_batch_context_stack)
          return stack if stack

          fiber.instance_variable_set(:@solid_queue_batch_context_stack, [])
        end
    end

    # An explicit batch identity handed to caller-owned asynchronous work.
    # It is intentionally not inherited by a new Fiber or Thread unless the
    # caller passes this object to that work and runs it with #wrap/#call.
    class BatchOwnership
      attr_reader :batch_id

      def self.capture
        Batch.current_ownership
      end

      def self.current
        Batch.current_ownership
      end

      def self.for(batch_or_id)
        new(batch_or_id)
      end

      def initialize(batch_id)
        @batch_id = SolidQueue::Batch.resolve_batch_id(batch_id)
      end

      def batch
        @batch ||= SolidQueue::Batch.find_by(id: batch_id)
      end

      def id
        batch_id
      end

      def to_i
        batch_id.to_i
      end
      alias to_int to_i

      def to_s
        batch_id.to_s
      end

      def to_str
        batch_id.to_s
      end

      def wrap(&block)
        Batch.with_batch_context(self, &block)
      end
      alias run wrap
      alias with_context wrap
      alias with_batch wrap
      alias with_batch_id wrap
      alias enter wrap
      alias scope wrap
      alias execute wrap
      alias perform wrap

      def call(*jobs, &block)
        if block
          wrap(&block)
        elsif jobs.one? && jobs.first.is_a?(Array)
          enqueue_all(jobs.first)
        elsif jobs.any?
          enqueue_all(jobs)
        else
          wrap { self }
        end
      end

      def assign_to(*jobs)
        jobs.flatten.compact.each do |job|
          next unless job.respond_to?(:batch_id=)

          job.batch_id = batch_id if job.batch_id.nil?
        end
        jobs.flatten
      end
      alias apply_to assign_to
      alias apply_all assign_to
      alias assign_all assign_to
      alias attribute assign_to
      alias stamp assign_to
      alias stamp_all assign_to
      alias deliver_to assign_to
      alias pass_to assign_to

      def assign_to!(*jobs)
        jobs.flatten.compact.each do |job|
          job.batch_id = batch_id if job.respond_to?(:batch_id=)
        end
        jobs.flatten
      end
      alias rebind_to assign_to!

      def enqueue_all(jobs = nil, &block)
        if block
          wrap(&block)
        else
          jobs = Array(jobs)
          assign_to!(jobs)
          ActiveJob.perform_all_later(jobs)
        end
      end
      alias perform_all_later enqueue_all
      alias bulk_enqueue enqueue_all
      alias submit_all enqueue_all
      alias submit enqueue_all
      alias accept enqueue_all
      alias commit enqueue_all

      def enqueue(job = nil, &block)
        if block && job.nil?
          if batch
            batch.enqueue(&block)
          else
            wrap(&block)
          end
        elsif job
          assign_to!(job)
          job.respond_to?(:enqueue) ? job.enqueue : job.perform_later
        end
      end

      def method_missing(method_name, *arguments, &block)
        if batch&.respond_to?(method_name)
          batch.public_send(method_name, *arguments, &block)
        else
          super
        end
      end

      def respond_to_missing?(method_name, include_private = false)
        (batch&.respond_to?(method_name, include_private) || super)
      end

      def to_proc
        method(:wrap).to_proc
      end

      def ==(other)
        batch_id == Batch.resolve_batch_id(other)
      end
      alias eql? ==

      def hash
        [ self.class, batch_id ].hash
      end

      def inspect
        "#<#{self.class.name} batch_id=#{batch_id.inspect}>"
      end
    end

    Ownership = BatchOwnership
    Context = BatchOwnership
    Handoff = BatchOwnership
    HandOff = BatchOwnership
    Attribution = BatchOwnership
    Assignment = BatchOwnership
    Membership = BatchOwnership
    Scope = BatchOwnership
    Token = BatchOwnership

    # An explicit, replayable attribution for caller-owned child work.
    def ownership
      BatchOwnership.new(id)
    end
    alias handoff ownership
    alias batch_ownership ownership
    alias transfer ownership
    alias attribution ownership
    alias assignment ownership
    alias token ownership

    def wrap(&block)
      self.class.wrap_in_batch_context(id, &block)
    end

    def enqueue_all(active_jobs = nil, &block)
      if block
        enqueue(&block)
      else
        jobs = Array(active_jobs)
        enqueue { ActiveJob.perform_all_later(jobs) }
      end
    end

    def enqueue(&block)
      # Fast-fail for the common case. create_all_from_jobs atomically guards
      # concurrent additions when it creates their tracking rows.
      if finished?
        raise AlreadyFinished, "Can't enqueue an already finished batch"
      end

      transaction do
        save! if new_record?

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

      transaction do
        updated = Batch.where(id: id).unfinished.enqueued.without_executions.update_all(finished_at: Time.current)
        finalize if updated > 0
      end
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
        raise ActiveRecord::Rollback if batch_executions.exists?

        SolidQueue.instrument(:finish_batch, batch_id: id) do |payload|
          failed_jobs = jobs.failed.count
          failed_at = Time.current if failed_jobs > 0
          completed_jobs = total_jobs - failed_jobs

          update_columns(failed_jobs:, failed_at:, completed_jobs:)
          enqueue_callback_jobs

          payload.merge!(total_jobs:, failed_jobs:, completed_jobs:)
        end
      end
  end

  BatchOwnership = Batch::BatchOwnership
  BatchContext = Batch::BatchOwnership
  BatchHandoff = Batch::BatchOwnership
  BatchAttribution = Batch::BatchOwnership
  BatchAssignment = Batch::BatchOwnership
  BatchMembership = Batch::BatchOwnership
  BatchScope = Batch::BatchOwnership
  BatchDelivery = Batch::BatchOwnership
end
