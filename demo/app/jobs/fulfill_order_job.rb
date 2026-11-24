# frozen_string_literal: true

# The job that eventually runs on Solid Queue workers. Its class must be
# resolvable by the queue side, and Active Job serialization must not depend on
# any business-DB state -- everything the worker needs travels in arguments.
class FulfillOrderJob < ActiveJob::Base
  queue_as :orders

  # In a real system this would call the warehouse API. Here it just records
  # what it received so the demo can assert the payload survived the hand-off.
  def perform(order_id, sku:, quantity:)
    OutboxDemo.audit(
      "FulfillOrderJob#perform order_id=#{order_id} sku=#{sku} quantity=#{quantity} " \
      "job_id=#{job_id} provider_job_id=#{provider_job_id}"
    )
  end
end
