##
# The TimedStack manages a pool of homogeneous connections (or any resource
# you wish to manage). Connections are created lazily up to a given maximum
# number.
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

  ##
  # Creates a new pool with +size+ connections that are created from the given
  # +block+.
  #
  # @option options [Proc, nil] :health_check called with a connection on checkout
  #   and checkin. Returning a falsey value (or raising) marks the connection as
  #   invalid: it is removed from the pool and replaced.
  # @option options [Proc, nil] :cleanup called with a connection outside the
  #   internal lock whenever one is removed due to a failed health check.
  def initialize(size: 0, health_check: nil, cleanup: nil, &block)
    @create_block = block
    @health_check = health_check
    @cleanup = cleanup
    @created = 0
    @que = []
    @max = size
    @mutex = Thread::Mutex.new
    @resource = Thread::ConditionVariable.new
    @shutdown_block = nil
  end

  ##
  # Returns +obj+ to the stack. Additional kwargs are ignored in TimedStack but may be
  # used by subclasses that extend TimedStack.
  #
  # If a +:health_check+ is configured (or passed per call), it runs against
  # +obj+ *outside* of the internal lock. A failed check discards the connection
  # instead of storing it. Health-check failures on the checkin path never raise
  # so they cannot mask an exception raised by the caller's own code.
  def push(obj, **kwargs)
    health_check = kwargs.delete(:health_check) { @health_check }

    if health_check
      healthy, = check_health(obj, health_check)
      unless healthy
        # discard_connection never raises: capacity is released under the lock
        # and cleanup errors are swallowed inside it.
        discard_connection(obj)
        return
      end
    end

    @mutex.synchronize do
      if @shutdown_block
        @created -= 1 unless @created == 0
        @shutdown_block.call(obj)
      else
        store_connection obj, **kwargs
      end

      @resource.broadcast
    end
    nil
  end
  alias_method :<<, :push

  ##
  # Permanently removes +obj+ from the pool, decrementing the created count and
  # invoking +block+ (or the configured cleanup callback) outside of the
  # internal lock. Errors raised by the callback are swallowed.
  def discard(obj, &block)
    decrement_created

    begin
      if block
        block.call(obj)
      else
        cleanup_connection(obj)
      end
    rescue
      nil
    end

    nil
  end

  ##
  # Retrieves a connection from the stack. If a connection is available it is
  # immediately returned. If no connection is available within the given
  # timeout a ConnectionPool::TimeoutError is raised.
  #
  # @option options [Float] :timeout (0.5) Wait this many seconds for an available entry
  # @option options [Class] :exception (ConnectionPool::TimeoutError) Exception class to raise
  #   if an entry was not available within the timeout period. Use `exception: false` to return nil.
  # @option options [Proc, nil] :health_check overrides the stack's health check
  #   for this checkout.
  #
  # When a health check is active it runs *outside* of the internal lock against
  # every connection before it is handed to the caller. A connection that fails
  # the check is removed from the pool (its capacity is released) and another
  # resource is acquired instead, but at most +@max+ failed connections are
  # replaced per checkout, so a permanently unhealthy pool can never trigger an
  # unbounded rebuild.
  def pop(timeout: 0.5, exception: ConnectionPool::TimeoutError, **kwargs)
    deadline = current_time + timeout
    health_check = kwargs.delete(:health_check) { @health_check }
    # One replacement per pool slot per checkout bounds the total work and the
    # number of connections created to @max + 1.
    failures = 0
    last_health_error = nil

    loop do
      # Fetching/creating and waiting happen inside one synchronized section so
      # a concurrent push/broadcast cannot be lost. The result tuple tells the
      # lock-free code below what to do next.
      status, conn = @mutex.synchronize do
        if @shutdown_block
          [:shutdown, nil]
        elsif (connection = try_fetch_connection(**kwargs))
          [:connection, connection]
        elsif (connection = try_create(**kwargs))
          [:connection, connection]
        else
          to_wait = deadline - current_time
          if to_wait <= 0
            [:timeout, nil]
          else
            @resource.wait(@mutex, to_wait)
            [:retry, nil]
          end
        end
      end

      case status
      when :shutdown
        raise ConnectionPool::PoolShuttingDownError
      when :timeout
        return nil unless exception

        raise exception, "Waited #{timeout} sec, #{length}/#{@max} available"
      when :retry
        next
      end

      if health_check
        # The user callback runs without holding the internal lock so a slow
        # or blocking health check cannot stall other pool operations.
        healthy, error = check_health(conn, health_check)
        unless healthy
          discard_connection(conn)
          failures += 1
          last_health_error = error
          if failures > @max
            raise ConnectionPool::HealthCheckFailedError.new(
              "All connections failed the health check after #{failures} attempts",
              last_health_error
            )
          end
          next
        end
      end

      return conn
    end
  end

  ##
  # Shuts down the TimedStack by passing each connection to +block+ and then
  # removing it from the pool. Attempting to checkout a connection after
  # shutdown will raise +ConnectionPool::PoolShuttingDownError+ unless
  # +:reload+ is +true+.
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
    (@created - @que.length) >= @max
  end

  ##
  # The number of connections available on the stack.
  def length
    @max - @created + @que.length
  end

  ##
  # The number of connections created and available on the stack.
  def idle
    @que.length
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
  # Runs +health_check+ against +conn+ outside of the internal lock. A falsey
  # return value or a raised exception both mean the connection is unhealthy.
  # Returns [healthy, error].
  def check_health(conn, health_check)
    [!!health_check.call(conn), nil]
  rescue => error
    [false, error]
  end

  ##
  # Removes +conn+ from the pool's capacity accounting and runs the configured
  # cleanup callback outside of the internal lock. Used when a connection fails
  # a health check so its slot can be reused.
  def discard_connection(conn)
    decrement_created
    cleanup_connection(conn)
  end

  ##
  # Invokes the cleanup callback for +conn+ outside of the internal lock.
  # Errors raised by user cleanup code must not affect pool bookkeeping.
  def cleanup_connection(conn)
    @cleanup&.call(conn)
  rescue
    nil
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
  # This method must create a connection if and only if the total number of
  # connections allowed has not been met.
  def try_create(**_kwargs)
    unless @created == @max
      object = @create_block.call
      @created += 1
      object
    end
  end
end
