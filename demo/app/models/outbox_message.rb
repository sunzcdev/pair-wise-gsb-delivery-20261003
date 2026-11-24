# frozen_string_literal: true

# Transactional outbox row, in the business DB.
#
# State machine:
#   pending     -- written in the same transaction as the business record,
#                  not yet handed off to Solid Queue
#   dispatched  -- the Solid Queue job was durably created in the queue DB
#
# +solid_queue_job_id+ is generated *before* the business transaction commits
# and reused on every relay attempt, which is what makes the hand-off
# idempotent: Solid Queue's unique index on active_job_id rejects a repeated
# enqueue instead of creating a duplicate job.
class OutboxMessage < ActiveRecord::Base
  PENDING    = "pending"
  DISPATCHED = "dispatched"

  belongs_to :order, optional: true

  serialize :arguments, coder: JSON

  validates :job_class, :arguments, presence: true
  validates :solid_queue_job_id, uniqueness: true, allow_nil: true

  scope :pending, -> { where(status: PENDING).order(:id) }

  def pending?
    status == PENDING
  end

  def dispatched?
    status == DISPATCHED
  end
end
