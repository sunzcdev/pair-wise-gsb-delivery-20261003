# frozen_string_literal: true

require "test_helper"

class AdjustMultiTest < ActiveSupport::TestCase
  setup do
    @namespace = "test-#{SecureRandom.hex}"
    @cache = lookup_store(expiry_batch_size: 1_000)
  end

  test "applies increments and decrements to several counters" do
    @cache.write("views", 10, raw: true)
    @cache.write("credits", 100, raw: true)
    @cache.write("stock", 5, raw: true)

    result = @cache.adjust_multi({ "views" => 3, "credits" => -40, "stock" => 1 })

    assert result.complete?
    assert_equal({ "views" => 13, "credits" => 60, "stock" => 6 }, result.values)
    assert_equal 13, @cache.read("views", raw: true).to_i
    assert_equal 60, @cache.read("credits", raw: true).to_i
    assert_equal 6, @cache.read("stock", raw: true).to_i
  end

  test "creates missing counters from zero" do
    result = @cache.adjust_multi({ "fresh" => 4, "negative" => -2 })

    assert result.complete?
    assert_equal({ "fresh" => 4, "negative" => -2 }, result.values)
    assert_equal 4, @cache.read("fresh", raw: true).to_i
    assert_equal(-2, @cache.read("negative", raw: true).to_i)
  end

  test "an empty batch is complete and changes nothing" do
    result = @cache.adjust_multi({})

    assert result.complete?
    assert_empty result
    assert_empty result.values
  end

  test "reports each counter by name and status" do
    result = @cache.adjust_multi({ "a" => 1, "b" => 2 })

    assert_equal [ "a", "b" ], result.keys
    assert_equal SolidCache::Store::Adjustment::APPLIED, result["a"].status
    assert_equal SolidCache::Store::Adjustment::APPLIED, result["b"].status
    assert_equal [ :applied, :applied ], result.map(&:status)
    assert result.to_h.values.all? { |outcome| outcome[:value].present? }
  end

  test "rejects non integer amounts before touching any counter" do
    assert_raises(ArgumentError) { @cache.adjust_multi({ "a" => 1, "b" => "2" }) }
    assert_nil @cache.read("a", raw: true)
  end

  test "names that normalize to one key have their amounts combined" do
    # 1 and "1" expand to the same cache key
    result = @cache.adjust_multi({ 1 => 2, "1" => 3 })

    assert_equal 1, result.keys.size
    assert_equal 5, result.first.value
    assert_equal 5, @cache.read(1, raw: true).to_i
    assert_equal 5, @cache.read("1", raw: true).to_i
  end

  test "re-sending the same token does not apply the adjustment twice" do
    @cache.write("counter", 0, raw: true)

    first = @cache.adjust_multi({ "counter" => 5 }, token: "order-1")
    second = @cache.adjust_multi({ "counter" => 5 }, token: "order-1")

    assert first.complete?
    assert_equal SolidCache::Store::Adjustment::APPLIED, first["counter"].status
    assert second.complete?
    assert_equal SolidCache::Store::Adjustment::ALREADY_APPLIED, second["counter"].status
    assert_equal [ "counter" ], second.already_applied
    assert_equal 5, @cache.read("counter", raw: true).to_i
  end

  test "a different token still applies to the same counter" do
    @cache.write("counter", 0, raw: true)

    @cache.adjust_multi({ "counter" => 1 }, token: "batch-1")
    @cache.adjust_multi({ "counter" => 1 }, token: "batch-2")

    assert_equal 2, @cache.read("counter", raw: true).to_i
  end

  test "a receipt survives a resend with an explicit expiry" do
    @cache.write("counter", 0, raw: true)

    @cache.adjust_multi({ "counter" => 1 }, token: "ttl", expires_in: 60)
    resend = @cache.adjust_multi({ "counter" => 1 }, token: "ttl", expires_in: 60)

    assert_equal SolidCache::Store::Adjustment::ALREADY_APPLIED, resend["counter"].status
    assert_equal 1, @cache.read("counter", raw: true).to_i
  end

  test "concurrent batches on one counter both count" do
    @cache.write("counter", 10, raw: true)

    threads = 2.times.map do |i|
      Thread.new do
        ActiveRecord::Base.connection_pool.with_connection do
          @cache.adjust_multi({ "counter" => 1 }, token: "parallel-#{i}")
        end
      end
    end
    threads.each(&:join)

    assert_equal 12, @cache.read("counter", raw: true).to_i
  end

  test "a resend of an already applied batch does not undo a later increment" do
    @cache.write("counter", 0, raw: true)

    @cache.adjust_multi({ "counter" => 1 }, token: "shared")
    @cache.adjust_multi({ "counter" => 1 }, token: "other")
    resend = @cache.adjust_multi({ "counter" => 1 }, token: "shared")

    assert_equal SolidCache::Store::Adjustment::ALREADY_APPLIED, resend["counter"].status
    assert_equal 2, resend["counter"].value
    assert_equal 2, @cache.read("counter", raw: true).to_i
  end

  test "keeps an enclosing local cache in step" do
    @cache.write("counter", 1, raw: true)

    @cache.with_local_cache do
      result = @cache.adjust_multi({ "counter" => 4 })

      assert_equal 5, result["counter"].value
      assert_equal 5, @cache.read("counter", raw: true)
    end
  end

  test "instrumentation reports the adjusted keys and their statuses" do
    events = capture_notifications("cache_adjust_multi.active_support") do
      @cache.adjust_multi({ "a" => 1, "b" => 2 }, token: "instrumented")
    end

    assert_equal %w[ cache_adjust_multi.active_support ], events.map(&:name)
    assert_equal [ "a", "b" ], events[0].payload[:key]
    assert_equal({ "a" => :applied, "b" => :applied }, events[0].payload[:statuses])
    assert_equal @cache.class.name, events[0].payload[:store]
  end

  test "an unavailable connection is reported per counter and healthy ones still apply" do
    keys = two_keys_on_different_shards
    skip "needs two shards" unless keys

    healthy_name, healthy_shard, failing_name, failing_shard = keys
    @cache.write(healthy_name, 10, raw: true)
    @cache.write(failing_name, 20, raw: true)

    result = with_shard_unavailable(failing_shard) do
      @cache.adjust_multi({ healthy_name => 3, failing_name => 7 }, token: "partial")
    end

    assert_not result.complete?
    assert_equal SolidCache::Store::Adjustment::APPLIED, result[healthy_name].status
    assert_equal SolidCache::Store::Adjustment::UNAVAILABLE, result[failing_name].status
    assert_equal [ failing_name ], result.unavailable
    assert_equal [ failing_shard ], result.unavailable_shards
    assert_equal 13, @cache.read(healthy_name, raw: true).to_i
    assert_equal 20, @cache.read(failing_name, raw: true).to_i
  end

  test "retry applies only the counter that could not be confirmed" do
    keys = two_keys_on_different_shards
    skip "needs two shards" unless keys

    healthy_name, healthy_shard, failing_name, failing_shard = keys
    @cache.write(healthy_name, 0, raw: true)
    @cache.write(failing_name, 5, raw: true)

    result = with_shard_unavailable(failing_shard) do
      @cache.adjust_multi({ healthy_name => 3, failing_name => 7 }, token: "retry-batch")
    end

    retried = result.retry

    assert retried.complete?
    assert_equal [ failing_name ], retried.keys
    assert_equal 3, @cache.read(healthy_name, raw: true).to_i
    assert_equal 12, @cache.read(failing_name, raw: true).to_i
  end

  test "replaying the original request after a retry changes the retried counter only once" do
    keys = two_keys_on_different_shards
    skip "needs two shards" unless keys

    healthy_name, healthy_shard, failing_name, failing_shard = keys
    @cache.write(healthy_name, 0, raw: true)
    @cache.write(failing_name, 0, raw: true)

    original = with_shard_unavailable(failing_shard) do
      @cache.adjust_multi({ healthy_name => 4, failing_name => 4 }, token: "replay")
    end
    assert_equal [ failing_name ], original.unavailable

    original.retry
    replay = @cache.adjust_multi({ healthy_name => 4, failing_name => 4 }, token: "replay")

    assert replay.complete?
    assert_equal [ healthy_name, failing_name ], replay.already_applied
    assert_equal 4, @cache.read(healthy_name, raw: true).to_i
    assert_equal 4, @cache.read(failing_name, raw: true).to_i
  end

  test "retry on a complete result does not repeat any adjustment" do
    @cache.write("counter", 1, raw: true)
    result = @cache.adjust_multi({ "counter" => 2 }, token: "done")

    assert_same result, result.retry
    assert_equal 3, @cache.read("counter", raw: true).to_i
  end

  private
    def two_keys_on_different_shards
      return nil if single_database?

      names = 80.times.map { |index| "key#{index}-#{SecureRandom.hex(3)}" }
      grouped = names.group_by do |name|
        @cache.send(:connections).assign([ @cache.send(:normalize_key, name, {}) ]).keys.first
      end
      return nil if grouped.size < 2

      (first_shard, first_names), (second_shard, second_names) = grouped.first(2)
      [ first_names.first, first_shard, second_names.first, second_shard ]
    end

    def with_shard_unavailable(shard, attempts: 1)
      original = SolidCache::Record.method(:with_shard)
      remaining = attempts

      SolidCache::Record.define_singleton_method(:with_shard) do |target, &block|
        if target == shard && remaining.positive?
          remaining -= 1
          raise ActiveRecord::StatementTimeout, "synthetic outage on #{shard}"
        end

        original.call(target, &block)
      end

      yield
    ensure
      SolidCache::Record.define_singleton_method(:with_shard, original) if original
    end
end