# frozen_string_literal: true

module SolidQueue
  class Batch
    # Dynamic batches stay open for explicit appends from any connection until
    # they are sealed. A batch can only complete once sealed, so a completion
    # check can never mistake "no outstanding work seen in one query" for "all
    # the work that will ever exist has finished". Static batches keep their
    # original, query-driven completion semantics.
    module Sealable
      extend ActiveSupport::Concern

      class_methods do
        # A dynamic batch: jobs may keep being appended (including from inside
        # running jobs or from another process) until #seal is called.
        def enqueue_dynamic(description: nil, on_success: nil, on_failure: nil,
          on_finish: nil, metadata: nil, **extra_metadata, &block)
          raise PendingMigrations unless migrated?

          new(dynamic: true).tap do |batch|
            attributes = { description:, on_success:, on_failure:, on_finish:,
                           metadata: (metadata || {}).merge(extra_metadata) }
            batch.assign_attributes(attributes)
            batch.enqueue(&block)
          end
        end
      end

      # Close the batch against further appends. Idempotent: sealing an already
      # sealed batch is a no-op, and a batch whose work is already done finishes
      # right away. Sealing a finished batch is a caller error. Static batches
      # don't participate: calling seal keeps their query-driven completion and
      # never marks them sealed.
      def seal
        return self unless dynamic?

        with_lock do
          reload
          # Idempotent: an already sealed batch (including one that sealing just
          # finished) has nothing left to do.
          return self if sealed?
          # An unsealed dynamic batch must never have finished: completion is
          # gated on the seal, so reaching this state means a caller error.
          raise AlreadyFinished, "Can't seal an already finished batch" if finished?

          Batch.where(id: id).unsealed.update_all(sealed_at: Time.current)
          reload
        end

        finish
        self
      end
    end
  end
end