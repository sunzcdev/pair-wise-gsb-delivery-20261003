# frozen_string_literal: true

module SolidQueue
  class Worker < Processes::Poller
    module QueueControls
      def release_unstarted_executions(queue_names = SolidQueue::Pause.queue_names)
        queue_names = Array(queue_names).compact.map(&:to_s)
        return 0 if queue_names.empty?

        cancelled = pool.cancel_pending_executions do |execution|
          queue_names.include?(execution.job.queue_name)
        end
        release_claimed_executions(cancelled)

        # Also return claims checked out of the database but not admitted to the
        # execution pool yet. Exclude both claims just returned and claims whose
        # user perform lifecycle has already started.
        excluded_claim_ids = (cancelled + pool.performing_executions).filter_map(&:id).uniq
        database_claims = unstarted_database_claims(queue_names, excluded_claim_ids).to_a
        release_claimed_executions(database_claims)

        cancelled.size + database_claims.size
      end

      def pause_queue(queue_name = nil)
        queue_control_mutex.synchronize do
          release_unstarted_executions(queue_name ? [ queue_name ] : SolidQueue::Pause.queue_names)
        end
      rescue StandardError => error
        handle_thread_error(error)
        request_termination if error.is_a?(Processes::UnrecoverableError)
        0
      end

      alias release_paused_executions release_unstarted_executions
      alias release_pending_executions release_unstarted_executions
      alias return_unstarted_executions release_unstarted_executions

      def resume_queue(*)
        wake_up
      end

      def heartbeat
        super
        pause_queue
      end

      def subscribe_to_queue_control_events
        @queue_pause_subscription = ActiveSupport::Notifications.subscribe("pause.solid_queue") do |*args|
          payload = args.last
          pause_queue(payload[:queue_name]) if payload[:queue_name]
        end

        @queue_resume_subscription = ActiveSupport::Notifications.subscribe("resume.solid_queue") do
          resume_queue
        end
      end

      def unsubscribe_from_queue_control_events
        ActiveSupport::Notifications.unsubscribe(@queue_pause_subscription) if @queue_pause_subscription
        ActiveSupport::Notifications.unsubscribe(@queue_resume_subscription) if @queue_resume_subscription
      ensure
        @queue_pause_subscription = @queue_resume_subscription = nil
      end

      private
        def queue_control_mutex
          @queue_control_mutex ||= Mutex.new
        end

        def unstarted_database_claims(queue_names, excluded_claim_ids)
          claims = SolidQueue::ClaimedExecution
            .joins(:job)
            .where(process_id: process_id)
            .where(solid_queue_jobs: { queue_name: queue_names })

          claims = claims.where.not(id: excluded_claim_ids) if excluded_claim_ids.any?
          claims
        end

        def release_paused_claims(executions)
          paused_queues = SolidQueue::Pause.queue_names
          return executions if paused_queues.empty?

          executions.reject! do |execution|
            next false unless paused_queues.include?(execution.job.queue_name)

            release_claimed_execution(execution)
            true
          end

          executions
        end

        def release_claimed_executions(executions)
          executions.each { |execution| release_claimed_execution(execution) }
          wake_up if executions.any?
        end

        def release_claimed_execution(execution)
          execution.release
        rescue Exception => error
          handle_thread_error(error)
          request_termination if error.is_a?(Processes::UnrecoverableError)
        end
    end
  end
end
