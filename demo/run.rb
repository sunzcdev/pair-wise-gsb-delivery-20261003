#!/usr/bin/env ruby
# frozen_string_literal: true

require_relative "support"

# Single-process subcommands (run inside one Ruby process each, so exit!
# genuinely simulates a process death with no in-memory state to recover from).
COMMANDS = %w[
  reset
  place_order
  place_failing_order
  relay
  relay_crash
  work
  state
  scenario
].freeze

command = ARGV[0] || "scenario"
abort "unknown command: #{command}; use one of #{COMMANDS.join(", ")}" unless COMMANDS.include?(command)

OutboxDemo.setup!
OutboxDemo.load_domain!

case command
when "reset"
  OutboxDemo.reset!

when "place_order"
  order = PlaceOrderWithOutbox.call(sku: ARGV[1] || "SKU-1", quantity: (ARGV[2] || 1).to_i)
  OutboxDemo.audit("placed order##{order.id} (outbox pending, queue DB untouched)")

when "place_failing_order"
  begin
    PlaceOrderWithOutbox.call(sku: "BROKEN", quantity: 7, simulate_failure: true)
  rescue PlaceOrderWithOutbox::Rollback => e
    OutboxDemo.audit("business transaction rolled back: #{e.message}")
  end
  puts "orders in DB: #{Order.count} | outbox rows in DB: #{OutboxMessage.count}"

when "relay"
  n = OutboxRelay.run_once
  OutboxDemo.audit("relay run complete, processed #{n} pending row(s)")

when "relay_crash"
  OutboxRelay.run_once(crash_point: OutboxRelay::CRASH_AFTER_QUEUE_COMMIT)

when "work"
  n = DemoWorker.run(max_jobs: (ARGV[1] || 100).to_i)
  OutboxDemo.audit("worker run complete, performed #{n} job(s)")

when "state"
  OutboxDemo.print_state

when "scenario"
  require_relative "scenario"
  Scenario.run!
end
