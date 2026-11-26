# frozen_string_literal: true

module SolidQueue
  class Batch
    module Retriable
      extend ActiveSupport::Concern

      class_methods do
        def retry_failed_jobs(batch = nil, *failed_executions, **options)
          raise ArgumentError, "a batch is required" if batch.nil?

          find_batch(batch).retry_failed_jobs(*failed_executions, **options)
        end

        def retry_failed_jobs!(batch = nil, *failed_executions, **options)
          raise ArgumentError, "a batch is required" if batch.nil?

          find_batch(batch).retry_failed_jobs!(*failed_executions, **options)
        end

        %i[retry_failed retry_failures retry_failed_members retry_failed_items retry_failed_executions
           retry_all_failed retry_all_failed_jobs retry_failed_jobs_for retry_failed_for retry! retry].each do |name|
          define_method(name) do |batch = nil, *args, **kwargs|
            retry_failed_jobs(batch, *args, **kwargs)
          end
        end

        %i[retry_failed_jobs_for_batch retry_failures_for_batch retry_failed_members_for_batch].each do |name|
          define_method(name) do |batch, *args, **kwargs|
            retry_failed_jobs(batch, *args, **kwargs)
          end
        end

        private
          def find_batch(batch_or_id)
            batch_or_id.is_a?(Batch) ? batch_or_id : find(batch_or_id)
          end
      end

      def failed_executions
        FailedExecution.where(job_id: jobs.select(:id))
      end

      def failed_members
        jobs.failed
      end
      alias failed_items failed_members

      def retry_failed_jobs(*failed_executions, force: false)
        retry_failed_jobs!(*failed_executions, force: force)
      rescue NothingToRetry
        active_retry_round || latest_retry_round
      end

      def retry_failed_jobs!(*failed_executions, force: false)
        transaction do
          # An atomic row write serializes duplicate clicks across processes and
          # connections. A plain SELECT ... FOR UPDATE does not do this on SQLite.
          self.class.where(id: id).update_all(updated_at: Time.current) if persisted?

          requested = failed_executions.flatten.compact
          unless force
            active_round = active_retry_round
            return active_round if active_round
          end

          executions = resolve_failed_executions(requested)

          if executions.empty? && requested.empty? && (latest = latest_retry_round)&.failed?
            return latest.retry_failed_jobs!(force: force)
          end

          if executions.empty?
            raise Batch::NothingToRetry, "batch ##{id} has no failed jobs to retry"
          end

          round = create_retry_round!(executions.size)
          executions.each { |execution| execution.retry(retry_batch: round) }

          Batch.where(id: round.id).update_all(enqueued_at: Time.current) if round.enqueued_at.nil?
          round.reload
        end.tap do |round|
          # The tracking rows are committed by now. start only marks the round
          # enqueued and performs the normal completion check.
          round&.start
        end
      end

      alias_method :retry_failed_jobs?, :retry_failed_jobs
      alias_method :retry_failed, :retry_failed_jobs
      alias_method :retry_failures, :retry_failed_jobs
      alias_method :retry_failed_members, :retry_failed_jobs
      alias_method :retry_failed_items, :retry_failed_jobs
      alias_method :retry_failed_executions, :retry_failed_jobs
      alias_method :retry_all_failed, :retry_failed_jobs
      alias_method :retry_all_failed_jobs, :retry_failed_jobs
      alias_method :retry_failed_for?, :retry_failed_jobs
      alias_method :create_retry_round, :retry_failed_jobs!
      alias_method :retry_round!, :retry_failed_jobs!
      alias_method :retry!, :retry_failed_jobs!
      alias_method :retry, :retry_failed_jobs

      %i[retry_failed! retry_failures! retry_failed_members! retry_failed_items! retry_failed_executions!].each do |name|
        alias_method name, :retry_failed_jobs!
      end

      private
        def resolve_failed_executions(requested)
          if requested.any?
            requested.map { |value| coerce_failed_execution(value) }.compact.uniq
          else
            FailedExecution.where(job_id: jobs.select(:id)).order(:id).to_a
          end
        end

        def coerce_failed_execution(value)
          case value
          when FailedExecution then value
          when Job then value.failed_execution
          when Integer, String
            FailedExecution.find_by(id: value) ||
              FailedExecution.find_by(job_id: value.to_i)
          end
        end

        def create_retry_round!(failed_count)
          next_round = round + 1
          lineage = {
            "round" => next_round,
            "parent_batch_id" => id,
            "retry_of_batch_id" => id,
            "original_batch_id" => original_batch_id || id,
            "root_batch_id" => original_batch_id || id,
            "retry_key" => "#{original_batch_id || id}:#{next_round}"
          }

          self.class.new(
            description: description,
            total_jobs: failed_count,
            enqueued_at: Time.current,
            round: next_round,
            parent_batch_id: id,
            original_batch_id: original_batch_id || id,
            metadata: metadata.to_h.except(*lineage.keys).merge(lineage)
          ).tap do |round|
            copy_callbacks_to(round)
            round.save!
          end
        end

        def copy_callbacks_to(round)
          %w[on_finish on_success on_failure].each do |callback|
            round.public_send(:"#{callback}=", public_send(callback))
          end
        end
    end
  end
end
