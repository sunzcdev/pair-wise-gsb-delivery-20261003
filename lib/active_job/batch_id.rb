# frozen_string_literal: true

# Inspired by active_job/core.rb docs
# https://github.com/rails/rails/blob/1c2529b9a6ba5a1eff58be0d0373d7d9d401015b/activejob/lib/active_job/core.rb#L136
module ActiveJob
  module BatchId
    extend ActiveSupport::Concern

    included do
      attr_accessor :batch_id
      attr_accessor :callback_batch_id

      around_perform :wrap_perform_in_batch_context
    end

    # Seed membership at construction for enqueue paths deferred until after the
    # batch context ends, notably bulk enqueue. Enqueueing inside another batch
    # rebinds membership; enqueueing without a batch preserves it.
    # SolidQueue::Job.enqueue_all repeats this capture because bulk enqueue
    # bypasses #enqueue.
    def initialize(*arguments, **kwargs)
      super
      self.batch_id = SolidQueue::Batch.current_batch_id if solid_queue_job?
    end

    def enqueue(options = {})
      self.batch_id = SolidQueue::Batch.current_batch_id || batch_id if solid_queue_job?
      super
    end

    def serialize
      super.tap do |data|
        data["batch_id"] = batch_id if batch_id
        data["callback_batch_id"] = callback_batch_id if callback_batch_id
      end
    end

    def deserialize(job_data)
      super
      self.batch_id = job_data["batch_id"]
      self.callback_batch_id = job_data["callback_batch_id"]
    end

    def batch
      batch_id_to_load = callback_batch_id || batch_id
      return if batch_id_to_load.nil?
      return @batch if defined?(@batch) && @loaded_batch_id == batch_id_to_load

      @loaded_batch_id = batch_id_to_load
      @batch = SolidQueue::Batch.find_by(id: batch_id_to_load)
    end

    private

      # Exposes the job's batch while it performs, so batches enqueued from
      # inside #perform become its children. Plain jobs enqueued there don't
      # join the batch: that still takes an explicit batch.enqueue block.
      def wrap_perform_in_batch_context(&block)
        if solid_queue_job? && batch_id.present?
          SolidQueue::Batch.wrap_in_performing_batch_context(batch_id, &block)
        else
          block.call
        end
      end

      def solid_queue_job?
        self.class.queue_adapter_name == "solid_queue"
      end
  end
end
