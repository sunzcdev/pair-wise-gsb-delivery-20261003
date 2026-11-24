# frozen_string_literal: true

require "test_helper"

class SolidCache::ReadMultiReportTest < ActiveSupport::TestCase
  include ActiveSupport::Testing::TimeHelpers

  setup do
    @namespace = "test-#{SecureRandom.hex}"
    @errors = []
    @cache = lookup_store(expires_in: 60, error_handler: ->(method:, returning:, exception:) { @errors << [ method, exception ] })
  end

  test "reports each key as a hit or a miss when every shard answers" do
    hit_key, missing_key = keys_for(first_shard, 2)

    @cache.write(hit_key, "value")

    report = @cache.read_multi_report(hit_key, missing_key)

    assert_predicate report, :complete?
    assert_equal [ hit_key ], report.hits
    assert_equal [ missing_key ], report.misses
    assert_empty report.unknown
    assert_equal({ hit_key => "value" }, report.results)
    assert_equal({ hit_key => "value" }, report.to_h)
    assert_equal first_shard, report.shard_for(hit_key)
    assert_predicate report.shard(first_shard), :ok?
    assert_empty report.unavailable_shards
  end

  test "a key stored with a nil value is a hit, not a miss" do
    nil_key, missing_key = keys_for(first_shard, 2)

    @cache.write(nil_key, nil)

    report = @cache.read_multi_report(nil_key, missing_key)

    assert_equal :hit, report.status(nil_key)
    assert_nil report.value(nil_key)
    assert_includes report.results.keys, nil_key
    assert_nil report.results[nil_key]

    assert_equal :miss, report.status(missing_key)
    assert_not_includes report.results.keys, missing_key
  end

  test "the report can be consumed like a hash of the hits" do
    nil_key, missing_key = keys_for(first_shard, 2)

    @cache.write(nil_key, nil)

    report = @cache.read_multi_report(nil_key, missing_key)

    assert_equal({ nil_key => nil }, report.to_h)
    assert_equal [ [ nil_key, nil ] ], report.map { |name, value| [ name, value ] }
  end

  test "a failed shard marks its own keys unknown and never as misses" do
    failing_keys = keys_for(first_shard, 2)

    report = nil
    emulating_shard_unavailability(first_shard) do
      report = @cache.read_multi_report(*failing_keys)
    end

    assert_not report.complete?
    assert_equal failing_keys, report.unknown
    assert_empty report.hits
    assert_empty report.misses
    assert_equal({}, report.results)

    assert_predicate report.shard(first_shard), :unavailable?
    assert_kind_of ActiveRecord::StatementTimeout, report.error_for(first_shard)
    assert_equal [ first_shard ], report.unavailable_shards.map(&:name)
    assert_equal failing_keys, report.shard(first_shard).names

    assert_equal [ [ :read_multi_mget, ActiveRecord::StatementTimeout ] ], @errors.map { |method, error| [ method, error.class ] }
  end

  test "a failed shard leaves only its own keys unknown while a healthy shard is still read" do
    skip unless shard_count > 1

    failing_key = keys_for(first_shard, 1).first
    nil_key, missing_key = keys_for(second_shard, 2)

    @cache.write(failing_key, "unreachable")
    @cache.write(nil_key, nil)

    report = nil
    emulating_shard_unavailability(first_shard) do
      report = @cache.read_multi_report(failing_key, nil_key, missing_key)
    end

    assert_not report.complete?
    assert_equal [ failing_key ], report.unknown
    assert_equal [ nil_key ], report.hits
    assert_equal [ missing_key ], report.misses

    assert_nil report.value(nil_key)
    assert_equal({ nil_key => nil }, report.results)
    assert_not_includes report.results.keys, failing_key

    assert_predicate report.shard(first_shard), :unavailable?
    assert_predicate report.shard(second_shard), :ok?

    assert_equal [ [ :read_multi_mget, ActiveRecord::StatementTimeout ] ], @errors.map { |method, error| [ method, error.class ] }
  end

  test "an expired entry is reported as a miss" do
    key = keys_for(first_shard, 1).first
    @cache.write(key, "value", expires_in: 1.second)
    travel 2.seconds

    report = @cache.read_multi_report(key)

    assert_equal :miss, report.status(key)
    assert_nil @cache.read(key)
  end

  test "a version mismatch is reported as a miss" do
    key = keys_for(first_shard, 1).first
    @cache.write(key, "value", version: "one")

    report = @cache.read_multi_report(key, version: "two")

    assert_equal :miss, report.status(key)
  end

  test "an empty request returns an empty report" do
    report = @cache.read_multi_report

    assert_predicate report, :complete?
    assert_empty report.names
    assert_empty report.results
    assert_empty report.shards
  end

  test "a raising error handler still raises instead of reporting unknown" do
    raising_cache = lookup_store(expires_in: 60, error_handler: ->(method:, returning:, exception:) { raise exception })
    keys = keys_for(first_shard, 2)

    assert_raises ActiveRecord::StatementTimeout do
      emulating_shard_unavailability(first_shard) do
        raising_cache.read_multi_report(*keys)
      end
    end
  end

  test "read_multi is unchanged and still returns a plain hash" do
    hit_key, missing_key = keys_for(first_shard, 2)

    @cache.write(hit_key, "value")
    @cache.write(missing_key, nil)

    assert_equal({ hit_key => "value", missing_key => nil }, @cache.read_multi(hit_key, missing_key))

    emulating_shard_unavailability(first_shard) do
      assert_equal({}, @cache.read_multi(hit_key, missing_key))
    end
  end

  private
    def keys_by_shard
      @keys_by_shard ||= begin
        normalized_keys = 200.times.map { |i| @cache.send(:normalize_key, "key#{i}", {}) }
        @cache.send(:connections).assign(normalized_keys)
          .transform_values { |keys| keys.map { |key| key.delete_prefix("#{@namespace}:") } }
      end
    end

    def shard_count
      keys_by_shard.size
    end

    def first_shard
      keys_by_shard.keys.first
    end

    def second_shard
      keys_by_shard.keys.second
    end

    def keys_for(shard, count)
      keys = keys_by_shard.fetch(shard)
      raise "Only #{keys.size} keys for #{shard}, need #{count}" if keys.size < count

      keys.first(count)
    end

    def emulating_shard_unavailability(shard)
      wait_for_background_tasks(@cache)

      unavailable_shard = shard
      original_read_multi = SolidCache::Entry.method(:read_multi)

      SolidCache::Entry.define_singleton_method(:read_multi) do |keys|
        if current_shard == unavailable_shard
          raise ActiveRecord::StatementTimeout, "shard #{unavailable_shard} is unavailable"
        end

        original_read_multi.call(keys)
      end

      yield
    ensure
      SolidCache::Entry.define_singleton_method(:read_multi, original_read_multi)
    end
end
