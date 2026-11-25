# frozen_string_literal: true

require "test_helper"

class NamespaceClearTest < ActiveSupport::TestCase
  setup do
    @namespace = "test-#{SecureRandom.hex}"
  end

  test "plain clear with no namespace still wipes everything" do
    cache = lookup_store(clear_with: :delete)
    cache.write("alpha", "one")
    cache.write("beta", "two")

    cache.clear

    assert_equal 0, uncached_entry_count
    assert_nil cache.read("alpha")
    assert_nil cache.read("beta")
  end

  test "scoped clear returns a report with the cutoff and per-shard deleted counts" do
    cache = lookup_store
    10.times { |i| cache.write("key#{i}", "value#{i}") }

    report = cache.clear(namespace: @namespace)

    assert_equal @namespace, report[:namespace]
    assert_kind_of Time, report[:cutoff_at]

    report[:shards].each do |shard, shard_report|
      assert_kind_of Symbol, shard
      assert_equal 1, shard_report[:generation]
      assert_equal 0, shard_report[:cleared_generation]
      assert shard_report.key?(:deleted)
      assert_not shard_report.key?(:paused)
    end

    assert_equal 10, report[:shards].values.sum { |shard_report| shard_report[:deleted] }
    assert_equal 0, uncached_entry_count
  end

  test "entries that existed within the scope are removed" do
    cache = lookup_store
    cache.write("old1", "a")
    cache.write("old2", "b")

    cache.clear(namespace: @namespace)

    assert_nil cache.read("old1")
    assert_nil cache.read("old2")
  end

  test "writes made after the cutoff stay readable" do
    cache = lookup_store
    cache.write("old", "before")

    cache.clear(namespace: @namespace)
    cache.write("new", "after")
    cache.write("old", "rewritten")

    assert_equal "after", cache.read("new")
    assert_equal "rewritten", cache.read("old")
  end

  test "a key rewritten while the sweep is in progress survives with its new version" do
    cache = lookup_store
    cache.write("contended", "v1")
    key_shard = shard_for(cache, "contended")
    # Pick a brand new key that is guaranteed to live on the same shard.
    new_key = key_on_shard(cache, key_shard)

    gate = lambda do |shard|
      if shard == key_shard
        # The generation has already been bumped, so these writes belong to the
        # surviving generation while the sweep is waiting at the gate.
        cache.write("contended", "v2")
        cache.write(new_key, "v3")
      end
      true
    end

    report = cache.clear(namespace: @namespace, gate: gate)

    assert_equal "v2", cache.read("contended")
    assert_equal "v3", cache.read(new_key)

    shard_report = report[:shards][key_shard]
    assert_equal 1, shard_report[:generation]
    assert_equal 0, shard_report[:cleared_generation]
    # Both the rewritten row and the brand new row carry generation 1 and must
    # not be counted as deleted.
    assert_equal 0, shard_report[:deleted]
  end

  test "a sibling namespace remains available and untouched" do
    sibling_namespace = "sibling-#{SecureRandom.hex}"
    cache = lookup_store
    sibling = ActiveSupport::Cache.lookup_store(:solid_cache_store, namespace: sibling_namespace)

    cache.write("mine", "secret")
    sibling.write("theirs", "keep")

    cache.clear(namespace: @namespace)

    assert_nil cache.read("mine")
    assert_equal "keep", sibling.read("theirs")
    assert_equal 1, uncached_entry_count
  end

  test "a namespace with a shared string prefix is not matched" do
    tricky_namespace = "#{@namespace}-extra"
    cache = lookup_store
    tricky = ActiveSupport::Cache.lookup_store(:solid_cache_store, namespace: tricky_namespace)

    cache.write("key", "mine")
    tricky.write("key", "theirs")

    cache.clear(namespace: @namespace)

    assert_nil cache.read("key")
    assert_equal "theirs", tricky.read("key")
  end

  test "SQL wildcard characters in a namespace are escaped" do
    wildcard_namespace = "wild_#{SecureRandom.hex}%"
    cache = ActiveSupport::Cache.lookup_store(:solid_cache_store, namespace: wildcard_namespace)
    similar_namespace = "wildX#{wildcard_namespace.delete_prefix("wild")}"
    similar = ActiveSupport::Cache.lookup_store(:solid_cache_store, namespace: similar_namespace)

    cache.write("key", "mine")
    similar.write("key", "theirs")

    cache.clear(namespace: wildcard_namespace)

    assert_nil cache.read("key")
    assert_equal "theirs", similar.read("key")
  end

  test "clearing with namespace true uses the store's own namespace" do
    cache = lookup_store
    cache.write("key", "value")

    report = cache.clear(namespace: true)

    assert_equal @namespace, report[:namespace]
    assert_nil cache.read("key")
  end

  test "a nil namespace falls back to a regular full clear" do
    cache = lookup_store(clear_with: :delete)
    cache.write("key", "value")

    cache.clear(namespace: nil)

    assert_nil cache.read("key")
  end

  test "each shard can be paused independently and resumed later" do
    skip "requires multiple shards" unless SolidCache.configuration.sharded?

    cache = lookup_store(shards: [ first_shard_key, second_shard_key ])
    skip "needs two distinct shards" if first_shard_key.nil? || second_shard_key.nil?

    first_old_key = key_on_shard(cache, first_shard_key)
    second_old_key = key_on_shard(cache, second_shard_key)
    cache.write(first_old_key, "old-1")
    cache.write(second_old_key, "old-2")

    # Pause only the second shard.
    gate = ->(shard) { shard != second_shard_key }
    report = cache.clear(namespace: @namespace, gate: gate)

    first_report = report[:shards][first_shard_key]
    second_report = report[:shards][second_shard_key]

    assert_not first_report.key?(:paused)
    assert second_report[:paused]
    assert_equal 0, second_report[:deleted]
    assert_equal 1, first_report[:generation]
    assert_equal 1, second_report[:generation]

    # While the second shard is paused, both new and rewritten writes survive.
    second_new_key = key_on_shard(cache, second_shard_key, excluding: [ second_old_key ])
    cache.write(second_new_key, "new-2")
    cache.write(second_old_key, "rewritten-2")
    assert_nil cache.read(first_old_key)
    assert_equal "rewritten-2", cache.read(second_old_key)
    assert_equal "new-2", cache.read(second_new_key)

    # Resume the sweep on the paused shard without bumping the generation again.
    resume_report = cache.clear(namespace: @namespace, sweep_pending: true)
    resume_second = resume_report[:shards][second_shard_key]

    assert_not resume_second.key?(:paused)
    assert_equal 1, resume_second[:generation]
    assert_equal 0, resume_second[:cleared_generation]
    # The original row was already replaced by the surviving rewrite, so there is
    # nothing left from generation 0 to delete.
    assert_equal 0, resume_second[:deleted]

    assert_equal "rewritten-2", cache.read(second_old_key)
    assert_equal "new-2", cache.read(second_new_key)
  end

  test "paused shard still deletes untouched old versions when resumed" do
    skip "requires multiple shards" unless SolidCache.configuration.sharded?

    cache = lookup_store(shards: [ first_shard_key, second_shard_key ])
    skip "needs two distinct shards" if first_shard_key.nil? || second_shard_key.nil?
    untouched_key = key_on_shard(cache, second_shard_key)
    cache.write(untouched_key, "stale")

    gate = ->(shard) { shard != second_shard_key }
    cache.clear(namespace: @namespace, gate: gate)

    assert_equal "stale", cache.read(untouched_key)

    resume_report = cache.clear(namespace: @namespace, sweep_pending: true)
    assert resume_report[:shards][second_shard_key][:deleted] >= 1
    assert_nil cache.read(untouched_key)
  end

  test "repeated scoped clears keep incrementing the generation" do
    cache = lookup_store
    cache.write("key", "v0")

    first = cache.clear(namespace: @namespace)
    cache.write("key", "v1")
    second = cache.clear(namespace: @namespace)
    cache.write("key", "v2")

    assert_equal 1, first[:shards].values.first[:generation]
    assert_equal 2, second[:shards].values.first[:generation]
    assert_equal "v2", cache.read("key")
  end

  test "sweep_pending without a prior clear is a no-op" do
    fresh_namespace = "fresh-#{SecureRandom.hex}"
    cache = ActiveSupport::Cache.lookup_store(:solid_cache_store, namespace: fresh_namespace)
    cache.write("key", "value")

    report = cache.clear(namespace: fresh_namespace, sweep_pending: true)

    report[:shards].each_value do |shard_report|
      assert_equal 0, shard_report[:generation]
      assert_equal 0, shard_report[:deleted]
    end
    assert_equal "value", cache.read("key")
  end

  test "the in-memory local cache does not serve stale entries after a scoped clear" do
    cache = lookup_store
    cache.with_local_cache do
      cache.write("key", "v1")
      assert_equal "v1", cache.read("key")

      report = cache.clear(namespace: @namespace)
      assert_kind_of Hash, report
      assert_nil cache.read("key")

      cache.write("key", "v2")
      assert_equal "v2", cache.read("key")
    end
  end

  test "incremented counters survive a concurrent clear" do
    cache = lookup_store
    cache.write("counter", 10, raw: true)
    key_shard = shard_for(cache, "counter")

    gate = lambda do |shard|
      if shard == key_shard
        cache.increment("counter", 5)
      end
      true
    end

    cache.clear(namespace: @namespace, gate: gate)

    assert_equal 15, cache.read("counter", raw: true)
  end

  test "a store without a namespace can clear its unprefixed entries by generation" do
    cache = ActiveSupport::Cache.lookup_store(:solid_cache_store)
    cache.write("plain", "v1")
    marker = SolidCache::Generation::DEFAULT_NAMESPACE
    generation_before = SolidCache::Record.each_shard.sum { SolidCache::Generation.current(marker) }

    report = cache.clear(namespace: marker)
    assert_nil cache.read("plain")

    cache.write("plain", "v2")
    assert_equal "v2", cache.read("plain")

    generation_after = SolidCache::Record.each_shard.sum { SolidCache::Generation.current(marker) }
    assert_equal SolidCache::Record.each_shard.count, generation_after - generation_before
    report[:shards].each_value do |shard_report|
      assert_equal shard_report[:cleared_generation] + 1, shard_report[:generation]
    end
  end

  test "parallel writers never leave an entry from the cleared generation behind" do
    cache = lookup_store
    cache.write("shared", "original")

    clear_thread = Thread.new do
      cache.clear(namespace: @namespace)
    end

    writer_threads = 4.times.map do |writer|
      Thread.new do
        25.times do |i|
          cache.write("shared", "writer-#{writer}-#{i}")
          cache.write("private-#{writer}", i)
        end
      end
    end

    clear_thread.join
    writer_threads.each(&:join)

    # The generation-based sweep must never delete a row written after the
    # cutoff: every surviving row carries the generation introduced by the
    # clear. Durability of writes racing a full table truncate is a separate
    # database concern (notably SQLite file locking) and is not asserted here.
    SolidCache::Record.each_shard do
      SolidCache::Entry.uncached do
        SolidCache::Entry.where(generation: ...1).find_each do |stale|
          flunk "stale generation 0 entry survived: #{stale.key.inspect}"
        end
      end
    end
  end

  private
    def shard_for(cache, key)
      normalized = cache.send(:normalize_key, key, {})
      cache.send(:connections).assign([ normalized ]).keys.first
    end

    # Find a key name that is assigned to the given shard.
    def key_on_shard(cache, shard, excluding: [])
      1000.times do |i|
        candidate = "shard-key-#{SecureRandom.hex}-#{i}"
        next if excluding.include?(candidate)
        return candidate if shard_for(cache, candidate) == shard
      end
      raise "Could not find a key for shard #{shard}"
    end
end
