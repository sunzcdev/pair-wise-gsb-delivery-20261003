# frozen_string_literal: true

require "securerandom"

# Places a business order and records the intent to enqueue a Solid Queue job,
# atomically, inside the BUSINESS database only. Nothing touches the queue
# database here: the two databases cannot share a transaction, so job creation
# is deferred to a separate, recoverable hand-off (see OutboxRelay).
class PlaceOrderWithOutbox
  class Rollback < StandardError; end

  def self.call(sku:, quantity:, simulate_failure: false)
    new(sku: sku, quantity: quantity, simulate_failure: simulate_failure).call
  end

  def initialize(sku:, quantity:, simulate_failure:)
    @sku = sku
    @quantity = quantity
    @simulate_failure = simulate_failure
  end

  def call
    order = nil
    job_id = nil
    ActiveRecord::Base.transaction do
      order = Order.create!(sku: @sku, quantity: @quantity, state: "placed")

      # The Active Job id is minted inside the transaction and stored on the
      # outbox row, so it becomes durable only if the business commit succeeds.
      # The relay reuses this exact id on every attempt, which is the basis for
      # idempotent recovery.
      job_id = SecureRandom.uuid
      active_job = FulfillOrderJob.new(order.id, sku: @sku, quantity: @quantity)
      active_job.job_id = job_id
      active_job.queue_name = "orders"

      OutboxMessage.create!(
        order: order,
        job_class: FulfillOrderJob.name,
        # The full Active Job serialization envelope, produced by Active Job's
        # own serializer. The relay hands it to Solid Queue verbatim, so
        # positional/keyword semantics (the _aj_ruby2_keywords marker included)
        # survive the cross-database hop, and the queue side needs no access to
        # the business database.
        arguments: active_job.serialize,
        queue_name: "orders",
        status: OutboxMessage::PENDING,
        solid_queue_job_id: job_id
      )

      # Crash point for the failure scenario: if the business operation rolls
      # back, the outbox row rolls back with it, so no consumable task ever
      # exists for a non-existent order.
      raise Rollback, "simulated business failure" if @simulate_failure
    end

    OutboxDemo.audit("business commit: order=#{order.id} outbox_job_id=#{job_id}")
    order
  end
end
