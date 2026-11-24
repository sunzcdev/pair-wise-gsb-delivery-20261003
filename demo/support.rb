# frozen_string_literal: true

require_relative "boot"
require_relative "database"

module OutboxDemo
  # Absolute path so child processes spawned from any working directory append
  # to one shared chronological log.
  AUDIT_FILE = File.expand_path(File.join(TMP, "audit.log"))

  module_function

  # Append-only narrative of every cross-database step, used to prove ordering
  # and recovery in the demo output.
  def audit(message)
    FileUtils.mkdir_p(TMP)
    line = "#{Time.now.utc.strftime("%H:%M:%S.%3N")} [#{Process.pid}] #{message}"
    File.open(AUDIT_FILE, "a") { |f| f.puts(line) }
    puts line
  end

  def setup!
    Database.connect!
  end

  def reset!
    FileUtils.mkdir_p(TMP)
    FileUtils.rm_f([BUSINESS_DB, QUEUE_DB, AUDIT_FILE])
    setup!
    Database.create_schema!
    audit("fresh databases created")
    audit("business DB: #{BUSINESS_DB}")
    audit("queue DB:    #{QUEUE_DB}")
  end

  def load_domain!
    require_relatively("app/models/order.rb")
    require_relatively("app/models/outbox_message.rb")
    require_relatively("app/jobs/fulfill_order_job.rb")
    require_relatively("app/services/place_order_with_outbox.rb")
    require_relatively("app/services/outbox_relay.rb")
    require_relatively("app/services/demo_worker.rb")
  end

  def require_relatively(path)
    require File.join(File.expand_path("..", __dir__), "demo", path)
  end

  def print_state
    puts "\n=== business DB: orders ==="
    Order.order(:id).each do |o|
      puts "  order##{o.id} sku=#{o.sku} qty=#{o.quantity} state=#{o.state}"
    end

    puts "\n=== business DB: outbox_messages ==="
    OutboxMessage.order(:id).each do |m|
      puts "  outbox##{m.id} order_id=#{m.order_id} status=#{m.status} " \
           "job_id=#{m.solid_queue_job_id} provider_id=#{m.solid_queue_provider_id} attempts=#{m.attempts}"
    end

    puts "\n=== queue DB: solid_queue_jobs ==="
    SolidQueue::Job.order(:id).each do |j|
      puts "  job##{j.id} active_job_id=#{j.active_job_id} class=#{j.class_name} " \
           "queue=#{j.queue_name} finished_at=#{j.finished_at}"
    end

    puts "\n=== queue DB: ready / claimed executions ==="
    puts "  ready:    #{SolidQueue::ReadyExecution.count}"
    puts "  claimed:  #{SolidQueue::ClaimedExecution.count}"
    puts "  failed:   #{SolidQueue::FailedExecution.count}"
    verify_one_to_one!
  end

  # The headline invariant: every dispatched outbox row maps to exactly one
  # Solid Queue job, and every job maps back to exactly one outbox row.
  def verify_one_to_one!
    dispatched = OutboxMessage.where(status: OutboxMessage::DISPATCHED).pluck(:solid_queue_job_id)
    queue_ids = SolidQueue::Job.pluck(:active_job_id)

    checks = {
      "dispatched outbox rows have a Solid Queue job" => (dispatched - queue_ids).empty?,
      "no duplicate Solid Queue jobs for one active_job_id" =>
        queue_ids.length == queue_ids.uniq.length,
      "every Solid Queue job traces back to one outbox row" =>
        (queue_ids - OutboxMessage.pluck(:solid_queue_job_id)).empty?,
      "dispatched outbox count == Solid Queue job count" =>
        dispatched.length == SolidQueue::Job.count
    }

    puts "\n=== invariant checks ==="
    checks.each do |name, ok|
      puts "  #{ok ? "PASS" : "FAIL"}  #{name}"
    end

    puts "  INFO  pending outbox rows (not yet handed off): #{OutboxMessage.pending.count}"
    checks.values.all?
  end
end
