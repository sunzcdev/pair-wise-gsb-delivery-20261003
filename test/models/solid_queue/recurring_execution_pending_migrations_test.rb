# frozen_string_literal: true

require "test_helper"
require_relative "../../../lib/generators/solid_queue/update/templates/db/add_folded_recurring_executions_to_solid_queue"

# Recreate an app that hasn't run the optional folding migration, to prove the
# pre-folding behaviour is kept until then and that the migration is reversible
# and matches the base schema.
class RecurringExecutionPendingMigrationsTest < ActiveSupport::TestCase
  self.use_transactional_tests = false

  setup do
    migrate(:down)
  end

  teardown do
    migrate(:up)
    destroy_records
  end

  test "the folding schema counts as pending migrations" do
    assert_not SolidQueue::RecurringExecution.supports_folding?
    assert_not SolidQueue::RecurringExecution.detachable?
  end

  test "recording still dedupes without the folding schema" do
    task = recurring_task
    at = Time.utc(2026, 1, 1)

    assert_difference -> { SolidQueue::Job.count }, +1 do
      assert task.enqueue(at: at)
    end

    assert_no_difference -> { SolidQueue::Job.count } do
      assert_equal false, task.enqueue(at: at)
    end
  end

  test "cleanup leaves history alone without the folding schema" do
    task = recurring_task
    task.enqueue(at: Time.utc(2026, 1, 1))

    # Without the folding schema, job_id can't be NULL, so there's nothing to
    # fold yet: cleanup keeps the behaviour of doing nothing.
    assert_no_difference -> { SolidQueue::RecurringExecution.count } do
      SolidQueue::RecurringExecution.clear_in_batches
    end
  end

  private
    def recurring_task
      SolidQueue::RecurringTask.create!(
        key: "pending-task", class_name: "AddToBufferJob", schedule: "every minute", arguments: [ 1 ], static: true
      )
    end

    def migrate(direction)
      SolidQueue::Record.connection_pool.with_connection do |connection|
        ActiveRecord::Migration.suppress_messages do
          AddFoldedRecurringExecutionsToSolidQueue.new.exec_migration(connection, direction)
        end
      end

      SolidQueue::RecurringExecution.reset_column_information
      SolidQueue::RecurringExecution.reset_supports_folding!

      # Changing the table's shape invalidates cached prepared statements whose
      # SQL text didn't change, which PostgreSQL rejects with
      # PreparedStatementCacheExpired, so start from fresh connections.
      SolidQueue::Record.connection_pool.disconnect!
    end
end
