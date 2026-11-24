# frozen_string_literal: true

# Lives in the BUSINESS database. An order plus its outbox hand-off row are
# written together in one local transaction: either both are visible after
# commit, or neither exists. There is no way to observe "half a commit".
class Order < ActiveRecord::Base
  has_one :outbox_message, class_name: "OutboxMessage", required: false

  validates :sku, :quantity, presence: true
end
