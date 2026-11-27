class AddFoldedRecurringExecutionsToSolidQueue < ActiveRecord::Migration[7.1]
  def up
    # Fresh installs create all of this with the base schema, so skip
    # anything that already exists
    add_column :solid_queue_recurring_executions, :folded_through, :datetime, if_not_exists: true
    add_column :solid_queue_recurring_executions, :folded_interval, :integer, if_not_exists: true

    # A recurring execution now outlives its job: the row is the evidence that
    # the occurrence ran, so job cleanup detaches it instead of deleting it.
    change_column_null :solid_queue_recurring_executions, :job_id, true

    remove_foreign_key :solid_queue_recurring_executions, :solid_queue_jobs, if_exists: true
    add_foreign_key :solid_queue_recurring_executions, :solid_queue_jobs, column: :job_id, on_delete: :nullify
  end

  def down
    remove_foreign_key :solid_queue_recurring_executions, :solid_queue_jobs, if_exists: true
    add_foreign_key :solid_queue_recurring_executions, :solid_queue_jobs, column: :job_id, on_delete: :cascade

    change_column_null :solid_queue_recurring_executions, :job_id, false
    remove_column :solid_queue_recurring_executions, :folded_interval, if_exists: true
    remove_column :solid_queue_recurring_executions, :folded_through, if_exists: true
  end
end
