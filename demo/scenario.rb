# frozen_string_literal: true

require "open3"

# End-to-end driver for the recoverable cross-database hand-off. Every "process
# death" is a real separate OS process that calls Process.exit!, so recovery
# genuinely starts from cold caches and whatever landed on disk.
module Scenario
  module_function

  def run!
    section("0. provision two separate SQLite databases")
    run_command("reset")

    section("1. business failure: rollback leaves no order AND no outbox row")
    run_command("place_failing_order")
    check("no consumable task exists",
          OutboxMessage.count.zero? && SolidQueue::Job.count.zero? && Order.count.zero?)

    section("2. successful business commit: order + outbox row in one local transaction")
    run_command("place_order SKU-100 2")
    run_command("state")

    section("3. relay crashes AFTER the queue DB commit but BEFORE the outbox update")
    # Separate process: its in-memory state dies with it.
    stdout, status = Open3.capture2e(ruby, script, "relay_crash")
    print_output(stdout)
    check("crashed process exited non-zero", !status.success?)
    run_command("state")
    check("queue job exists while outbox row is still pending (the dangerous gap)",
          SolidQueue::Job.count == 1 && OutboxMessage.pending.count == 1)

    section("4. restart and relay again: duplicate job rejected, hand-off recovered exactly once")
    run_command("relay")
    run_command("state")
    check("still exactly one Solid Queue job (no duplicate)", SolidQueue::Job.count == 1)
    check("outbox row now dispatched",
          OutboxMessage.where(status: OutboxMessage::DISPATCHED).count == 1)

    section("5. relay is idempotent: a third run does nothing")
    run_command("relay")
    check("job count unchanged", SolidQueue::Job.count == 1)

    section("6. a second business record gets its own distinct job")
    run_command("place_order SKU-200 5")
    run_command("relay")
    run_command("state")

    section("7. worker consumes the queue and performs the real Active Jobs")
    run_command("work 10")
    run_command("state")
    check("both jobs finished", SolidQueue::Job.where.not(finished_at: nil).count == 2)
    check("no ready/claimed rows left behind",
          SolidQueue::ReadyExecution.count.zero? && SolidQueue::ClaimedExecution.count.zero?)

    section("audit trail (chronological)")
    puts File.read(OutboxDemo::AUDIT_FILE)
  end

  def section(title)
    puts "\n" + ("=" * 78)
    puts title
    puts ("=" * 78)
  end

  def check(name, ok)
    puts "  -> #{ok ? "PASS" : "FAIL"}: #{name}"
    raise "invariant failed: #{name}" unless ok
  end

  def run_command(args)
    stdout, status = Open3.capture2e(ruby, script, *args.split)
    print_output(stdout)
    raise("command failed: #{args}\n#{stdout}") unless status.success?
    stdout
  end

  def print_output(output)
    output.each_line { |line| puts "   | #{line}" }
  end

  def ruby
    RbConfig.ruby
  end

  def script
    File.join(OutboxDemo::ROOT, "demo", "run.rb")
  end
end
