# frozen_string_literal: true

module ActiveJob
  # Allows a job class to declare a business key that identifies equivalent
  # requests. Bulk enqueues (ActiveJob.perform_all_later) use it to recognize
  # jobs that are already enqueued instead of inserting them again:
  #
  #   class PublishInvoiceJob < ApplicationJob
  #     dedup_by ->(invoice) { invoice.id }
  #   end
  #
  # The key is computed from the job's arguments, like concurrency keys, and
  # namespaced by job class so identical arguments in different jobs never
  # collide. Jobs without a +dedup_by+ declaration have no key and are always
  # enqueued as new.
  module Dedupable
    extend ActiveSupport::Concern

    included do
      class_attribute :dedup_key_callable, instance_accessor: false, default: nil
    end

    class_methods do
      def dedup_by(callable = nil, &block)
        self.dedup_key_callable = callable || block
      end
    end

    # Stable identifier of the work this job represents, or +nil+ when the job
    # class doesn't declare one. Two jobs of the same class with the same
    # dedup key describe the same business request.
    def solid_queue_dedup_key
      return unless self.class.dedup_key_callable

      parameter = compute_dedup_parameter(self.class.dedup_key_callable)
      [ self.class.name, parameter ].join("/") if parameter.present?
    end

    private
      def compute_dedup_parameter(callable)
        case callable
        when String, Symbol
          callable.to_s
        when Proc
          instance_exec(*arguments, &callable)
        end
      end
  end
end
