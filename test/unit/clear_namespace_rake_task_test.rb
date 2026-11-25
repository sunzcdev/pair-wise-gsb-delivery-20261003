# frozen_string_literal: true

require "test_helper"
require "rake"

class ClearNamespaceRakeTaskTest < ActiveSupport::TestCase
  setup do
    @namespace = "rake-#{SecureRandom.hex}"
    Rake::Task.tasks.each(&:reenable)
    Rails.application.load_tasks if Rake::Task.tasks.empty?
    Rake::Task["solid_cache:clear_namespace"].reenable
  end

  test "clears the given namespace and prints a per-shard report" do
    cache = ActiveSupport::Cache.lookup_store(:solid_cache_store, namespace: @namespace)
    cache.write("alpha", "one")
    cache.write("beta", "two")

    output = capture_io do
      Rake::Task["solid_cache:clear_namespace"].invoke(@namespace)
    end.join

    assert_match(/Cleared namespace "#{Regexp.escape(@namespace)}"/, output)
    assert_match(/rows deleted/, output)
    assert_nil cache.read("alpha")
    assert_nil cache.read("beta")

    # Writes made after the cutoff remain readable.
    cache.write("gamma", "three")
    assert_equal "three", cache.read("gamma")
  end

  test "requires a namespace" do
    assert_raises(RuntimeError) do
      capture_io { Rake::Task["solid_cache:clear_namespace"].invoke }
    end
  end
end
