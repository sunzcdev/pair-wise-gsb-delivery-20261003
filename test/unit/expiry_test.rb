# frozen_string_literal: true

require "test_helper"
require "active_support/testing/method_call_assertions"

class SolidCache::ExpiryTest < ActiveSupport::TestCase
  include ActiveSupport::Testing::TimeHelpers
  include ActiveJob::TestHelper

  setup do
    @namespace = "test-#{SecureRandom.hex}"
    @single_shard_cluster = default_database? ? {} : { shards: [ first_shard_key ] }
  end

  teardown do
    wait_for_background_tasks(@cache) if @cache
  end

  [ :thread, :job ].each do |expiry_method|
    test "expires old records (#{expiry_method})" do
      SolidCache::Store.any_instance.stubs(:rand).returns(0)

      @cache = lookup_store(expiry_batch_size: 3, max_age: 2.weeks, expiry_method: expiry_method)
      default_shard_keys = shard_keys(@cache, first_shard_key)
      @cache.write(default_shard_keys[0], 1)
      @cache.write(default_shard_keys[1], 2)
      assert_equal 1, @cache.read(default_shard_keys[0])
      assert_equal 2, @cache.read(default_shard_keys[1])

      send_entries_back_in_time(3.weeks)

      @cache.write(default_shard_keys[2], 3)
      @cache.write(default_shard_keys[3], 4)

      wait_for_background_tasks(@cache)
      perform_enqueued_jobs

      assert_nil @cache.read(default_shard_keys[0])
      assert_nil @cache.read(default_shard_keys[1])
      assert_equal 3, @cache.read(default_shard_keys[2])
      assert_equal 4, @cache.read(default_shard_keys[3])
    end

    test "expires records when the cache is full (#{expiry_method})" do
      SolidCache::Store.any_instance.stubs(:rand).returns(0)

      @cache = lookup_store(expiry_batch_size: 3, max_age: nil, max_entries: 2, expiry_method: expiry_method)
      default_shard_keys = shard_keys(@cache, first_shard_key)
      @cache.write(default_shard_keys[0], 1)
      @cache.write(default_shard_keys[1], 2)

      wait_for_background_tasks(@cache)

      @cache.write(default_shard_keys[2], 3)
      @cache.write(default_shard_keys[3], 4)

      wait_for_background_tasks(@cache)
      perform_enqueued_jobs

      # Two records have been deleted
      assert_equal 1, SolidCache::Record.each_shard.sum { SolidCache::Entry.count }
    end

    test "expires records when the cache is full via max_size (#{expiry_method})" do
      SolidCache::Store.any_instance.stubs(:rand).returns(0)

      @cache = lookup_store(expiry_batch_size: 3, max_age: nil, max_size: 1000, expiry_method: expiry_method)
      default_shard_keys = shard_keys(@cache, first_shard_key)
      @cache.write(default_shard_keys[0], "a" * 350)
      @cache.write(default_shard_keys[1], "a" * 350)

      wait_for_background_tasks(@cache)
      perform_enqueued_jobs

      @cache.write(default_shard_keys[2], "a" * 350)
      @cache.write(default_shard_keys[3], "a" * 350)

      wait_for_background_tasks(@cache)
      perform_enqueued_jobs

      assert_operator SolidCache::Record.each_shard.sum { SolidCache::Entry.count }, :<, 4
    end

    test "expires records no shards (#{expiry_method})" do
      SolidCache::Store.any_instance.stubs(:rand).returns(0)

      @cache = ActiveSupport::Cache.lookup_store(:solid_cache_store, expiry_batch_size: 3, namespace: @namespace, max_entries: 2, expiry_method: expiry_method, **@single_shard_cluster)
      default_shard_keys = shard_keys(@cache, first_shard_key)

      @cache.write(default_shard_keys[0], 1)
      @cache.write(default_shard_keys[1], 2)

      wait_for_background_tasks(@cache)

      @cache.write(default_shard_keys[2], 3)
      @cache.write(default_shard_keys[3], 4)

      wait_for_background_tasks(@cache)
      perform_enqueued_jobs

      # Three records have been deleted
      assert_equal 1, SolidCache::Record.each_shard.sum { SolidCache::Entry.count }
    end

    test "expires when random number is below threshold (#{expiry_method})" do
      SolidCache::Store.any_instance.stubs(:rand).returns(0.416)

      @cache = ActiveSupport::Cache.lookup_store(:solid_cache_store, expiry_batch_size: 3, namespace: @namespace, max_entries: 1, expiry_method: expiry_method)
      default_shard_keys = shard_keys(@cache, first_shard_key)

      @cache.write(default_shard_keys[0], 1)
      @cache.write(default_shard_keys[1], 2)

      wait_for_background_tasks(@cache)
      perform_enqueued_jobs

      assert_equal 0, SolidCache::Record.each_shard.sum { SolidCache::Entry.count }
    end

    test "doesn't expire when random number is above threshold (#{expiry_method})" do
      SolidCache::Store.any_instance.stubs(:rand).returns(0.417)

      @cache = ActiveSupport::Cache.lookup_store(:solid_cache_store, expiry_batch_size: 6, namespace: @namespace, max_entries: 1, expiry_method: expiry_method)
      default_shard_keys = shard_keys(@cache, first_shard_key)

      @cache.write(default_shard_keys[0], 1)
      @cache.write(default_shard_keys[1], 2)

      wait_for_background_tasks(@cache)
      perform_enqueued_jobs

      assert_equal 2, SolidCache::Record.each_shard.sum { SolidCache::Entry.count }
    end

    test "expires old records multiple shards (#{expiry_method})" do
      skip if single_database?

      SolidCache::Store.any_instance.stubs(:rand).returns(0, 1, 0, 1, 0, 1, 0, 1)
      @cache = lookup_store(expiry_batch_size: 2, shards: [ first_shard_key, second_shard_key ], expiry_method: expiry_method)
      default_shard_keys = shard_keys(@cache, first_shard_key)
      shard_one_keys = shard_keys(@cache, second_shard_key)

      @cache.write(default_shard_keys[0], 1)
      @cache.write(default_shard_keys[1], 2)
      @cache.write(shard_one_keys[0], 3)
      @cache.write(shard_one_keys[1], 4)

      assert_equal 1, @cache.read(default_shard_keys[0])
      assert_equal 2, @cache.read(default_shard_keys[1])
      assert_equal 3, @cache.read(shard_one_keys[0])
      assert_equal 4, @cache.read(shard_one_keys[1])

      wait_for_background_tasks(@cache)
      send_entries_back_in_time(3.weeks)

      @cache.write(default_shard_keys[2], 5)
      @cache.write(default_shard_keys[3], 6)
      @cache.write(shard_one_keys[2], 7)
      @cache.write(shard_one_keys[3], 8)

      wait_for_background_tasks(@cache)
      perform_enqueued_jobs

      assert_nil @cache.read(default_shard_keys[0])
      assert_nil @cache.read(default_shard_keys[1])
      assert_nil @cache.read(shard_one_keys[0])
      assert_nil @cache.read(shard_one_keys[1])
      assert_equal 5, @cache.read(default_shard_keys[2])
      assert_equal 6, @cache.read(default_shard_keys[3])
      assert_equal 7, @cache.read(shard_one_keys[2])
      assert_equal 8, @cache.read(shard_one_keys[3])

      [ first_shard_key, :primary_shard_one ].each do |shard|
        SolidCache::Record.connected_to(shard: shard) do
          assert_equal 2, SolidCache::Entry.count
        end
      end
    end
  end

  test "expires old records with a custom queue" do
    SolidCache::Store.any_instance.stubs(:rand).returns(0, 1, 0, 1)

    @cache = lookup_store(expiry_batch_size: 3, max_entries: 2, expiry_method: :job, expiry_queue: :cache_expiry)

    default_shard_keys = shard_keys(@cache, first_shard_key)

    assert_enqueued_jobs(2, only: SolidCache::ExpiryJob, queue: :cache_expiry) do
      @cache.write(default_shard_keys[0], 1)
      @cache.write(default_shard_keys[1], 2)
      @cache.write(default_shard_keys[2], 3)
      @cache.write(default_shard_keys[2], 4)
    end

    perform_enqueued_jobs
    assert_equal 0, SolidCache::Record.each_shard.sum { SolidCache::Entry.count }
  end

  test "triggers multiple expiry tasks when there are many writes" do
    @cache = lookup_store(expiry_batch_size: 20, max_entries: 2, expiry_queue: :cache_expiry, **@single_shard_cluster)
    background = @cache.instance_variable_get("@background")

    SolidCache::Store.any_instance.stubs(:rand).returns(0.25, 0.24)
    # We expect 2 expiry job for 8 writes
    assert_difference -> { background.scheduled_task_count }, +1 do
      @cache.write_multi(8.times.index_by { |i| "key#{i}" })
      wait_for_background_tasks(@cache)
    end

    assert_difference -> { background.scheduled_task_count }, +3 do
      @cache.write_multi(24.times.index_by { |i| "key#{i}" })
      wait_for_background_tasks(@cache)
    end

    # Whether we overflow an extra job depends on rand
    SolidCache::Store.any_instance.stubs(:rand).returns(0.25, 0.24)
    assert_difference -> { background.scheduled_task_count }, +1 do
      @cache.write_multi(10.times.index_by { |i| "key#{i}" })
      wait_for_background_tasks(@cache)
    end

    assert_difference -> { background.scheduled_task_count }, +1 do
      @cache.write_multi(10.times.index_by { |i| "key#{i}" })
      wait_for_background_tasks(@cache)
    end
  end

  test "triggers multiple expiry jobs when there are many writes" do
    @cache = lookup_store(expiry_batch_size: 10, max_entries: 4, expiry_queue: :cache_expiry, expiry_method: :job, **@single_shard_cluster)

    SolidCache::Store.any_instance.stubs(:rand).returns(0.25, 0.24)
    # We expect 1 expiry job for 8 writes
    assert_enqueued_jobs(2, only: SolidCache::ExpiryJob) do
      @cache.write_multi(8.times.index_by { |i| "key#{i}" })
    end

    assert_enqueued_jobs(5, only: SolidCache::ExpiryJob) do
      @cache.write_multi(24.times.index_by { |i| "key#{i}" })
    end

    # Whether we overflow an extra job depends on rand
    SolidCache::Store.any_instance.stubs(:rand).returns(0.125, 0.124)
    assert_enqueued_jobs(2, only: SolidCache::ExpiryJob) do
      @cache.write_multi(10.times.index_by { |i| "key#{i}" })
    end
  end

  test "size estimate tracks write deltas per shard and drives max_size expiry" do
    skip if single_database?

    SolidCache::Store.any_instance.stubs(:rand).returns(0)

    max_size = 10_000
    @cache = lookup_store(expiry_batch_size: 1, max_age: nil, max_size: max_size, expiry_method: :job, shards: [ first_shard_key, second_shard_key ])
    shard_one_keys = shard_keys(@cache, first_shard_key)
    shard_two_keys = shard_keys(@cache, second_shard_key)

    # Place the same number of small values on both shards, two expiry triggers per write
    assert_enqueued_jobs(20, only: SolidCache::ExpiryJob) do
      shard_one_keys.first(5).each { |key| @cache.write(key, "a" * 10) }
      shard_two_keys.first(5).each { |key| @cache.write(key, "a" * 10) }
    end
    perform_enqueued_jobs

    # Both shards start with an exact, independent estimate below the threshold
    shard_two_estimate = estimate_on(second_shard_key)
    assert_equal actual_byte_size_on(first_shard_key), estimate_on(first_shard_key)
    assert_equal actual_byte_size_on(second_shard_key), shard_two_estimate
    assert_operator estimate_on(first_shard_key), :<, max_size

    # Overwrite an old key on shard one with a large (incompressible) value: the
    # estimate crosses the max_size threshold right away and expiry kicks in.
    # Use the most recently written key so the large entry is not an expiry candidate.
    large_value = SecureRandom.random_bytes(20_000)
    assert_enqueued_jobs(2, only: SolidCache::ExpiryJob) do
      @cache.write(shard_one_keys[4], large_value)
    end
    assert_operator estimate_on(first_shard_key), :>, max_size
    perform_enqueued_jobs

    # Five entries were written and two expiry batches deleted the two oldest
    assert_equal 3, entry_count_on(first_shard_key)
    assert_equal 5, entry_count_on(second_shard_key)

    # A write on shard two that fails as part of a transaction leaves its estimate untouched
    assert_enqueued_jobs(2, only: SolidCache::ExpiryJob) do
      SolidCache::Record.with_shard(second_shard_key) do
        SolidCache::Entry.transaction do
          @cache.write(shard_two_keys[0], "b" * 5_000)
          raise ActiveRecord::Rollback
        end
      end
    end
    perform_enqueued_jobs

    assert_equal shard_two_estimate, estimate_on(second_shard_key)
    assert_equal "a" * 10, @cache.read(shard_two_keys[0])

    # Next round: delete the large value, the estimate drops by exactly its byte size
    large_key = @cache.send(:normalize_key, shard_one_keys[4], {})
    large_byte_size = SolidCache::Record.with_shard(first_shard_key) do
      SolidCache::Entry.uncached do
        SolidCache::Entry.where(key_hash: SolidCache::Entry.send(:key_hash_for, large_key)).pick(:byte_size)
      end
    end

    before_delete = estimate_on(first_shard_key)
    @cache.delete(shard_one_keys[4])
    assert_equal before_delete - large_byte_size, estimate_on(first_shard_key)

    # Reconcile actual usage, estimates and expiry triggers on both shards
    assert_equal actual_byte_size_on(first_shard_key), estimate_on(first_shard_key)
    assert_equal actual_byte_size_on(second_shard_key), estimate_on(second_shard_key)
    assert_equal 24, performed_jobs.count { |job| job[:job] == SolidCache::ExpiryJob }
  end

  private
    def estimate_on(shard)
      SolidCache::Record.with_shard(shard) { SolidCache::Entry.estimated_size }
    end

    def actual_byte_size_on(shard)
      SolidCache::Record.with_shard(shard) { SolidCache::Entry.uncached { SolidCache::Entry.sum(:byte_size) } }
    end

    def entry_count_on(shard)
      SolidCache::Record.with_shard(shard) { SolidCache::Entry.uncached { SolidCache::Entry.count } }
    end
end
