require "test_helper"
require_relative "../../../lib/generators/solid_queue/update/templates/db/add_dedup_key_to_solid_queue"

class DedupKeyPendingMigrationsTest < ActiveSupport::TestCase
  self.use_transactional_tests = false

  setup do
    migrate(:down)
  end

  teardown do
    migrate(:up)
    SolidQueue::Job.instance_variable_set(:@dedup_migrated, nil)
    destroy_records
  end

  test "the dedup column counts as a pending migration" do
    assert_not SolidQueue::Job.dedup_migrated?
  end

  test "jobs enqueue in bulk without dedup bookkeeping" do
    assert_difference -> { SolidQueue::Job.count }, +2 do
      ActiveJob.perform_all_later([ DeduplicableJob.new("same"), DeduplicableJob.new("same") ])
    end
  end

  test "a repeated bulk request inserts every job again while the migration is pending" do
    ActiveJob.perform_all_later([ DeduplicableJob.new("same") ])

    assert_difference -> { SolidQueue::Job.count }, +2 do
      result = SolidQueue::Job.enqueue_all([
        DeduplicableJob.new("same"),
        DeduplicableJob.new("other")
      ])
      assert_equal 2, result.enqueued.size
      assert_empty result.duplicated
    end
  end

  test "single enqueue keeps working without the dedup column" do
    active_job = DeduplicableJob.new("solo")
    SolidQueue::Job.enqueue(active_job)

    assert active_job.successfully_enqueued?
    assert SolidQueue::Job.last.ready?
  end

  private
    def migrate(direction)
      ActiveRecord::Migration.suppress_messages do
        SolidQueue::Record.connection_pool.with_connection do |connection|
          AddDedupKeyToSolidQueue.new.exec_migration(connection, direction)
        end
      end

      SolidQueue::Job.reset_column_information
      SolidQueue::Job.instance_variable_set(:@dedup_migrated, nil)

      # Changing the jobs table's shape invalidates cached prepared statements
      SolidQueue::Record.connection_pool.disconnect!
    end
end
