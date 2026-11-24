# frozen_string_literal: true

require "test_helper"
require "active_support/testing/method_call_assertions"

class SolidCache::StatsTest < ActiveSupport::TestCase
  include ActiveSupport::Testing::TimeHelpers

  setup do
    @namespace = "test-#{SecureRandom.hex}"
  end

  # The no-database configuration manages a single connection itself and rejects
  # explicit shard names; every other configuration can pin to one shard.
  def single_shard_options
    default_database? ? {} : { shards: [ first_shard_key ] }
  end

  def test_stats_with_entries_no_shards
    skip unless ENV["SOLID_CACHE_CONFIG"] == "config/cache_no_database.yml"

    @cache = lookup_store(expiry_batch_size: 2, max_age: 2.weeks.to_i, max_entries: 1000)

    expected_empty = { connections: 1, connection_stats: { default: { max_age: 2.weeks.to_i, oldest_age: nil, max_entries: 1000, entries: 0 } }, namespace_stats: {} }

    assert_equal expected_empty, @cache.stats

    freeze_time
    @cache.write("foo", 1)
    @cache.write("bar", 1)

    SolidCache::Record.each_shard do
      SolidCache::Entry.update_all(created_at: Time.now - 20.minutes)
    end

    stats = @cache.stats
    assert_equal 1, stats[:connections]
    assert_equal 20.minutes.to_f, stats[:connection_stats][:default][:oldest_age]
    assert_equal 2, stats[:connection_stats][:default][:entries]

    group = stats[:namespace_stats][SolidCache::Entry.namespace_group_id(@namespace)]
    assert_equal 2, group[:entries]
    assert group[:byte_size] > 0
    assert_equal 0, group[:key_hash_collisions]
  end

  def test_stats_with_entries
    skip if ENV["SOLID_CACHE_CONFIG"]

    @cache = lookup_store(expiry_batch_size: 2, max_age: 2.weeks.to_i, max_entries: 1000)

    expected_empty = { connections: 4, connection_stats: {
      primary_shard_one: { max_age: 2.weeks.to_i, oldest_age: nil, max_entries: 1000, entries: 0 },
      primary_shard_two: { max_age: 2.weeks.to_i, oldest_age: nil, max_entries: 1000, entries: 0 },
      secondary_shard_one: { max_age: 2.weeks.to_i, oldest_age: nil, max_entries: 1000, entries: 0 },
      secondary_shard_two: { max_age: 2.weeks.to_i, oldest_age: nil, max_entries: 1000, entries: 0 }
    }, namespace_stats: {} }

    assert_equal expected_empty, @cache.stats

    freeze_time
    @cache.write("foo", 1)
    @cache.write("bar", 1)

    SolidCache::Record.each_shard do
      SolidCache::Entry.update_all(created_at: Time.now - 20.minutes)
    end

    stats = @cache.stats

    assert_equal 20.minutes.to_i, stats[:connection_stats].values.map { |value| value[:oldest_age] }.compact.min
    assert_equal 2, stats[:connection_stats].values.map { |value| value[:entries] }.compact.sum
  end

  def test_stats_one_shard
    skip if ENV["SOLID_CACHE_CONFIG"]

    @cache = lookup_store(expiry_batch_size: 2, max_age: 2.weeks.to_i, max_entries: 1000, shards: [ first_shard_key ])

    expected = {
      connections: 1,
      connection_stats: {
        first_shard_key => { max_age: 2.weeks.to_i, oldest_age: nil, max_entries: 1000, entries: 0 }
      },
      namespace_stats: {}
    }

    assert_equal expected, @cache.stats
  end

  def test_stats_multiple_shards
    skip if ENV["SOLID_CACHE_CONFIG"]

    @cache = lookup_store(expiry_batch_size: 2, max_age: 2.weeks.to_i, max_entries: 1000, shards: [ first_shard_key, second_shard_key ])

    expected = {
      connections: 2,
      connection_stats: {
        first_shard_key => { max_age: 2.weeks.to_i, oldest_age: nil, max_entries: 1000, entries: 0 },
        second_shard_key => { max_age: 2.weeks.to_i, oldest_age: nil, max_entries: 1000, entries: 0 }
      },
      namespace_stats: {}
    }

    assert_equal expected, @cache.stats
  end

  def test_stats_reports_namespace_sizes_without_leaking_keys_or_values
    @cache = lookup_store(single_shard_options)
    @cache.write("accounts/1", "secret-balance")
    @cache.write("accounts/2", "secret-balance-two")
    @cache.write("billing/1", "secret-invoice")

    stats = @cache.stats
    namespace_stats = stats[:namespace_stats]

    group_id = SolidCache::Entry.namespace_group_id(@namespace)
    assert_equal 3, namespace_stats[group_id][:entries]
    assert namespace_stats[group_id][:byte_size] > 0
    assert_equal 0, namespace_stats[group_id][:key_hash_collisions]

    rendered = stats.inspect
    assert_not_includes rendered, "secret-balance"
    assert_not_includes rendered, "secret-invoice"
    assert_not_includes rendered, "accounts/1"
    assert_not_includes rendered, "billing/1"
  end

  def test_stats_keeps_namespaces_separate
    accounts_cache = lookup_store(namespace: "accounts-#{SecureRandom.hex}", **single_shard_options)
    billing_cache = lookup_store(namespace: "billing-#{SecureRandom.hex}", **single_shard_options)

    accounts_cache.write("1", "value-a")
    billing_cache.write("1", "value-b")
    billing_cache.write("2", "value-c")

    stats = accounts_cache.stats[:namespace_stats]

    assert_equal 1, stats[SolidCache::Entry.namespace_group_id(accounts_cache.namespace)][:entries]
    assert_equal 2, stats[SolidCache::Entry.namespace_group_id(billing_cache.namespace)][:entries]
  end

  def test_key_hash_collision_is_detected_and_reported_without_exposing_values
    # A single shard guarantees both forced keys land in the same table, where
    # the unique key_hash index can actually collide
    @cache = lookup_store(single_shard_options)

    first_key = @cache.send(:normalize_key, "first", {})
    colliding_key = @cache.send(:normalize_key, "colliding", {})
    long_key = @cache.send(:normalize_key, "long-#{"x" * 2000}", {})

    # The long key is normalized (truncated with a digest suffix), but still
    # hashed through the regular key rules
    assert long_key.bytesize <= @cache.max_key_bytesize
    assert long_key.include?(":hash:")

    # Force the two real keys onto the same index hash while every other key
    # (including the truncated long key) keeps its real hash
    SolidCache::Entry.singleton_class.alias_method(:original_key_hash_for, :key_hash_for) unless SolidCache::Entry.respond_to?(:original_key_hash_for)
    SolidCache::Entry.define_singleton_method(:key_hash_for) do |key|
      [ first_key, colliding_key ].include?(key) ? 123_456 : original_key_hash_for(key)
    end

    begin
      @cache.write("first", "first-secret-value")
      @cache.write("colliding", "colliding-secret-value")
      @cache.write("long-#{"x" * 2000}", "long-secret-value")

      # The first key is shadowed by the colliding write; the colliding key reads.
      # Reads must happen before the original hashing is restored.
      assert_nil @cache.read("first")
      assert_equal "colliding-secret-value", @cache.read("colliding")
      assert_equal "long-secret-value", @cache.read("long-#{"x" * 2000}")
    ensure
      SolidCache::Entry.singleton_class.alias_method(:key_hash_for, :original_key_hash_for)
      SolidCache::Entry.singleton_class.remove_method(:original_key_hash_for)
    end

    stats = @cache.stats[:namespace_stats]
    group = stats[SolidCache::Entry.namespace_group_id(@namespace)]

    # Three entries were written, but the collision overwrote one row
    assert_equal 2, group[:entries]
    assert_equal 1, group[:key_hash_collisions]

    rendered = stats.inspect
    assert_not_includes rendered, "first-secret-value"
    assert_not_includes rendered, "colliding-secret-value"
    assert_not_includes rendered, "long-secret-value"
    assert_not_includes rendered, first_key
    assert_not_includes rendered, colliding_key
  end

  def test_rewriting_the_same_key_is_not_reported_as_a_collision
    @cache = lookup_store(single_shard_options)
    @cache.write("stable", "one")
    @cache.write("stable", "two")

    group = @cache.stats[:namespace_stats][SolidCache::Entry.namespace_group_id(@namespace)]
    assert_equal 1, group[:entries]
    assert_equal 0, group[:key_hash_collisions]
    assert_equal "two", @cache.read("stable")
  end

  def test_write_multi_collision_is_counted_per_namespace
    @cache = lookup_store(single_shard_options)

    first_key = @cache.send(:normalize_key, "multi-first", {})
    colliding_key = @cache.send(:normalize_key, "multi-colliding", {})

    SolidCache::Entry.singleton_class.alias_method(:original_key_hash_for, :key_hash_for) unless SolidCache::Entry.respond_to?(:original_key_hash_for)
    SolidCache::Entry.define_singleton_method(:key_hash_for) do |key|
      [ first_key, colliding_key ].include?(key) ? 654_321 : original_key_hash_for(key)
    end

    begin
      @cache.write("multi-first", "first-value")
      @cache.write_multi("multi-colliding" => "colliding-value", "other" => "other-value")
    ensure
      SolidCache::Entry.singleton_class.alias_method(:key_hash_for, :original_key_hash_for)
      SolidCache::Entry.singleton_class.remove_method(:original_key_hash_for)
    end

    group = @cache.stats[:namespace_stats][SolidCache::Entry.namespace_group_id(@namespace)]
    assert_equal 2, group[:entries]
    assert_equal 1, group[:key_hash_collisions]
  end
end

