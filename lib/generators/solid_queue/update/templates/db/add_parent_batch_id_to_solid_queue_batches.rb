class AddParentBatchIdToSolidQueueBatches < ActiveRecord::Migration[7.1]
  def change
    # Batches created by the current add_batches_to_solid_queue migration
    # already have this column, so skip anything that already exists
    add_column :solid_queue_batches, :parent_batch_id, :bigint, if_not_exists: true
    add_index :solid_queue_batches, :parent_batch_id, if_not_exists: true
  end
end
