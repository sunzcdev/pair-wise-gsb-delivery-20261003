# frozen_string_literal: true

module SolidQueue
  class QueueSelector
    # Default strategy: walk the configured queues in order and never look at a
    # later queue while an earlier one still has ready jobs. Jobs within each
    # queue keep being selected in their own (priority, id) order.
    class StrictStrategy
      def initialize(**)
      end

      def order(queue_names)
        queue_names
      end
    end
  end
end
