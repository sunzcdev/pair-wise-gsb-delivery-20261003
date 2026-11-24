class AddDedupKeyToSolidQueue < ActiveRecord::Migration[7.1]
  def change
    # Fresh installs create all of this with the base schema, so skip
    # anything that already exists
    add_column :solid_queue_jobs, :dedup_key, :string, if_not_exists: true

    # NULLs never conflict, so jobs without a declared dedup key keep working
    # while duplicate business keys are rejected at the database level.
    unless index_exists?(:solid_queue_jobs, :dedup_key, name: "index_solid_queue_jobs_on_dedup_key")
      add_index :solid_queue_jobs, :dedup_key, unique: true,
        name: "index_solid_queue_jobs_on_dedup_key"
    end
  end
end
