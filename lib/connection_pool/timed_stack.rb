##
# The TimedStack manages a pool of homogeneous connections (or any resource
# you wish to manage). Connections are created lazily up to a given maximum
# number.
#
# Creating a connection can be slow (opening a socket, performing a TLS
# handshake, ...), so TimedStack never calls the creation block while holding
# its lock. Capacity for a new connection is first reserved under the lock, the
# connection itself is built outside of it and the connection is admitted to
# the stack once it exists. Other threads keep checking connections in, out and
# using already idle connections while a slow creation runs.
#
# Every reservation is settled exactly once:
#
# * a created connection is admitted to the stack and its slot becomes a
#   created connection. The caller that created it then takes it from the
#   stack, so cancellation cannot separate the caller from the connection;
# * when the creation block raises, or the creating thread is cancelled before
#   a connection exists, the reservation is released so the capacity can be
#   used again;
# * when the creating thread is cancelled after the connection exists, the
#   connection is admitted to the stack anyway and stays available;
# * when the stack is shut down while a connection is being created, the late
#   connection is passed to the shutdown block instead of being admitted.
#
# The stack never creates more connections than its size and never loses a
# connection it created. Preloading (#preload) and creating connections on
# demand (#pop) follow the same capacity and reservation rules.
#
# Examples:
#
#    ts = TimedStack.new(size: 1) { MyConnection.new }
#
#    # fetch a connection
#    conn = ts.pop
#
#    # return a connection
#    ts.push conn
#
#    conn = ts.pop
#    ts.pop timeout: 5
#    #=> raises ConnectionPool::TimeoutError after 5 seconds
class ConnectionPool::TimedStack
  attr_reader :max

  # Asynchronous interruptions are deferred while a reservation is settled, so
  # a connection which was created is always accounted for before a
  # cancellation is delivered.
  SETTLE_INTERRUPTS = {Exception => :never}.freeze
  private_constant :SETTLE_INTERRUPTS

  ##
  # Creates a new pool with +size+ connections that are created from the given
  # +block+.
  def initialize(size: 0, &block)
    @create_block = block
    @created = 0
    # Slots reserved by threads which are creating a connection outside the
    # stack lock. `@created + @reserved` never exceeds `@max`.
    @reserved = 0
    @que = []
    @max = size
    @mutex = Thread::Mutex.new
    @resource = Thread::ConditionVariable.new
    @shutdown_block = nil
  end

  ##
  # Returns +obj+ to the stack. Additional kwargs are ignored in TimedStack but may be
  # used by subclasses that extend TimedStack.
  def push(obj, **kwargs)
    @mutex.synchronize do
      if @shutdown_block
        @created -= 1 unless @created == 0
        @shutdown_block.call(obj)
      else
        store_connection obj, **kwargs
      end

      @resource.broadcast
    end
  end
  alias_method :<<, :push

  ##
  # Retrieves a connection from the stack. If a connection is available it is
  # immediately returned. If no connection is available within the given
  # timeout a ConnectionPool::TimeoutError is raised.
  #
  # When no connection is idle, capacity for a new one is reserved and the
  # connection is created without holding the stack lock. Other threads can
  # therefore keep checking connections in, checking them out and using idle
  # connections while a slow creation block runs. The timeout covers waiting
  # for capacity as well as creating the connection.
  #
  # A connection built by this call is admitted to the stack before it is
  # returned, so a cancellation arriving during creation cannot leak it.
  #
  # @option options [Float] :timeout (0.5) Wait this many seconds for an available entry
  # @option options [Class] :exception (ConnectionPool::TimeoutError) Exception class to raise
  #   if an entry was not available within the timeout period. Use `exception: false` to return nil.
  #
  # Other options may be used by subclasses that extend TimedStack.
  def pop(timeout: 0.5, exception: ConnectionPool::TimeoutError, **kwargs)
    deadline = current_time + timeout
    loop do
      conn = @mutex.synchronize do
        raise ConnectionPool::PoolShuttingDownError if @shutdown_block
        try_fetch_connection(**kwargs)
      end
      return conn if conn

      # Reserve capacity and build a connection without the lock held. The new
      # connection is admitted to the stack, never handed over from inside the
      # creation block, so a caller which is cancelled mid-creation cannot leak
      # it: the connection simply stays available for somebody else.
      created = try_create(**kwargs)

      action = @mutex.synchronize do
        raise ConnectionPool::PoolShuttingDownError if @shutdown_block

        # Take the connection we (or a racing thread) just made.
        if (conn = try_fetch_connection(**kwargs))
          return conn
        end

        # A connection was created or a slot is free, so trying again is
        # progress. Otherwise the pool is full and the only thing to do is to
        # wait for a checkin, a discarded connection or a released reservation.
        if created || capacity_available?
          :retry
        elsif (to_wait = deadline - current_time) > 0
          @resource.wait(@mutex, to_wait)
          :retry
        elsif exception
          raise exception, "Waited #{timeout} sec, #{length}/#{@max} available"
        else
          :give_up
        end
      end
      return nil if action == :give_up
    end
  end

  ##
  # Preloads connections into the stack, up to +count+ (the pool size by
  # default). Connections are created outside the lock and counted exactly like
  # connections created on demand by #pop, so preloading never creates more
  # connections than the pool size, even when checkouts race with it. Returns
  # the number of connections created and stored.
  #
  # Raises ConnectionPool::PoolShuttingDownError if the stack is shutting down.
  # An exception raised by the creation block releases its reservation and
  # propagates to the caller.
  def preload(count: @max, **kwargs)
    count = Integer(count)
    raise ArgumentError, "preload count cannot be negative" if count.negative?

    created = 0
    count.times do
      @mutex.synchronize do
        raise ConnectionPool::PoolShuttingDownError if @shutdown_block
      end

      break unless try_create(**kwargs)
      created += 1
    end
    created
  end

  ##
  # Shuts down the TimedStack by passing each connection to +block+ and then
  # removing it from the pool. Attempting to checkout a connection after
  # shutdown will raise +ConnectionPool::PoolShuttingDownError+ unless
  # +:reload+ is +true+.
  #
  # Connections which are still being created are not waited for; when they
  # finish they are passed to +block+ instead of being added to the pool.
  def shutdown(reload: false, &block)
    raise ArgumentError, "shutdown must receive a block" unless block

    @mutex.synchronize do
      @shutdown_block = block
      @resource.broadcast

      shutdown_connections
      @shutdown_block = nil if reload
    end
  end

  ##
  # Reaps connections that were checked in more than +idle_seconds+ ago.
  def reap(idle_seconds:)
    raise ArgumentError, "reap must receive a block" unless block_given?
    raise ArgumentError, "idle_seconds must be a number" unless idle_seconds.is_a?(Numeric)
    raise ConnectionPool::PoolShuttingDownError if @shutdown_block

    count = idle
    count.times do
      conn = @mutex.synchronize do
        raise ConnectionPool::PoolShuttingDownError if @shutdown_block
        reserve_idle_connection(idle_seconds)
      end
      break unless conn

      yield conn
    end
  end

  ##
  # Returns +true+ if there are no available connections.
  def empty?
    (@created + @reserved - @que.length) >= @max
  end

  ##
  # The number of connections available on the stack. Capacity reserved for
  # connections which are still being created is not available yet.
  def length
    @max - @created - @reserved + @que.length
  end

  ##
  # The number of connections created and available on the stack.
  def idle
    @que.length
  end

  ##
  # The number of capacity slots currently reserved by threads which are
  # creating a connection outside the stack lock.
  def reserved
    @reserved
  end

  ##
  # Reduce the created count
  def decrement_created
    @mutex.synchronize do
      @created -= 1 unless @created == 0
      @resource.broadcast
    end
  end

  private

  def current_time
    Process.clock_gettime(Process::CLOCK_MONOTONIC)
  end

  ##
  # Called with a mutex. Returns true if another connection may be created.
  def capacity_available?
    (@created + @reserved) < @max
  end

  ##
  # Reserves capacity for one connection. Must not be called with a mutex: the
  # reservation is taken under the lock, the connection is created without it
  # and the reservation is settled exactly once with #admit_created or
  # #release_capacity.
  def reserve_capacity
    @mutex.synchronize do
      return false if @shutdown_block
      return false unless capacity_available?

      @reserved += 1
      true
    end
  end

  ##
  # Gives a reservation back, e.g. because the creation block raised or the
  # creating thread was cancelled before a connection existed.
  def release_capacity
    @mutex.synchronize do
      @reserved -= 1 unless @reserved == 0
      @resource.broadcast
    end
  end

  ##
  # Turns a reservation into a created connection. The connection is stored on
  # the stack at the same moment the accounting changes, so a caller which is
  # cancelled right after admission leaves the connection available for the
  # next thread rather than losing it.
  #
  # If the stack is shutting down, the late connection is handed to the
  # shutdown block instead of being added to the pool.
  def admit_created(conn)
    @mutex.synchronize do
      @reserved -= 1 unless @reserved == 0

      if @shutdown_block
        @shutdown_block.call(conn)
        false
      else
        @created += 1
        store_created_connection(conn)
        @resource.broadcast
        true
      end
    end
  end

  ##
  # Reserves capacity, creates a connection outside the lock and settles the
  # reservation exactly once. Returns true when a new connection was admitted
  # to the stack, or false when the pool is full, is shutting down or the late
  # connection was closed by the shutdown block.
  #
  # The reservation is settled in a deferred-interrupt region and matched by an
  # +ensure+, so no cancellation or factory error can leave the capacity
  # accounting inconsistent or lose a created connection.
  def create_reserved
    conn = nil
    reserved = false
    settled = false

    begin
      # Cancellations are deferred across the reservation itself so that
      # +reserved+ is always accurate when one is delivered.
      Thread.handle_interrupt(SETTLE_INTERRUPTS) do
        reserved = reserve_capacity
      end
      return false unless reserved

      conn = @create_block.call

      Thread.handle_interrupt(SETTLE_INTERRUPTS) do
        begin
          admit_created(conn)
        ensure
          settled = true
        end
      end
      true
    ensure
      if reserved && !settled
        # The creation block raised, or this thread was cancelled before the
        # reservation was settled. Settle it now with interruptions deferred so
        # the capacity or the connection is always accounted for.
        Thread.handle_interrupt(SETTLE_INTERRUPTS) do
          begin
            if conn
              # The connection exists but this thread is unwinding. Admit it so
              # it stays available for another thread instead of being lost.
              admit_created(conn)
            else
              # No connection was created, so give the capacity back.
              release_capacity
            end
          rescue Exception
            # A failing shutdown block must not mask the original error or
            # cancellation that is unwinding this thread.
            nil
          end
        end
      end
    end
  end

  ##
  # This is an extension point for TimedStack. Unlike the other extension
  # points it is deliberately called *without* a mutex so that a slow creation
  # block cannot prevent other threads from checking connections in or out.
  #
  # This method must create a connection if and only if capacity for it could
  # be reserved. It returns true when a connection was admitted to the stack.
  def try_create(**_kwargs)
    create_reserved
  end

  ##
  # This is an extension point for TimedStack and is called with a mutex.
  #
  # This method must returns a connection from the stack if one exists. Allows
  # subclasses with expensive match/search algorithms to avoid double-handling
  # their stack.
  def try_fetch_connection(**kwargs)
    connection_stored?(**kwargs) && fetch_connection(**kwargs)
  end

  ##
  # This is an extension point for TimedStack and is called with a mutex.
  #
  # This method must returns true if a connection is available on the stack.
  def connection_stored?(**_kwargs)
    !@que.empty?
  end

  ##
  # This is an extension point for TimedStack and is called with a mutex.
  #
  # This method must return a connection from the stack.
  def fetch_connection(**_kwargs)
    @que.pop&.first
  end

  ##
  # This is an extension point for TimedStack and is called with a mutex.
  #
  # This method must shut down all connections on the stack.
  def shutdown_connections(**kwargs)
    while (conn = try_fetch_connection(**kwargs))
      @created -= 1 unless @created == 0
      @shutdown_block.call(conn)
    end
  end

  ##
  # This is an extension point for TimedStack and is called with a mutex.
  #
  # This method returns the oldest idle connection if it has been idle for more than idle_seconds.
  # This requires that the stack is kept in order of checked in time (oldest first).
  def reserve_idle_connection(idle_seconds)
    return unless idle_connections?(idle_seconds)

    @created -= 1 unless @created == 0

    # Most active elements are at the tail of the array.
    # Most idle will be at the head so `shift` rather than `pop`.
    @que.shift.first
  end

  ##
  # This is an extension point for TimedStack and is called with a mutex.
  #
  # Returns true if the first connection in the stack has been idle for more than idle_seconds
  def idle_connections?(idle_seconds)
    return unless connection_stored?
    # Most idle will be at the head so `first`
    age = (current_time - @que.first.last)
    age > idle_seconds
  end

  ##
  # This is an extension point for TimedStack and is called with a mutex.
  #
  # This method must return +obj+ to the stack.
  def store_connection(obj, **_kwargs)
    @que.push [obj, current_time]
  end

  ##
  # This is an extension point for TimedStack and is called with a mutex.
  #
  # This method must admit a freshly created connection to the stack. It is
  # separate from #store_connection on purpose: #store_connection is the
  # checkin path and receives the checkout keywords, which do not exist yet
  # when a connection is first created. Subclasses which keep extra per
  # connection state must override this method as well.
  def store_created_connection(obj)
    @que.push [obj, current_time]
  end
end
