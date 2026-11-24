# frozen_string_literal: true

require "test_helper"

module SolidCache
  class CounterTest < ActiveSupport::TestCase
    test "read returns 0 for an unknown counter" do
      assert_equal 0, Counter.read("size")
    end

    test "increment and read" do
      Counter.increment("size", amount: 10)
      Counter.increment("size", amount: 5)

      assert_equal 15, Counter.read("size")
    end

    test "increment defaults to one" do
      3.times { Counter.increment("size") }

      assert_equal 3, Counter.read("size")
    end

    test "set overwrites the current value" do
      Counter.increment("size", amount: 10)
      Counter.set("size", amount: 42)

      assert_equal 42, Counter.read("size")
    end

    test "counters with different names are independent" do
      Counter.increment("a", amount: 1)
      Counter.increment("b", amount: 2)

      assert_equal 1, Counter.read("a")
      assert_equal 2, Counter.read("b")
    end

    test "a failed transaction does not change the counter" do
      Counter.increment("size", amount: 10)

      assert_raises(RuntimeError) do
        Counter.transaction do
          Counter.increment("size", amount: 5)
          raise "boom"
        end
      end

      assert_equal 10, Counter.read("size")
    end

    test "each shard keeps its own counter" do
      skip "needs multiple shards" unless SolidCache.configuration.shard_keys.many?

      first_shard, second_shard = SolidCache.configuration.shard_keys.first(2)
      SolidCache::Record.with_shard(first_shard) { Counter.increment("size", amount: 10) }
      SolidCache::Record.with_shard(second_shard) { Counter.increment("size", amount: 20) }

      assert_equal 10, SolidCache::Record.with_shard(first_shard) { Counter.read("size") }
      assert_equal 20, SolidCache::Record.with_shard(second_shard) { Counter.read("size") }
    end
  end
end
