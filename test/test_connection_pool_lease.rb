require_relative "helper"

class TestConnectionPoolLease < Minitest::Test
  def teardown
    ConnectionPool.reset_instances
  end

  def test_lease_returns_a_handle_to_a_checked_out_connection
    pool = ConnectionPool.new(size: 1) { Object.new }

    lease = pool.lease

    assert_kind_of ConnectionPool::Lease, lease
    assert lease.active?
    refute lease.released?
    refute lease.returned?
    refute lease.invalidated?
    refute lease.transferred?
    assert_nil lease.cleanup_exception
    assert_equal 0, pool.available
  end

  def test_release_returns_the_connection_once
    pool = ConnectionPool.new(size: 1) { Object.new }
    lease = pool.lease
    conn = lease.connection

    assert_equal true, lease.release
    assert_equal 1, pool.available

    assert lease.released?
    assert lease.returned?
    refute lease.active?

    assert_same conn, pool.lease.connection
  end

  def test_release_can_happen_from_another_thread
    pool = ConnectionPool.new(size: 1, timeout: 0) { Object.new }
    lease = pool.lease

    Thread.new { lease.release }.join

    assert lease.returned?
    # The releasing thread never touched the pool's thread-local state.
    assert_equal 1, pool.available
    assert_raises(ConnectionPool::Error) { pool.checkin }
  end

  def test_duplicate_release_settles_only_once
    pool = ConnectionPool.new(size: 1) { Object.new }
    lease = pool.lease

    assert_equal true, lease.release
    pushes = 0
    stack = pool.instance_variable_get(:@available)
    stack.define_singleton_method(:push) do |conn, **kwargs|
      pushes += 1
      super(conn, **kwargs)
    end

    assert_equal false, lease.release
    assert_equal false, lease.release
    assert_equal 0, pushes
    assert lease.returned?
    assert_equal 1, pool.available
  end

  def test_release_after_shutdown_is_cleaned_up_by_shutdown_block
    pool = ConnectionPool.new(size: 1) { Object.new }
    lease = pool.lease
    conn = lease.connection
    closed = []

    pool.shutdown { |c| closed << c }

    # A late release after shutdown must not resurrect or lose the connection.
    assert_equal true, lease.release
    assert_equal [conn], closed
    assert lease.returned?
    assert_equal 0, pool.idle
  end

  def test_duplicate_release_after_shutdown_runs_cleanup_once
    pool = ConnectionPool.new(size: 1) { Object.new }
    lease = pool.lease
    closed = []

    pool.shutdown { |c| closed << c }

    assert_equal true, lease.release
    assert_equal false, lease.release
    assert_equal 1, closed.size
  end

  def test_lease_timeout
    pool = ConnectionPool.new(size: 1, timeout: 0) { Object.new }
    pool.lease

    assert_raises(ConnectionPool::TimeoutError) { pool.lease }
  end

  def test_lease_does_not_disturb_thread_local_checkout
    pool = ConnectionPool.new(size: 2, timeout: 0) { Object.new }

    pool.with do |conn|
      lease = pool.lease
      refute_same conn, lease.connection
      lease.release

      # The with block's nested counting is unaffected by the lease lifecycle.
      assert_equal 1, Thread.current[pool.instance_variable_get(:@key_count)]
    end

    assert_equal 2, pool.available
  end

  def test_nested_with_nesting_count_is_preserved
    pool = ConnectionPool.new(size: 1, timeout: 0) { Object.new }

    pool.with do |outer|
      pool.with do |inner|
        assert_same outer, inner
      end
      # Inner checkin only decremented the nesting count; still checked out.
      assert_equal 0, pool.available
    end

    assert_equal 1, pool.available
  end

  def test_transfer_returns_a_new_handle_for_the_same_connection
    pool = ConnectionPool.new(size: 1) { Object.new }
    lease = pool.lease
    conn = lease.connection

    new_lease = lease.transfer

    assert_kind_of ConnectionPool::Lease, new_lease
    refute_same lease, new_lease
    assert_same conn, new_lease.connection
    assert new_lease.active?
    assert lease.transferred?
    refute lease.active?
    assert lease.released?
    assert_equal 0, pool.available
  end

  def test_old_handle_cannot_release_after_transfer
    pool = ConnectionPool.new(size: 1) { Object.new }
    lease = pool.lease
    new_lease = lease.transfer

    pushes = 0
    stack = pool.instance_variable_get(:@available)
    stack.define_singleton_method(:push) do |conn, **kwargs|
      pushes += 1
      super(conn, **kwargs)
    end

    assert_equal false, lease.release
    assert_equal false, lease.invalidate
    assert_equal 0, pushes
    assert_equal 0, pool.available
    assert lease.transferred?

    # Only the new handle can settle the checkout.
    assert_equal true, new_lease.release
    assert new_lease.returned?
    assert_equal 1, pool.available
  end

  def test_transferring_a_settled_lease_raises
    pool = ConnectionPool.new(size: 1) { Object.new }
    lease = pool.lease
    lease.release

    error = assert_raises(ConnectionPool::Error) { lease.transfer }
    assert_match(/transferred|returned/, error.message)
  end

  def test_invalidate_removes_the_connection_from_the_pool
    pool = ConnectionPool.new(size: 1) { Object.new }
    lease = pool.lease

    assert_equal true, lease.invalidate
    assert lease.invalidated?
    refute lease.active?
    assert_nil lease.cleanup_exception
    assert_equal 1, pool.size
    assert_equal 0, pool.idle
    assert_equal 1, pool.available

    # A fresh connection is created to replace the invalidated one.
    pool.with { |c| refute_nil c }
    assert_equal 1, pool.available
  end

  def test_invalidate_runs_cleanup_block_with_the_connection
    pool = ConnectionPool.new(size: 1) { Object.new }
    lease = pool.lease
    conn = lease.connection
    cleaned = []

    lease.invalidate { |c| cleaned << c }

    assert_equal [conn], cleaned
    assert lease.invalidated?
  end

  def test_invalidate_reports_cleanup_exception_separately
    pool = ConnectionPool.new(size: 1) { Object.new }
    lease = pool.lease
    boom = RuntimeError.new("boom")

    result = lease.invalidate { raise boom }

    assert_equal true, result
    assert lease.invalidated?
    assert_same boom, lease.cleanup_exception
    # The resource was still removed from the pool despite the clean-up error.
    assert_equal 1, pool.available
    assert_equal 0, pool.idle
  end

  def test_duplicate_invalidate_settles_only_once
    pool = ConnectionPool.new(size: 1) { Object.new }
    lease = pool.lease
    cleanups = 0

    assert_equal true, lease.invalidate { cleanups += 1 }
    assert_equal false, lease.invalidate { cleanups += 1 }
    assert_equal false, lease.invalidate
    assert_equal 1, cleanups
    assert lease.invalidated?
  end

  def test_invalidate_after_shutdown_still_reports_cleanup_errors
    pool = ConnectionPool.new(size: 1) { Object.new }
    lease = pool.lease
    pool.shutdown {}
    boom = RuntimeError.new("close failed")

    lease.invalidate { raise boom }

    assert lease.invalidated?
    assert_same boom, lease.cleanup_exception
  end

  def test_release_and_invalidate_are_mutually_exclusive
    pool = ConnectionPool.new(size: 1) { Object.new }
    lease = pool.lease
    lease.invalidate

    assert_equal false, lease.release
    assert lease.invalidated?
    refute lease.returned?
  end

  def test_concurrent_release_settles_exactly_once
    pool = ConnectionPool.new(size: 1) { Object.new }
    lease = pool.lease
    stack = pool.instance_variable_get(:@available)
    pushes = 0
    stack.define_singleton_method(:push) do |conn, **kwargs|
      pushes += 1
      super(conn, **kwargs)
    end

    results = Array.new(8) {
      Thread.new { lease.release }
    }.map(&:value)

    # Exactly one caller settles the lease; which thread wins is nondeterministic.
    assert_equal 1, results.count(true)
    assert_equal 7, results.count(false)
    assert_equal 1, pushes
    assert_equal 1, pool.available
  end

  def test_handover_between_worker_threads
    pool = ConnectionPool.new(size: 1, timeout: 0) { Object.new }
    lease = pool.lease
    received = nil

    worker = Thread.new do
      # The worker owns nothing in its own thread locals; it only uses the handle.
      refute Thread.current[pool.instance_variable_get(:@key)]
      received = lease.connection
      lease.release
    end
    worker.join

    assert_same lease.connection, received
    assert lease.returned?
    assert_equal 1, pool.available
  end

  def test_lease_passes_checkout_options_through
    pool = ConnectionPool.new(size: 1) { Object.new }
    stack = pool.instance_variable_get(:@available)
    stored_options = []
    stack.define_singleton_method(:store_connection) do |connection, **options|
      stored_options << options
      super(connection, **options)
    end

    lease = pool.lease(role: :replica)
    lease.release

    assert_equal [{role: :replica}], stored_options
  end
end
