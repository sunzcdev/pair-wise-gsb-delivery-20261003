# frozen_string_literal: true

module SolidQueue
  class Batch
    module Status
      extend ActiveSupport::Concern

      included do
        scope :finished, -> { where.not(finished_at: nil) }
        scope :succeeded, -> { finished.where(failed_at: nil) }
        scope :unfinished, -> { where(finished_at: nil) }
        scope :failed, -> { where.not(failed_at: nil) }
        scope :enqueued, -> { where.not(enqueued_at: nil) }
      end

      def status
        if finished?
          failed? ? :failed : :completed
        elsif enqueued?
          :enqueued
        else
          :pending
        end
      end

      # Retry rounds are represented as separate batches so the batch that
      # originally fired its callback keeps its terminal counters and state.
      def round
        lineage_value("round") || 1
      end
      alias round_number round
      alias retry_round round
      alias generation round
      alias epoch round
      alias iteration round

      def round=(value)
        write_lineage_value("round", value.to_i)
      end

      def retry_round=(value)
        self.round = value
      end

      def retry_round?
        round > 1 || parent_batch_id.present?
      end
      alias retry? retry_round?
      alias retried? retry_round?

      def retry_count
        [ round.to_i - 1, 0 ].max
      end

      def parent_batch_id
        lineage_value("parent_batch_id") || lineage_value("retry_of_batch_id")
      end
      alias retry_of_batch_id parent_batch_id
      alias source_batch_id parent_batch_id
      alias retry_parent_id parent_batch_id

      def parent_batch_id=(value)
        write_lineage_value("parent_batch_id", value)
        write_lineage_value("retry_of_batch_id", value) if lineage_column?("retry_of_batch_id")
      end
      alias_method :retry_of_batch_id=, :parent_batch_id=
      alias_method :source_batch_id=, :parent_batch_id=

      def original_batch_id
        lineage_value("original_batch_id") || lineage_value("root_batch_id") || parent_batch_id
      end
      alias root_batch_id original_batch_id

      def original_batch_id=(value)
        write_lineage_value("original_batch_id", value)
        write_lineage_value("root_batch_id", value) if lineage_column?("root_batch_id")
      end
      alias_method :root_batch_id=, :original_batch_id=

      def parent_batch
        Batch.find_by(id: parent_batch_id) if parent_batch_id
      end
      alias retry_of_batch parent_batch
      alias source_batch parent_batch
      alias retry_parent parent_batch
      alias parent parent_batch

      def original_batch
        Batch.find_by(id: original_batch_id) if original_batch_id
      end
      alias root_batch original_batch
      alias root original_batch

      def retry_rounds
        if lineage_column?("parent_batch_id")
          scope = Batch.where(parent_batch_id: id)
          lineage_column?("round") ? scope.order(:round) : scope.to_a.sort_by(&:round)
        else
          Batch.retry_rounds_for(self)
        end
      end
      alias rounds retry_rounds
      alias execution_rounds retry_rounds
      alias child_batches retry_rounds
      alias retries retry_rounds
      alias retry_history retry_rounds

      def latest_retry_round
        retry_rounds.max_by(&:round)
      end

      def active_retry_round
        retry_rounds.reject(&:finished?).max_by(&:round)
      end
      alias current_retry_round active_retry_round

      def failed?
        failed_at.present?
      end

      def succeeded?
        finished? && !failed?
      end

      def finished?
        finished_at.present?
      end

      def enqueued?
        enqueued_at.present?
      end

      # Failed jobs no longer have tracking rows, so exclude them from the completed count.
      def completed_jobs
        finished? ? self[:completed_jobs] : [ total_jobs - pending_jobs - failed_jobs, 0 ].max
      end

      def failed_jobs
        finished? ? self[:failed_jobs] : jobs.failed.count
      end

      # Pending counts attempts, not logical jobs: while a retry is enqueued
      # and its previous attempt hasn't finished yet, both have tracking rows,
      # so the counts derived from it clamp at the logical totals.
      def pending_jobs
        finished? ? 0 : batch_executions.count
      end

      def progress_percentage
        return 0 if total_jobs == 0
        ([ total_jobs - pending_jobs, 0 ].max * 100.0 / total_jobs).round(2)
      end

      private
        def lineage_value(key)
          if lineage_column?(key)
            value = self[key]
            return value unless value.nil?
          end

          metadata[key] || metadata[key.to_sym]
        end

        def write_lineage_value(key, value)
          if lineage_column?(key)
            write_attribute(key, value)
          else
            self.metadata = metadata.to_h.merge(key => value)
          end
        end

        def lineage_column?(key)
          self.class.columns_hash.key?(key.to_s)
        end

      module ClassMethods
        def retry_rounds_for(batch)
          batch_id = batch.respond_to?(:id) ? batch.id : batch

          uncached do
            all.to_a
          end.select do |candidate|
            candidate.parent_batch_id&.to_i == batch_id.to_i
          end.sort_by(&:round)
        end
      end
    end
  end
end
