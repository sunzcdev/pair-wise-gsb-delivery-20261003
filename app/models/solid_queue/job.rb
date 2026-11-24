# frozen_string_literal: true

module SolidQueue
  class Job < Record
    class EnqueueError < StandardError; end

    # Raised when a job's class can't be resolved anymore, typically because it
    # was renamed or removed in a deploy while jobs referencing it were in
    # flight. It subclasses NameError, which is what resolving the class raises.
    class ClassMissingError < NameError
      def self.for(job)
        new("Job class #{job.class_name.inspect} could not be resolved")
      end
    end

    include Executable, Clearable, Recurrable, Batchable, BulkEnqueue

    serialize :arguments, coder: JSON

    class << self
      # The dedup_key column ships in the base schema but reaches existing apps
      # through an optional update migration. Until they run it, bulk enqueues
      # behave exactly as before and every job is inserted as new.
      def dedup_migrated?
        @dedup_migrated ||= column_names.include?("dedup_key")
      end

      # Dedup relies on the database skipping rows that violate the dedup_key
      # unique index during a bulk insert, which only adapters with insert
      # conflict targets support.
      def dedup_enabled?
        dedup_migrated? && supports_insert_conflict_target?
      end

      def enqueue(active_job, scheduled_at: Time.current)
        active_job.scheduled_at = scheduled_at

        create_from_active_job(active_job).tap do |enqueued_job|
          active_job.provider_job_id = enqueued_job.id if enqueued_job.persisted?
          active_job.successfully_enqueued = enqueued_job.persisted?
        end
      end

      private
        DEFAULT_PRIORITY = 0
        DEFAULT_QUEUE_NAME = "default"

        def create_from_active_job(active_job)
          create!(**attributes_from_active_job(active_job))
        rescue ActiveRecord::ActiveRecordError => e
          enqueue_error = EnqueueError.new("#{e.class.name}: #{e.message}").tap do |error|
            error.set_backtrace e.backtrace
          end
          raise enqueue_error
        end

        def attributes_from_active_job(active_job)
          {
            queue_name: active_job.queue_name || DEFAULT_QUEUE_NAME,
            active_job_id: active_job.job_id,
            priority: active_job.priority || DEFAULT_PRIORITY,
            scheduled_at: active_job.scheduled_at,
            class_name: active_job.class.name,
            arguments: active_job.serialize,
            concurrency_key: active_job.concurrency_key
          }.tap do |attributes|
            attributes[:batch_id] = active_job.batch_id if Batch.migrated?
            attributes[:dedup_key] = active_job.solid_queue_dedup_key if dedup_migrated?
          end
        end
    end
  end
end
