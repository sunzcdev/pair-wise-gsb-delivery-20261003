# frozen_string_literal: true

require_relative "test_helper"
require "httpx/resolver/cache/file"

class ResolverCacheFileTest < Minitest::Test
  include HTTPX

  include ResolverCacheHelpers

  def test_file_cache_merges_writes_from_concurrent_readers
    now = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    fake_now = now

    reader1 = Resolver::Cache::File.new(@file.path)
    reader2 = Resolver::Cache::File.new(@file.path)

    Utils.stub(:now, -> { fake_now }) do
      # both readers load their in-memory view of the (still empty) cache file
      assert_nil reader1.get("reader1.com")
      assert_nil reader2.get("reader2.com")

      # reader1 updates the cache file
      reader1.set("reader1.com", Socket::AF_INET, [{ "data" => "127.0.0.1", "TTL" => now + 20, "name" => "reader1.com" }])

      fake_now += 1

      # reader2 still holds its earlier in-memory view, yet it must only merge
      # the keys it changed itself, and not clobber reader1's entry
      reader2.set("reader2.com", Socket::AF_INET, [{ "data" => "127.0.0.2", "TTL" => now + 20, "name" => "reader2.com" }])
    end

    # reopen the cache and verify both entries are there
    cache = Resolver::Cache::File.new(@file.path)
    Utils.stub(:now, -> { fake_now }) do
      assert_ips ["127.0.0.1"], cache.get("reader1.com")
      assert_ips ["127.0.0.2"], cache.get("reader2.com")
    end
  end

  def test_file_cache_save_drops_expired_entries
    now = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    fake_now = now

    cache = Resolver::Cache::File.new(@file.path)

    Utils.stub(:now, -> { fake_now }) do
      cache.set("expired.com", Socket::AF_INET, [{ "data" => "127.0.0.1", "TTL" => now + 2, "name" => "expired.com" }])
      cache.set("fresh.com", Socket::AF_INET, [{ "data" => "127.0.0.2", "TTL" => now + 200, "name" => "fresh.com" }])

      fake_now += 10

      # the expired entry is evicted, and the removal is merged back into the file
      assert_nil cache.get("expired.com")
      assert_ips ["127.0.0.2"], cache.get("fresh.com")
    end

    cache = Resolver::Cache::File.new(@file.path)
    Utils.stub(:now, -> { fake_now }) do
      assert_nil cache.get("expired.com")
      assert_ips ["127.0.0.2"], cache.get("fresh.com")
    end
  end

  def test_file_cache_detects_corrupted_file
    @file.write("this is not a pstore")
    @file.flush

    now = Process.clock_gettime(Process::CLOCK_MONOTONIC)

    cache = Resolver::Cache::File.new(@file.path)

    # a corrupted file is detected and treated as empty
    assert_nil cache.get("test.com")

    # and it is reset on save
    cache.set("test.com", Socket::AF_INET, [{ "data" => "127.0.0.1", "TTL" => now + 20, "name" => "test.com" }])

    cache = Resolver::Cache::File.new(@file.path)
    assert_ips ["127.0.0.1"], cache.get("test.com")
  end

  private

  def setup
    super
    @file = Tempfile.new("httpx-resolver-cache-test")
  end

  def teardown
    @file.close
    @file.unlink
    super
  end

  def cache
    @cache ||= Resolver::Cache::File.new(@file).tap do |cache|
      def cache.lookups
        @store.transaction { |st| st[:lookups] || {} }
      end

      def cache.hostnames
        @store.transaction { |st| st[:hostnames] || [] }
      end
    end
  end
end
