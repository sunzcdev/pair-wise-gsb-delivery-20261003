# frozen_string_literal: true

require "test_helper"

class SolidQueue::ScheduledDispatchScanningTest < ActiveSupport::TestCase
  self.use_transactional_tests = false

  class PlainScanJob < ApplicationJob
    def perform(value); end
  end

  class LimitedScanJob < ApplicationJob
    limits_concurrency to: 1, key: ->(_value = nil) { "scheduled-scan-key" }

    def perform(value); end
  end

  setup do
    SolidQueue::ReadyExecution.delete_all
    SolidQueue::BlockedExecution.delete_all
    SolidQueue::ScheduledExecution.delete_all
    SolidQueue::ClaimedExecution.delete_all
    SolidQueue::Semaphore.delete_all
    SolidQueue::Job.delete_all
  end

  teardown do
    travel_back
    SolidQueue::ReadyExecution.delete_all
    SolidQueue::BlockedExecution.delete_all
    SolidQueue::ScheduledExecution.delete_all
    SolidQueue::ClaimedExecution.delete_all
    SolidQueue::Semaphore.delete_all
    SolidQueue::Job.delete_all
  end

  test "scans past a concurrency-blocked head to a job that can enter ready" do
    LimitedScanJob.perform_later("hold")

    head_id = LimitedScanJob.set(wait: 1.minute).perform_later("head").provider_job_id
    available_id = PlainScanJob.set(wait: 1.minute).perform_later("available").provider_job_id

    travel_to 2.minutes.from_now

    report = dispatch_with_report(1)

    assert SolidQueue::BlockedExecution.exists?(job_id: head_id)
    assert SolidQueue::ReadyExecution.exists?(job_id: available_id)
    assert_not SolidQueue::ScheduledExecution.exists?(job_id: available_id)

    assert_equal 1, report[:ready]
    assert_equal 1, report[:blocked]
    assert_equal 2, report[:scanned]
    assert_equal 2, report[:size]
  end

  test "batch size counts ready jobs while the scan stays bounded by scan_limit" do
    LimitedScanJob.perform_later("hold")

    head_ids = 5.times.map { |i| LimitedScanJob.set(wait: 1.minute).perform_later("head-#{i}").provider_job_id }
    plain_ids = 4.times.map { |i| PlainScanJob.set(wait: 1.minute).perform_later("plain-#{i}").provider_job_id }

    travel_to 2.minutes.from_now

    report = dispatch_with_report(3)

    assert_equal 3, report[:ready]
    assert_equal 5, report[:blocked]
    assert_equal 8, report[:scanned]
    assert_equal 8, report[:size]

    assert head_ids.all? { |id| SolidQueue::BlockedExecution.exists?(job_id: id) }
    assert plain_ids.first(3).all? { |id| SolidQueue::ReadyExecution.exists?(job_id: id) }
    assert SolidQueue::ScheduledExecution.exists?(job_id: plain_ids.last)
    assert_not SolidQueue::ReadyExecution.exists?(job_id: plain_ids.last)
  end

  test "scan_limit is a hard bound even when nothing becomes ready" do
    LimitedScanJob.perform_later("hold")

    head_ids = 6.times.map { |i| LimitedScanJob.set(wait: 1.minute).perform_later("head-#{i}").provider_job_id }
    plain_id = PlainScanJob.set(wait: 1.minute).perform_later("plain").provider_job_id

    travel_to 2.minutes.from_now

    report = dispatch_with_report(1, scan_limit: 4)

    assert_equal 4, report[:scanned]
    assert_equal 0, report[:ready]
    assert_equal 4, report[:blocked]
    assert head_ids.first(4).all? { |id| SolidQueue::BlockedExecution.exists?(job_id: id) }
    assert SolidQueue::ScheduledExecution.exists?(job_id: head_ids[4])
    assert SolidQueue::ScheduledExecution.exists?(job_id: plain_id)
    assert_not SolidQueue::ReadyExecution.exists?(job_id: plain_id)
  end

  test "blocked heads keep their order and reach ready as the first segment completes" do
    hold = LimitedScanJob.perform_later("hold")
    head_ids = 3.times.map { |i| LimitedScanJob.set(wait: 1.minute).perform_later("head-#{i}").provider_job_id }
    plain_ids = 2.times.map { |i| PlainScanJob.set(wait: 1.minute).perform_later("plain-#{i}").provider_job_id }

    travel_to 2.minutes.from_now
    SolidQueue::ScheduledExecution.dispatch_next_batch(2)

    assert head_ids.all? { |id| SolidQueue::BlockedExecution.exists?(job_id: id) }
    assert plain_ids.all? { |id| SolidQueue::ReadyExecution.exists?(job_id: id) }

    # Completing the job holding the permit transfers it to the oldest blocked head.
    SolidQueue::Job.find(hold.provider_job_id).destroy!
    assert SolidQueue::ReadyExecution.exists?(job_id: head_ids[0])
    assert SolidQueue::BlockedExecution.exists?(job_id: head_ids[1])

    SolidQueue::Job.find(head_ids[0]).destroy!
    assert SolidQueue::ReadyExecution.exists?(job_id: head_ids[1])
    assert SolidQueue::BlockedExecution.exists?(job_id: head_ids[2])

    SolidQueue::Job.find(head_ids[1]).destroy!
    assert SolidQueue::ReadyExecution.exists?(job_id: head_ids[2])
    assert_equal 0, SolidQueue::BlockedExecution.where(job_id: head_ids).count
  end

  private
    def dispatch_with_report(batch_size, **options)
      payload = nil
      callback = ->(*args) { payload = args.last }

      ActiveSupport::Notifications.subscribed(callback, "dispatch_scheduled.solid_queue") do
        SolidQueue::ScheduledExecution.dispatch_next_batch(batch_size, **options)
      end

      payload
    end
end