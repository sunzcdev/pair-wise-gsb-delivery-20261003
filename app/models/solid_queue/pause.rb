# frozen_string_literal: true

module SolidQueue
  class Pause < Record
    class << self
      def queue_names
        pluck(:queue_name)
      end

      def paused?(queue_name)
        exists?(queue_name: queue_name.to_s)
      end

      def pause_queue(queue_name)
        queue_name = queue_name.to_s

        create_or_find_by!(queue_name: queue_name).tap do
          # Local workers subscribe to this event and return admissions that
          # have not entered +perform+ yet. Database-backed workers created in
          # another process observe the Pause row through polling as well.
          SolidQueue.instrument(:pause, queue_name: queue_name)
        end
      end

      def resume_queue(queue_name)
        queue_name = queue_name.to_s
        deleted = where(queue_name: queue_name).delete_all

        SolidQueue.instrument(:resume, queue_name: queue_name) do
          deleted
        end
      end

      alias pause pause_queue
      alias resume resume_queue
    end
  end
end
