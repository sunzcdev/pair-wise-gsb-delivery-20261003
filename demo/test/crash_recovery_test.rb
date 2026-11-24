#!/usr/bin/env ruby
# frozen_string_literal: true

# Crash-recovery verification across real OS processes. The "crash" is a
# Process.exit! in a child Ruby process, so nothing in memory survives: the
# parent only observes what was durably written to the two SQLite files.
#
# Run: ruby demo/test/crash_recovery_test.rb
require "open3"
require_relative "../support"

$failures = 0
def check(name)
  ok = yield
  puts "#{ok ? "PASS" : "FAIL"}  #{name}"
  $failures += 1 unless ok
end

def run(*args)
  out, status = Open3.capture2e(RbConfig.ruby,
                                File.join(OutboxDemo::ROOT, "demo", "run.rb"), *args)
  [out, status]
end

OutboxDemo.reset!
OutboxDemo.load_domain!

# One committed business record waiting for hand-off.
run("place_order", "SKU-CRASH", "3")

# Child process dies between the queue commit and the outbox status update.
out, status = run("relay_crash")
check("crash child exited non-zero") { !status.success? }
check("crash child logged the simulated crash") { out.include?("SIMULATED CRASH") }

# Cold observation: job is durable in the queue DB, outbox row still pending.
outbox = OutboxMessage.first
job = SolidQueue::Job.find_by(active_job_id: outbox.solid_queue_job_id)
check("after crash: outbox row still pending") { outbox.reload.pending? }
check("after crash: Solid Queue job already committed") { !job.nil? }
check("after crash: ready execution is consumable") { SolidQueue::ReadyExecution.count == 1 }

# A brand-new process retries the hand-off.
out, status = run("relay")
check("recovery relay exits zero") { status.success? }
check("recovery relay reports duplicate-based recovery") { out.include?("already existed") }
check("recovery relay created no duplicate job") { SolidQueue::Job.count == 1 }
check("recovery relay marked the outbox dispatched") { outbox.reload.dispatched? }
check("recovery recorded the existing provider id") do
  outbox.solid_queue_provider_id == job.id
end

# Further retries are no-ops.
run("relay")
check("additional relay runs do not create jobs") { SolidQueue::Job.count == 1 }

# The recovered job is fully consumable end to end.
out, = run("work", "10")
check("recovered job was performed") { out.include?("FulfillOrderJob#perform") }
check("queue drained after performance") do
  SolidQueue::ReadyExecution.count.zero? &&
    SolidQueue::ClaimedExecution.count.zero? &&
    SolidQueue::Job.where.not(finished_at: nil).count == 1
end

# Strict one-to-one: one business record, one outbox row, one queue job.
check("one order -> one outbox row -> one Solid Queue job") do
  Order.count == 1 && OutboxMessage.count == 1 && SolidQueue::Job.count == 1
end

puts "\n#{$failures.zero? ? "ALL CRASH-RECOVERY CHECKS PASSED" : "#{$failures} CHECK(S) FAILED"}"
exit($failures.zero? ? 0 : 1)
