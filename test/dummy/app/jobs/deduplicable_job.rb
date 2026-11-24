class DeduplicableJob < ApplicationJob
  queue_as :background

  dedup_by ->(arg) { arg }

  def perform(arg)
    JobBuffer.add(arg)
  end
end
