class AddConcurrencyReleasesToSolidQueue < ActiveRecord::Migration[7.1]
  def change
    create_table :solid_queue_concurrency_releases, if_not_exists: true do |t|
      t.bigint :claim_id, null: false
      t.bigint :job_id, null: false
      t.string :concurrency_key, null: false
      t.bigint :semaphore_id
      t.integer :concurrency_limit
      t.integer :concurrency_duration
      t.datetime :released_at

      t.datetime :created_at, null: false
      t.datetime :updated_at, null: false

      t.index :claim_id, unique: true, name: "index_solid_queue_concurrency_releases_on_claim_id"
      t.index :job_id, name: "index_solid_queue_concurrency_releases_on_job_id"
    end
  end
end
