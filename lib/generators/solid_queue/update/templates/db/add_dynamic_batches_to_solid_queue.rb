class AddDynamicBatchesToSolidQueue < ActiveRecord::Migration[7.1]
  def change
    # Dynamic batches opt in to explicit append/seal semantics. Existing
    # batches stay static: dynamic stays NULL/false and they never carry a
    # sealed_at, so their behavior is unchanged without a backfill.
    add_column :solid_queue_batches, :dynamic, :boolean, if_not_exists: true
    add_column :solid_queue_batches, :sealed_at, :datetime, if_not_exists: true
  end
end