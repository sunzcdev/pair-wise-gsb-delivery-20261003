# frozen_string_literal: true

require "test_helper"

module SolidCache
  class EntryTest < ActiveSupport::TestCase
    test "write and read cache entries" do
      Entry.write("hello".b, "there")
      assert_equal "there", Entry.read("hello".b)
    end

    test "write multi and read multi cache entries" do
      Entry.write_multi([ { key: "hello".b, value: "there" }, { key: "foo".b, value: "bar" } ])
      assert_equal({ "foo" => "bar", "hello" => "there" }, Entry.read_multi([ "hello".b, "foo".b, "bar".b ]))
    end

    test "id range" do
      assert_equal 0, Entry.id_range

      Entry.write("hello".b, "there")
      Entry.write("hello2".b, "there")

      assert_equal 2, Entry.uncached { Entry.id_range }
    end

    test "clear with truncate" do
      write_entries
      assert_equal 20, uncached_entry_count
      Entry.clear_truncate
      assert_equal 0, uncached_entry_count
    end

    test "clear with delete" do
      write_entries
      assert_equal 20, uncached_entry_count
      Entry.clear_delete
      assert_equal 0, uncached_entry_count
    end

    test "handles key_hash collisions" do
      Entry.stubs(:key_hash_for).with("hello".b).returns(1)
      Entry.stubs(:key_hash_for).with("hi".b).returns(1)

      Entry.write "hello".b, "there"
      assert_equal "there", Entry.read("hello".b)
      assert_nil Entry.read("hi".b)
      assert_equal({ "hello" => "there" }, Entry.read_multi([ "hello", "hi" ]))

      Entry.write "hi".b, "now"
      assert_nil Entry.read("hello".b)
      assert_equal "now", Entry.read("hi".b)
      assert_equal({ "hi" => "now" }, Entry.read_multi([ "hello", "hi" ]))
    end

    test "key_hash_collisions reports identity clashes but not re-writes" do
      Entry.stubs(:key_hash_for).with("hello").returns(1)
      Entry.stubs(:key_hash_for).with("hi").returns(1)
      Entry.stubs(:key_hash_for).with("other").returns(2)

      assert_equal [], Entry.key_hash_collisions([ { key: "hello", value: "there" } ])

      Entry.write "hello", "there"

      re_write = Entry.key_hash_collisions([ { key: "hello", value: "there" } ])
      assert_equal [], re_write

      collisions = Entry.key_hash_collisions([
        { key: "hi", value: "now" }, { key: "other", value: "thing" }
      ])
      assert_equal [ { key_hash: 1, key: "hi", existing_key: "hello" } ], collisions
    end

    test "write_multi reports collisions" do
      Entry.stubs(:key_hash_for).with("hello").returns(1)
      Entry.stubs(:key_hash_for).with("hi").returns(1)

      assert_equal [], Entry.write_multi([ { key: "hello", value: "there" } ])
      collisions = Entry.write_multi([ { key: "hi", value: "now" } ])
      assert_equal [ { key_hash: 1, key: "hi", existing_key: "hello" } ], collisions
    end

    test "namespace_stats groups entries by digest without exposing keys or values" do
      Entry.write_multi([
        { key: "accounts:1".b, value: "secret-one" },
        { key: "accounts:2".b, value: "secret-two" },
        { key: "billing:1".b, value: "secret-three" }
      ])

      stats = Entry.namespace_stats

      accounts_group = Entry.namespace_group_id("accounts")
      billing_group = Entry.namespace_group_id("billing")

      assert_equal 2, stats[accounts_group][:entries]
      assert_equal 1, stats[billing_group][:entries]
      assert stats[accounts_group][:byte_size] > 0

      # The raw namespaces, keys and values must never appear in the report
      rendered = stats.inspect
      assert_not_includes rendered, "accounts"
      assert_not_includes rendered, "billing"
      assert_not_includes rendered, "secret"
    end

    test "namespace_stats keeps entries without a namespace in their own group" do
      Entry.write_multi([
        { key: "top-level".b, value: "one" },
        { key: "accounts:1".b, value: "two" }
      ])

      stats = Entry.namespace_stats
      top_level_group = Entry.namespace_group_id("")

      assert_equal 1, stats[top_level_group][:entries]
      assert_equal 2, stats.count
    end

    test "namespace_for and namespace_group_id are stable" do
      assert_equal "accounts", Entry.namespace_for("accounts:1:deep".b)
      assert_equal "", Entry.namespace_for("top-level".b)
      assert_equal ActiveSupport::Digest.hexdigest("accounts"), Entry.namespace_group_id("accounts")
    end

    test "byte_size" do
      if SolidCache.configuration.encrypt?
        Entry.write "hello".b, "test"
        assert_equal 319, Entry.uncached { Entry.last.byte_size }
        Entry.write "hello".b, "12345"
        assert_equal 320, Entry.uncached { Entry.last.byte_size }
        Entry.write "hi".b, "12345"
        assert_equal 317, Entry.uncached { Entry.last.byte_size }
      else
        Entry.write "hello".b, "test"
        assert_equal 149, Entry.uncached { Entry.last.byte_size }
        Entry.write "hello".b, "12345"
        assert_equal 150, Entry.uncached { Entry.last.byte_size }
        Entry.write "hi".b, "12345"
        assert_equal 147, Entry.uncached { Entry.last.byte_size }
      end
    end

    test "batching multi queries" do
      stub_const(Entry, :MULTI_BATCH_SIZE, 2) do
        # Two upserts for the two batches plus the key hash collision checks before each
        assert_queries_count(4) do
          Entry.write_multi([ { key: "hello".b, value: "there" }, { key: "foo".b, value: "bar" }, { key: "baz".b, value: "zab" } ])
        end

        assert_queries_count(2) do
          assert_equal({ "foo" => "bar", "hello" => "there", "baz" => "zab" }, Entry.read_multi([ "hello".b, "foo".b, "baz".b, "bar".b ]))
        end
      end
    end

    private
      def write_entries(count = 20)
        Entry.write_multi(count.times.map { |i| { key: "key#{i}", value: "value#{i}" } })
      end
  end
end
