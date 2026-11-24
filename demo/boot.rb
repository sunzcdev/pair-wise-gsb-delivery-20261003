# frozen_string_literal: true

# Standalone (non-Rails-app) bootstrap for the cross-database outbox demo.
# Solid Queue's engine normally contributes app/models through the Rails
# autoloader; outside a Rails application we register those directories with
# Zeitwerk ourselves.
$LOAD_PATH.unshift(File.expand_path("../lib", __dir__))

require "fileutils"
require "rails"
require "active_record"
require "active_job"
require "solid_queue"

# The crash simulation uses Process.exit!, which bypasses at_exit hooks and
# does not flush buffered stdio. Syncing stdout/stderr ensures the audit lines
# emitted immediately before the hard exit are actually observable.
STDOUT.sync = true
STDERR.sync = true

module OutboxDemo
  ROOT = File.expand_path("..", __dir__)
  TMP  = File.join(ROOT, "demo", "tmp")

  module Bootstrap
    def self.load_solid_queue!
      loader = Zeitwerk::Loader.new
      loader.push_dir(File.join(OutboxDemo::ROOT, "app", "models", "solid_queue"), namespace: SolidQueue)
      loader.push_dir(File.join(OutboxDemo::ROOT, "app", "jobs"))
      loader.setup
    end
  end
end

OutboxDemo::Bootstrap.load_solid_queue!

# Outside a Rails application the engine's initializer that extends Active Job
# never runs, so apply the same includes here: Solid Queue's Job model calls
# #concurrency_key and reads #batch_id on every enqueued job.
ActiveJob::Base.include(ActiveJob::ConcurrencyControls)
ActiveJob::Base.include(ActiveJob::BatchId)
