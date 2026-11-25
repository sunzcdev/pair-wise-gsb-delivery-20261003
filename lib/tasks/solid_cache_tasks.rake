# frozen_string_literal: true

namespace :solid_cache do
  desc "Copy over the migration, and set cache"
  task :install do
    Rails::Command.invoke :generate, [ "solid_cache:install" ]
  end

  # Admin entry point for a namespace-scoped, point-in-time clear.
  #
  #   bin/rails solid_cache:clear_namespace[NAMESPACE]
  #
  # Prints the sweep report, including the cutoff time and the rows deleted on
  # every shard. Set SWEEP_PENDING=1 to resume a previously paused sweep without
  # bumping the generation again.
  desc "Clear a single cache namespace without affecting writes or other namespaces"
  task :clear_namespace, [ :namespace ] => :environment do |_task, args|
    namespace = args[:namespace] || ENV["NAMESPACE"]
    raise "A namespace must be provided, e.g. bin/rails solid_cache:clear_namespace[tenant-123]" if namespace.nil?

    options = { namespace: namespace }
    options[:sweep_pending] = true if ENV["SWEEP_PENDING"].present?

    report = Rails.cache.clear(options)
    puts "Cleared namespace #{report[:namespace].inspect} at #{report[:cutoff_at].iso8601}"
    report[:shards].each do |shard, shard_report|
      status = shard_report[:paused] ? "PAUSED" : "swept"
      puts "  #{shard}: #{status}, generation #{shard_report[:generation]} " \
           "(cleared #{shard_report[:cleared_generation]}), #{shard_report[:deleted]} rows deleted"
    end
  end
end
