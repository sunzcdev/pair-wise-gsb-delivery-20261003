#!/usr/bin/env ruby
# frozen_string_literal: true

# Self-contained verification of the cross-database recoverable outbox.
# Run: ruby demo/test/outbox_test.rb
require_relative "../support"

OutboxDemo.reset!
OutboxDemo.load_domain!

$failures = 0
def check(name)
  ok = yield
  puts "#{ok ? "PASS" : "FAIL"}  #{name}"
  $failures += 1 unless ok
  ok
end

# ---------------------------------------------------------------------------
# 1. Business rollback: nothing durable, no task the queue could ever consume.
# ---------------------------------------------------------------------------
begin
  PlaceOrderWithOutbox.call(sku: "BROKEN", quantity: 9, simulate_failure: true)
rescue PlaceOrderWithOutbox::Rollback
end
check("rolled-back business tx leaves zero orders") { Order.count.zero? }
check("rolled-back business tx leaves zero outbox rows") { OutboxMessage.count.zero? }
check("queue DB stays empty after business failure") { SolidQueue::Job.count.zero? }

# ---------------------------------------------------------------------------
# 2. Business commit: order + outbox row atomically, queue DB untouched.
# ---------------------------------------------------------------------------
order = PlaceOrderWithOutbox.call(sku: "SKU-1", quantity: 2)
outbox = OutboxMessage.find_by!(order: order)
check("committed order exists") { order.persisted? }
check("outbox row is pending") { outbox.pending? }
check("outbox row carries a stable job id") { outbox.solid_queue_job_id.present? }
check("outbox payload is a full Active Job envelope") do
  outbox.arguments["job_class"] == "FulfillOrderJob" &&
    outbox.arguments["job_id"] == outbox.solid_queue_job_id
end
check("no queue job before the relay runs") { SolidQueue::Job.count.zero? }

# ---------------------------------------------------------------------------
# 3. Relay: one outbox row becomes exactly one Solid Queue job + ready row.
# ---------------------------------------------------------------------------
processed = OutboxRelay.run_once
check("relay processed the single pending row") { processed == 1 }
outbox.reload
queue_job = SolidQueue::Job.find_by!(active_job_id: outbox.solid_queue_job_id)
check("outbox row marked dispatched") { outbox.dispatched? }
check("provider id recorded") { outbox.solid_queue_provider_id == queue_job.id }
check("one ready execution created") { SolidQueue::ReadyExecution.count == 1 }
check("stored active_job_id matches the outbox job id") do
  queue_job.active_job_id == outbox.solid_queue_job_id
end
check("stored arguments survive the cross-database hop") do
  queue_job.arguments["arguments"].last.key?("_aj_ruby2_keywords")
end

# ---------------------------------------------------------------------------
# 4. Idempotency: re-running the relay changes nothing.
# ---------------------------------------------------------------------------
check("second relay run processes nothing") { OutboxRelay.run_once.zero? }
check("still exactly one Solid Queue job") { SolidQueue::Job.count == 1 }

# ---------------------------------------------------------------------------
# 5. Simulated duplicate at the database layer: no second job is ever created.
# ---------------------------------------------------------------------------
duplicate_job = FulfillOrderJob.new(order.id, sku: "SKU-1", quantity: 2)
duplicate_job.job_id = outbox.solid_queue_job_id # same stable id
duplicate = false
begin
  SolidQueue::Record.transaction { SolidQueue::Job.enqueue(duplicate_job) }
rescue SolidQueue::Job::EnqueueError => e
  duplicate = e.message.match?(/UNIQUE constraint|unique|duplicate/i)
end
check("re-enqueue with the same job id is hard-rejected") { duplicate }
check("job count unchanged after rejected duplicate") { SolidQueue::Job.count == 1 }

# ---------------------------------------------------------------------------
# 6. Multiple business records: strict one-to-one mapping.
# ---------------------------------------------------------------------------
2.times { |i| PlaceOrderWithOutbox.call(sku: "SKU-#{i + 10}", quantity: i + 1) }
OutboxRelay.run_once
check("three outbox rows, three Solid Queue jobs") do
  OutboxMessage.count == 3 && SolidQueue::Job.count == 3
end
check("every dispatched outbox row maps 1:1 to a queue job") do
  ids = OutboxMessage.pluck(:solid_queue_job_id).sort
  ids == SolidQueue::Job.pluck(:active_job_id).sort && ids.uniq.length == 3
end

# ---------------------------------------------------------------------------
# 7. Worker performs every job and drains the queue.
# ---------------------------------------------------------------------------
performed = DemoWorker.run(max_jobs: 10)
check("worker performed all three jobs") { performed == 3 }
check("no ready or claimed executions remain") do
  SolidQueue::ReadyExecution.count.zero? && SolidQueue::ClaimedExecution.count.zero?
end
check("all jobs finished") { SolidQueue::Job.where.not(finished_at: nil).count == 3 }

# ---------------------------------------------------------------------------
# 8. Cross-database isolation: the two files really are separate databases.
# ---------------------------------------------------------------------------
require "sqlite3"
biz_db = SQLite3::Database.new(OutboxDemo::BUSINESS_DB)
q_db = SQLite3::Database.new(OutboxDemo::QUEUE_DB)
business_tables = biz_db.execute("SELECT name FROM sqlite_master WHERE type='table'").flatten
queue_tables = q_db.execute("SELECT name FROM sqlite_master WHERE type='table'").flatten
check("business DB has no Solid Queue tables") do
  (business_tables.grep(/solid_queue/)).empty?
end
check("queue DB has no business tables") do
  (queue_tables & %w[orders outbox_messages]).empty?
end
check("queue DB has Solid Queue tables") { queue_tables.include?("solid_queue_jobs") }

puts "\n#{$failures.zero? ? "ALL CHECKS PASSED" : "#{$failures} CHECK(S) FAILED"}"
exit($failures.zero? ? 0 : 1)
