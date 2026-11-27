# frozen_string_literal: true

require "test_helper"

class SolidQueue::ConcurrencyKeyMigrationTest < ActiveSupport::TestCase
  # A deploy that replaces concurrency keys computed from the job class and a
  # user id with an explicit group shared by both job classes, while jobs
  # limited by the old keys are still running and blocked.
  class LegacyJobA < ActiveJob::Base
    def perform(user_id)
    end
  end

  class LegacyJobB < ActiveJob::Base
    def perform(user_id)
    end
  end

  OLD_POLICY = { to: 1, key: ->(user_id) { user_id }, duration: 1.minute }.freeze
  NEW_POLICY = { to: 1, key: ->(user_id) { user_id }, group: "UserTasks", duration: 1.minute }.freeze
  NEW_KEY = "UserTasks/7"

  setup do
    @process = SolidQueue::Process.register(kind: "Worker", pid: 42, name: "worker-123")
    set_policy OLD_POLICY
  end

  test "migrating folds two legacy keys into the new group and debits the slots they hold" do
    a_running = claim(LegacyJobA.perform_later(7))
    a_blocked = job_for(LegacyJobA.perform_later(7))
    b_ready = job_for(LegacyJobB.perform_later(7))
    b_blocked = job_for(LegacyJobB.perform_later(7))

    assert a_blocked.blocked?
    assert b_ready.ready?
    assert b_blocked.blocked?

    old_keys = [ a_running.job.concurrency_key, b_ready.concurrency_key ]
    assert_equal 2, old_keys.uniq.size

    deploy_new_policy
    migrate_keys from: old_keys, to: NEW_KEY, limit: 1, duration: 1.minute

    # Every unfinished job, running or waiting, now belongs to the new group
    [ a_running.job, a_blocked, b_ready, b_blocked ].each do |job|
      assert_equal NEW_KEY, job.reload.concurrency_key
    end
    assert_equal NEW_KEY, a_blocked.blocked_execution.reload.concurrency_key
    assert_equal NEW_KEY, b_blocked.blocked_execution.reload.concurrency_key

    # The group's semaphore only opens once the two slots the old keys still have out come back
    assert_equal(-1, new_semaphore.value)

    # New submissions are admitted under the new policy: the group is full, so this one blocks
    c = job_for(LegacyJobA.perform_later(7))
    assert c.blocked?
    assert_equal NEW_KEY, c.concurrency_key
  end

  test "in-flight jobs settle their tokens under their original keys and release no extra positions" do
    a_running = claim(LegacyJobA.perform_later(7))
    a_blocked = job_for(LegacyJobA.perform_later(7))
    b_ready = job_for(LegacyJobB.perform_later(7))
    b_blocked = job_for(LegacyJobB.perform_later(7))

    old_keys = [ a_running.job.concurrency_key, b_ready.concurrency_key ]
    legacy_semaphores = SolidQueue::Semaphore.where(key: old_keys).index_by(&:key)

    deploy_new_policy
    migrate_keys from: old_keys, to: NEW_KEY, limit: 1, duration: 1.minute
    c = job_for(LegacyJobA.perform_later(7))

    # The running job's token was recorded against the semaphore it got it from
    a_running = SolidQueue::ClaimedExecution.find(a_running.id)
    assert_equal legacy_semaphores[old_keys.first].id, a_running.job.acquired_semaphore_id

    # The stuck running item finishes: its token settles under the original key...
    a_running.perform
    assert_equal 1, legacy_semaphores[old_keys.first].reload.value

    # ...and no position opens up in the new group
    assert_equal(-1, new_semaphore.reload.value)
    assert a_blocked.reload.blocked?
    assert b_blocked.reload.blocked?
    assert c.reload.blocked?

    # The other legacy side ends the same way
    claim_job(b_ready).perform
    assert_equal 1, legacy_semaphores[old_keys.second].reload.value
    assert_equal(-1, new_semaphore.reload.value)
    assert c.reload.blocked?
  end

  test "once the legacy semaphores expire the merged group unblocks one job at a time" do
    a_running = claim(LegacyJobA.perform_later(7))
    a_blocked = job_for(LegacyJobA.perform_later(7))
    b_ready = job_for(LegacyJobB.perform_later(7))
    b_blocked = job_for(LegacyJobB.perform_later(7))

    old_keys = [ a_running.job.concurrency_key, b_ready.concurrency_key ]

    deploy_new_policy
    migrate_keys from: old_keys, to: NEW_KEY, limit: 1, duration: 1.minute
    c = job_for(LegacyJobA.perform_later(7))

    # A worker picks up the remaining legacy-ready job before maintenance runs
    claim_job(b_ready)

    travel_to 2.minutes.from_now do
      # What the dispatcher's concurrency maintenance does
      SolidQueue::Semaphore.expired.delete_all
      SolidQueue::BlockedExecution.unblock(10)

      # Exactly one position opens in the merged group
      assert a_blocked.reload.ready?
      assert b_blocked.reload.blocked?
      assert c.reload.blocked?
      assert_equal 0, new_semaphore.value

      # Each finish opens exactly one more, never going over the limit
      claim_job(a_blocked).perform
      assert b_blocked.reload.ready?
      assert c.reload.blocked?
      assert_equal 0, new_semaphore.reload.value

      claim_job(b_blocked).perform
      assert c.reload.ready?
      assert_equal 0, new_semaphore.reload.value
    end
  end

  test "slots the old keys don't hold are open to the new group right away" do
    deploy_new_policy
    migrate_keys from: [ "LegacyJobA/7", "LegacyJobB/7" ], to: NEW_KEY, limit: 1, duration: 1.minute

    assert_equal 1, new_semaphore.value

    c = job_for(LegacyJobA.perform_later(7))
    assert c.ready?
    assert_equal 0, new_semaphore.reload.value
  end

  private
    def set_policy(policy)
      [ LegacyJobA, LegacyJobB ].each { |job_class| job_class.limits_concurrency(**policy) }
    end

    # The new declaration going out with the deploy
    def deploy_new_policy
      set_policy NEW_POLICY
    end

    # The data migration documented in "Changing concurrency keys": folds the
    # +from+ keys into the group key +to+, debiting the slots they still have out
    def migrate_keys(from:, to:, limit:, duration:)
      keys = Array(from) + [ to ]

      SolidQueue::Job.transaction do
        # Slots handed out under these keys sit with ready and claimed jobs
        held = SolidQueue::Job.where(finished_at: nil, concurrency_key: keys)
          .where(id: SolidQueue::ReadyExecution.select(:job_id))
          .or(SolidQueue::Job.where(finished_at: nil, concurrency_key: keys).where(id: SolidQueue::ClaimedExecution.select(:job_id)))
          .count

        # Move every unfinished job -- running, blocked or scheduled -- into the
        # new group. Running jobs keep the semaphore they recorded, so their
        # tokens still settle against the original keys.
        SolidQueue::Job.where(finished_at: nil, concurrency_key: Array(from)).update_all(concurrency_key: to)
        SolidQueue::BlockedExecution.where(concurrency_key: Array(from)).update_all(concurrency_key: to)

        # Open the group's semaphore with the slots still out already debited
        semaphore = SolidQueue::Semaphore.create_or_find_by!(key: to, expires_at: duration.from_now)
        semaphore.update!(value: limit - held, expires_at: duration.from_now)
      end
    end

    def new_semaphore
      SolidQueue::Semaphore.find_by!(key: NEW_KEY)
    end

    def job_for(active_job)
      SolidQueue::Job.find_by!(active_job_id: active_job.job_id)
    end

    def claim(active_job)
      claim_job(job_for(active_job))
    end

    def claim_job(job)
      SolidQueue::ReadyExecution.claim(job.queue_name, 1, @process.id)
      SolidQueue::ClaimedExecution.find_by!(job_id: job.id)
    end
end

