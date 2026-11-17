require_relative "helper"

class TestConnectionPoolPartitioned < Minitest::Test
  def teardown
    # wipe the `:INSTANCES` const to avoid cross test contamination
    ConnectionPool.reset_instances
  end

  class TaggedConnection
    attr_reader :tag

    def initialize(tag)
      @tag = tag
      @closed = false
    end

    def close
      @closed = true
    end

    def closed?
      @closed
    end
  end

  def tagged_pool(size: 5, timeout: 5, normalize_key: nil)
    created = []
    factory = lambda do |key|
      conn = TaggedConnection.new(key)
      created << conn
      conn
    end
    pool = ConnectionPool::Partitioned.new(size: size, timeout: timeout, normalize_key: normalize_key, &factory)
    [pool, created]
  end

  # Checks out +key+ on a background thread and holds it until killed.
  def hold_connection(pool, key)
    held = Queue.new
    thread = Thread.new do
      pool.checkout(key: key)
      held << true
      sleep
    end
    held.pop
    thread
  end

  def kill_threads(threads)
    threads.each(&:kill)
    threads.each(&:join)
  end

  def test_factory_is_tagged_with_partition_key
    pool, created = tagged_pool(size: 1)

    pool.with(key: "redis://host-a/0") do |conn|
      assert_equal "redis://host-a/0", conn.tag
    end

    assert_equal ["redis://host-a/0"], created.map(&:tag)
  end

  def test_same_partition_reuses_idle_connection
    pool, _ = tagged_pool(size: 2)

    first = pool.checkout(key: "a")
    pool.checkin
    second = pool.checkout(key: "a")
    pool.checkin

    assert_same first, second
  end

  def test_different_partition_never_reuses_idle_connection
    pool, created = tagged_pool(size: 2)

    a = pool.checkout(key: "a")
    pool.checkin
    b = pool.checkout(key: "b")
    pool.checkin

    refute_same a, b
    assert_equal "b", b.tag
    assert_equal 2, created.size
    assert_equal 1, pool.idle(key: "a")
    assert_equal 1, pool.idle(key: "b")
  end

  def test_normalize_key_maps_raw_config_to_canonical_partition
    normalize = ->(cfg) { [cfg[:host], cfg[:port]] }
    pool, created = tagged_pool(size: 2, normalize_key: normalize)

    first = pool.checkout(key: {host: "h1", port: 6379, password: "secret-a"})
    pool.checkin
    second = pool.checkout(key: {host: "h1", port: 6379, password: "rotated-b"})
    pool.checkin
    other = pool.checkout(key: {host: "h2", port: 6379})
    pool.checkin

    assert_same first, second
    refute_same first, other
    assert_equal [["h1", 6379], ["h2", 6379]], created.map(&:tag)
  end

  def test_partitions_share_pool_wide_capacity
    pool, _ = tagged_pool(size: 2, timeout: 0)
    threads = [hold_connection(pool, "a"), hold_connection(pool, "b")]

    # every partition, known or unknown, is bounded by the same ledger
    assert_raises ConnectionPool::TimeoutError do
      pool.checkout(key: "c")
    end
    assert_raises ConnectionPool::TimeoutError do
      pool.checkout(key: "a")
    end
  ensure
    kill_threads(threads)
  end

  def test_concurrent_same_key_creation_shares_capacity_ledger
    mutex = Mutex.new
    created_count = 0
    pool = ConnectionPool::Partitioned.new(size: 2, timeout: 5) do |key|
      mutex.synchronize { created_count += 1 }
      sleep 0.05 # widen the creation race window
      TaggedConnection.new(key)
    end

    threads = Array.new(6) do
      Thread.new do
        3.times { pool.with(key: "same") { |conn| conn.tag } }
      end
    end
    threads.each(&:join)

    count = mutex.synchronize { created_count }
    assert_operator count, :<=, 2
    assert_equal count, pool.idle(key: "same")
  end

  def test_nested_with_same_partition_reuses_connection
    pool, created = tagged_pool(size: 2)

    pool.with(key: "a") do |outer|
      pool.with(key: "a") do |inner|
        assert_same outer, inner
      end
    end

    assert_equal 1, created.size
  end

  def test_nested_with_different_partition_raises_instead_of_reusing
    pool, _ = tagged_pool(size: 2)

    pool.with(key: "redis://:secret-a@host-a/0") do |outer|
      err = assert_raises(ConnectionPool::Error) do
        pool.with(key: "redis://:secret-b@host-b/0") { |_| flunk "should not yield" }
      end
      refute_includes err.message, "secret-a"
      refute_includes err.message, "secret-b"
      refute_includes err.message, "host-a"
      refute_includes err.message, "host-b"
      assert_equal "redis://:secret-a@host-a/0", outer.tag
    end
  end

  def test_timeout_diagnostic_redacts_partition_key
    pool, _ = tagged_pool(size: 1, timeout: 0)
    secret = "redis://:hunter2@db.internal/0"
    threads = [hold_connection(pool, "other")]

    err = assert_raises(ConnectionPool::TimeoutError) do
      pool.checkout(key: secret)
    end

    refute_includes err.message, "hunter2"
    refute_includes err.message, "db.internal"
    assert_includes err.message, "partition-"
  ensure
    kill_threads(threads)
  end

  def test_reap_only_reaps_its_own_partition
    pool, _ = tagged_pool(size: 3)

    a = pool.checkout(key: "a")
    pool.checkin
    b = pool.checkout(key: "b")
    pool.checkin
    sleep 0.01

    reaped = []
    pool.reap(idle_seconds: 0, key: "a") do |conn|
      conn.close
      reaped << conn
    end

    assert_equal [a], reaped
    assert a.closed?
    refute b.closed?
    assert_equal 0, pool.idle(key: "a")
    assert_equal 1, pool.idle(key: "b")
  end

  def test_unknown_partition_precreates_nothing
    pool, created = tagged_pool(size: 2)

    assert_equal 0, pool.idle(key: "never-seen")

    reaped = []
    pool.reap(idle_seconds: 0, key: "never-seen") { |conn| reaped << conn }

    assert_empty reaped
    assert_empty created
    assert_equal 2, pool.available
  end

  def test_checkin_returns_connection_to_owning_partition
    pool, _ = tagged_pool(size: 2)

    a = pool.checkout(key: "a")
    pool.checkin
    b = pool.checkout(key: "b")
    pool.checkin

    assert_same a, pool.checkout(key: "a")
    pool.checkin
    assert_same b, pool.checkout(key: "b")
    pool.checkin
  end

  def test_checkout_requires_a_partition_key
    pool, _ = tagged_pool(size: 1)

    assert_raises(ArgumentError) { pool.checkout }
    assert_raises(ArgumentError) { pool.checkout(key: nil) }
  end

  def test_normalize_key_returning_nil_is_rejected
    pool, _ = tagged_pool(size: 1, normalize_key: ->(_cfg) { nil })

    assert_raises(ArgumentError) { pool.checkout(key: "anything") }
  end

  def test_discard_frees_shared_capacity_for_other_partitions
    pool, _ = tagged_pool(size: 1, timeout: 0)

    pool.with(key: "a") do |conn|
      pool.discard_current_connection(&:close)
    end

    # the discarded connection's ledger slot must be reusable by any partition
    pool.with(key: "b") do |conn|
      assert_equal "b", conn.tag
    end
  end

  def test_shutdown_closes_connections_across_partitions
    pool, created = tagged_pool(size: 3)

    pool.checkout(key: "a")
    pool.checkin
    pool.checkout(key: "b")
    pool.checkin

    pool.shutdown(&:close)

    assert created.all?(&:closed?)
    assert_raises ConnectionPool::PoolShuttingDownError do
      pool.checkout(key: "a")
    end
  end

  def test_reload_recreates_per_partition_on_demand
    pool, created = tagged_pool(size: 2)

    pool.with(key: "a") { |conn| conn }
    pool.reload(&:close)

    assert created.first.closed?
    assert_equal 0, pool.idle

    pool.with(key: "a") do |conn|
      refute_same created.first, conn
      assert_equal "a", conn.tag
    end
  end

  def test_with_returns_block_value
    pool, _ = tagged_pool(size: 1)

    assert_equal 42, pool.with(key: "a") { 42 }
  end

  def test_available_reflects_shared_ledger
    pool, _ = tagged_pool(size: 2)

    assert_equal 2, pool.available
    pool.checkout(key: "a")
    assert_equal 1, pool.available
    pool.checkin
    assert_equal 2, pool.available
    assert_equal 1, pool.idle
  end
end
