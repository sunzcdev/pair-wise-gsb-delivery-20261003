class BlankKeyJob < ApplicationJob
  queue_as :background

  # Returns nil for nil arguments, so no business key is produced
  dedup_by ->(arg) { arg&.to_s }

  def perform(arg)
    JobBuffer.add(arg)
  end
end
