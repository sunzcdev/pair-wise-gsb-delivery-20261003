class AddRetryRoundsToSolidQueueBatches < ActiveRecord::Migration[7.1]
  def change
    add_column :solid_queue_batches, :round, :integer, default: 1, null: false, if_not_exists: true
    add_column :solid_queue_batches, :parent_batch_id, :bigint, if_not_exists: true
    add_column :solid_queue_batches, :original_batch_id, :bigint, if_not_exists: true

    add_index :solid_queue_batches, :parent_batch_id, if_not_exists: true
    add_index :solid_queue_batches, :round, if_not_exists: true
  end
end
