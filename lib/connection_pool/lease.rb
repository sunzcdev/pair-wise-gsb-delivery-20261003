##
# A Lease is an explicit, thread-independent handle to a single connection
# checked out from a ConnectionPool.
#
# Unlike +ConnectionPool#checkout+, which tracks ownership in the checking-out
# thread's local variables, a Lease can be passed to another thread (or any
# other owner) and the connection can later be returned using nothing but the
# handle itself. No thread needs to pretend to hold another thread's state.
#
# A Lease is single-use and goes through a small state machine:
#
#   :active       - the connection is checked out and owned by whoever holds
#                   the handle.
#   :returned     - the connection was given back to the pool exactly once.
#   :invalidated  - the connection was removed from the pool, optionally after
#                   running a clean-up block.
#   :transferred  - ownership was handed over to a brand new Lease; this handle
#                   can no longer settle the checkout.
#
# Terminal methods (+release+, +invalidate+) are idempotent: calling them on a
# lease that has already been settled is a no-op returning +false+ rather than
# settling the connection a second time.
class ConnectionPool::Lease
  # The possible lifecycle states of a Lease.
  STATES = [:active, :returned, :invalidated, :transferred].freeze

  # @return [Object] the checked-out connection. The object remains reachable
  #   even after the lease has been settled so callers can inspect what they
  #   handed back.
  attr_reader :connection

  def initialize(pool, connection, kwargs)
    @pool = pool
    @connection = connection
    @kwargs = kwargs
    @state = :active
    @cleanup_exception = nil
    @mutex = Thread::Mutex.new
  end

  ##
  # Returns the leased connection to the pool.
  #
  # Only the first call on an active lease settles the checkout. Repeated
  # releases, or releases of a lease that was invalidated or transferred, are
  # no-ops.
  #
  # If the pool has been shut down, a late release is still cleaned up using the
  # pool's shutdown block instead of being silently lost.
  #
  # @return [Boolean] +true+ if this call settled the checkout, +false+ if the
  #   lease was already settled or is no longer valid.
  def release
    settle do
      @pool.send(:return_leased_connection, @connection, @kwargs)
      @state = :returned
    end
  end

  ##
  # Removes the leased connection from the pool instead of returning it.
  #
  # This is useful when the resource has become invalid or corrupted and must
  # not be reused. The optional block is called with the connection to perform
  # any necessary clean-up.
  #
  # The pool's accounting is updated *before* the clean-up block runs, so a
  # failure inside the block never leaves a leased connection dangling. Any
  # exception raised by the clean-up block is captured and reported separately
  # through {#cleanup_exception} rather than being swallowed or re-raised as a
  # pool failure.
  #
  # Like {#release}, this is idempotent: only the first call on an active lease
  # has an effect.
  #
  # @yieldparam connection [Object] the connection being invalidated
  # @return [Boolean] +true+ if this call invalidated the checkout, +false+ if
  #   the lease was already settled or is no longer valid.
  def invalidate(&block)
    settle do
      # Drop the resource from the pool's bookkeeping first so it can never be
      # handed out again, even if the clean-up below raises.
      @pool.send(:discard_leased_connection, @connection)
      begin
        block&.call(@connection)
      rescue Exception => e # rubocop:disable Lint/SuppressedException
        @cleanup_exception = e
      end
      @state = :invalidated
    end
  end

  ##
  # Hands ownership of the checked-out connection to a new Lease.
  #
  # The returned Lease refers to the exact same connection and becomes its sole
  # valid handle. The receiver is marked +:transferred+ and can never be used
  # to release or invalidate the connection: the previous owner cannot settle a
  # future round of borrowing through the stale handle.
  #
  # @return [ConnectionPool::Lease] the new, active handle for the connection.
  # @raise [ConnectionPool::Error] if the lease is no longer active.
  def transfer
    @mutex.synchronize do
      unless @state == :active
        raise ConnectionPool::Error, "cannot transfer a lease that is #{@state}"
      end

      new_lease = self.class.new(@pool, @connection, @kwargs)
      @state = :transferred
      new_lease
    end
  end

  # @return [Boolean] whether the lease still owns the connection and can be
  #   released, invalidated or transferred.
  def active?
    @mutex.synchronize { @state == :active }
  end

  # @return [Boolean] whether the connection was returned to the pool.
  def returned?
    @mutex.synchronize { @state == :returned }
  end

  # @return [Boolean] whether the connection was removed from the pool.
  def invalidated?
    @mutex.synchronize { @state == :invalidated }
  end

  # @return [Boolean] whether ownership was handed to another Lease, leaving
  #   this handle unable to settle the checkout.
  def transferred?
    @mutex.synchronize { @state == :transferred }
  end

  # @return [Boolean] whether the lease has been settled in any way (returned,
  #   invalidated or transferred) and can therefore no longer release the
  #   connection.
  def released?
    @mutex.synchronize { @state != :active }
  end

  # @return [Exception, nil] the exception raised by the clean-up block passed
  #   to {#invalidate}, if any. Reported separately so clean-up failures never
  #   corrupt the pool's state or masquerade as check-out failures.
  attr_reader :cleanup_exception

  private

  # Runs +block+ only while the lease is still active, transitioning the state
  # exactly once. Returns whether the block ran so callers can tell a real
  # settlement from a duplicate, no-op call.
  def settle
    @mutex.synchronize do
      return false unless @state == :active

      yield
      true
    end
  end
end
