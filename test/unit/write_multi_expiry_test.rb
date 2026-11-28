# frozen_string_literal: true

require "test_helper"

class SolidCache::WriteMultiExpiryTest < ActiveSupport::TestCase
  include ActiveSupport::Testing::TimeHelpers

  setup do
    @namespace = "test-#{SecureRandom.hex}"
  end

  test "batch entries without their own ttl expire from a single shared clock reading" do
    @cache = lookup_store
    travel_to Time.now.change(usec: 0)
    batch_started_at = Time.now

    entries = {
      normalize_key("default-ttl-one") => ActiveSupport::Cache::Entry.new("one"),
      normalize_key("default-ttl-two") => ActiveSupport::Cache::Entry.new("two")
    }
    advance_clock_while_serializing_first_entry(@cache, 15.seconds)

    assert_equal true, @cache.send(:write_multi_entries, entries, expires_in: 60)

    # Both entries expire 60 seconds after the batch started, not 60 seconds
    # after they happened to be serialized.
    assert_equal (batch_started_at + 60).to_f, stored_entry("default-ttl-one").expires_at
    assert_equal (batch_started_at + 60).to_f, stored_entry("default-ttl-two").expires_at
    assert_equal "one", @cache.read("default-ttl-one")
    assert_equal "two", @cache.read("default-ttl-two")
  end

  test "entries with their own shorter ttl keep it, others use the batch default" do
    @cache = lookup_store
    travel_to Time.now.change(usec: 0)
    batch_started_at = Time.now

    entries = {
      normalize_key("default-ttl") => ActiveSupport::Cache::Entry.new("batch default"),
      normalize_key("short-ttl") => ActiveSupport::Cache::Entry.new("shorter", expires_in: 30)
    }
    advance_clock_while_serializing_first_entry(@cache, 15.seconds)

    assert_equal true, @cache.send(:write_multi_entries, entries, expires_in: 60)

    assert_equal (batch_started_at + 60).to_f, stored_entry("default-ttl").expires_at
    assert_equal (batch_started_at + 30).to_f, stored_entry("short-ttl").expires_at
    assert_equal "batch default", @cache.read("default-ttl")
    assert_equal "shorter", @cache.read("short-ttl")
  end

  test "entries that expire before the sql write are skipped and cannot clobber live values" do
    @cache = lookup_store
    travel_to Time.now.change(usec: 0)
    batch_started_at = Time.now

    @cache.write("expiring", "previous", expires_in: 1.hour)

    entries = {
      normalize_key("regular") => ActiveSupport::Cache::Entry.new("regular value"),
      normalize_key("expiring") => ActiveSupport::Cache::Entry.new("late value", expires_in: 10)
    }
    advance_clock_while_serializing_first_entry(@cache, 15.seconds)

    # The expired entry has a defined result: the batch reports that not
    # everything was written.
    assert_equal false, @cache.send(:write_multi_entries, entries, expires_in: 60)

    # The unexpired entry is written, expiring 60 seconds after the batch started.
    assert_equal "regular value", @cache.read("regular")
    assert_equal (batch_started_at + 60).to_f, stored_entry("regular").expires_at

    # The expired entry is not written, so the live value it would have
    # overwritten is still there, with its original expiry.
    assert_equal "previous", @cache.read("expiring")
    assert_equal (batch_started_at + 1.hour).to_f, stored_entry("expiring").expires_at
  end

  test "batch clock is shared across shards, first shard written first" do
    skip if single_database?

    write_batch_across_shards(shortest_shard_first: false)
  end

  test "batch clock is shared across shards, second shard written first" do
    skip if single_database?

    write_batch_across_shards(shortest_shard_first: true)
  end

  test "write_multi with expires_in gives every entry the same expiry" do
    @cache = lookup_store
    travel_to Time.now.change(usec: 0)

    assert @cache.write_multi({ "one" => 1, "two" => 2 }, expires_in: 60)

    assert_equal (Time.now + 60).to_f, stored_entry("one").expires_at
    assert_equal stored_entry("one").expires_at, stored_entry("two").expires_at
    assert_equal 1, @cache.read("one")
    assert_equal 2, @cache.read("two")
  end

  test "write_multi still compresses large values" do
    @cache = lookup_store
    large = "a" * 2048

    assert @cache.write_multi("large" => large, "small" => "s")
    assert_equal large, @cache.read("large")
    assert_equal "s", @cache.read("small")
  end

  private
    def write_batch_across_shards(shortest_shard_first:)
      @cache = lookup_store(shards: [ first_shard_key, second_shard_key ])
      key_one = shard_keys(@cache, first_shard_key).first
      key_two = shard_keys(@cache, second_shard_key).first

      travel_to Time.now.change(usec: 0)
      batch_started_at = Time.now

      # A live value that the late write must not overwrite.
      @cache.write(key_two, "previous", expires_in: 1.hour)

      entries = {
        normalize_key(key_one) => ActiveSupport::Cache::Entry.new("one"),
        normalize_key(key_two) => ActiveSupport::Cache::Entry.new("two", expires_in: 10)
      }
      entries = entries.reverse_each.to_h if shortest_shard_first

      expected_shard_order = shortest_shard_first ? [ second_shard_key, first_shard_key ] : [ first_shard_key, second_shard_key ]
      assert_equal expected_shard_order, @cache.send(:connections).assign(entries.keys).keys

      # Time moves on while the first entry is serialized, so the short-lived
      # entry on the other shard has expired by the time its SQL write runs.
      advance_clock_while_serializing_first_entry(@cache, 15.seconds)

      assert_equal false, @cache.send(:write_multi_entries, entries, expires_in: 60)

      # The regular entry expires 60 seconds after the batch started,
      # regardless of which shard was written first.
      assert_equal "one", @cache.read(key_one)
      assert_equal (batch_started_at + 60).to_f, stored_entry(key_one).expires_at

      # The short-lived entry expired before its shard's write, so the
      # existing live value is untouched.
      assert_equal "previous", @cache.read(key_two)
      assert_equal (batch_started_at + 1.hour).to_f, stored_entry(key_two).expires_at

      travel_to(batch_started_at + 61) do
        assert_nil @cache.read(key_one)
        assert_equal "previous", @cache.read(key_two)
      end
    end

    def normalize_key(name)
      @cache.send(:normalize_key, name, {})
    end

    def stored_entry(name)
      @cache.send(:read_entry, normalize_key(name))
    end

    def advance_clock_while_serializing_first_entry(store, duration)
      test = self
      travelled = false
      original = store.method(:serialize_entry)
      store.define_singleton_method(:serialize_entry) do |entry, **options|
        unless travelled
          travelled = true
          test.travel(duration)
        end
        original.call(entry, **options)
      end
    end
end