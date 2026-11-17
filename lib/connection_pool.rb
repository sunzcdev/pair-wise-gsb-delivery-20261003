require "timeout"
require_relative "connection_pool/version"

class ConnectionPool
  class Error < ::RuntimeError; end

  class PoolShuttingDownError < ::ConnectionPool::Error; end

  class TimeoutError < ::Timeout::Error; end
end

# Generic connection pool class for sharing a limited number of objects or network connections
# among many threads.  Note: pool elements are lazily created.
#
# Example usage with block (faster):
#
#    @pool = ConnectionPool.new { Redis.new }
#    @pool.with do |redis|
#      redis.lpop('my-list') if redis.llen('my-list') > 0
#    end
#
# Using optional timeout override (for that single invocation)
#
#    @pool.with(timeout: 2.0) do |redis|
#      redis.lpop('my-list') if redis.llen('my-list') > 0
#    end
#
# Example usage replacing an existing connection (slower):
#
#    $redis = ConnectionPool.wrap { Redis.new }
#
#    def do_work
#      $redis.lpop('my-list') if $redis.llen('my-list') > 0
#    end
#
# Accepts the following options:
# - :size - number of connections to pool, defaults to 5
# - :timeout - amount of time to wait for a connection if none currently available, defaults to 5 seconds
# - :auto_reload_after_fork - automatically drop all connections after fork, defaults to true
#
class ConnectionPool
  # Built once: a Hash literal argument costs a Hash and its keyword copy per call.
  DEFER_INTERRUPTS = {Exception => :never}.freeze
  IMMEDIATE_INTERRUPTS = {Exception => :immediate}.freeze
  private_constant :DEFER_INTERRUPTS, :IMMEDIATE_INTERRUPTS

  def self.wrap(**kwargs, &block)
    Wrapper.new(**kwargs, &block)
  end

  attr_reader :size

  def initialize(timeout: 5, size: 5, auto_reload_after_fork: true, name: nil, &block)
    raise ArgumentError, "Connection pool requires a block" unless block_given?

    @size = Integer(size)
    raise ArgumentError, "Connection pool size cannot be negative" if @size.negative?

    @timeout = Float(timeout)
    @available = TimedStack.new(size: @size, &block)
    @key = :"pool-#{@available.object_id}"
    @key_count = :"pool-#{@available.object_id}-count"
    @key_kwargs = :"pool-#{@available.object_id}-kwargs"
    @discard_key = :"pool-#{@available.object_id}-discard"
    INSTANCES[self] = self if auto_reload_after_fork && INSTANCES
  end

  def with(**kwargs)
    # We need to manage exception handling manually here in order
    # to work correctly with `Timeout.timeout` and `Thread#raise`.
    # Otherwise an interrupted Thread can leak connections.
    Thread.handle_interrupt(DEFER_INTERRUPTS) do
      conn = checkout(**kwargs)
      begin
        Thread.handle_interrupt(IMMEDIATE_INTERRUPTS) do
          yield conn
        end
      ensure
        checkin
      end
    end
  end
  alias_method :then, :with

  ##
  # Marks the current thread's checked-out connection for discard.
  #
  # When a connection is marked for discard, it will not be returned to the pool
  # when checked in. Instead, the connection will be discarded.
  # This is useful when a connection has become invalid or corrupted
  # and should not be reused.
  #
  # Takes an optional block that will be called with the connection to be discarded.
  # The block should perform any necessary clean-up on the connection.
  #
  # @yield [conn]
  # @yieldparam conn [Object] The connection to be discarded.
  # @yieldreturn [void]
  #
  #
  # Note: This only affects the connection currently checked out by the calling thread.
  # The connection will be discarded when +checkin+ is called.
  #
  # @return [void]
  #
  # @example
  #   pool.with do |conn|
  #     begin
  #       conn.execute("SELECT 1")
  #     rescue SomeConnectionError
  #       pool.discard_current_connection  # Mark connection as bad
  #       raise
  #     end
  #   end
  def discard_current_connection(&block)
    ::Thread.current[@discard_key] = block || proc { |conn| conn }
  end

  def checkout(timeout: @timeout, **kwargs)
    if ::Thread.current[@key]
      ::Thread.current[@key_count] += 1
      ::Thread.current[@key]
    else
      conn = @available.pop(timeout:, **kwargs)
      ::Thread.current[@key] = conn
      ::Thread.current[@key_count] = 1
      ::Thread.current[@key_kwargs] = kwargs
      conn
    end
  end

  ##
  # Checks out a connection and returns an explicit, thread-independent
  # ConnectionPool::Lease handle for it.
  #
  # Use this when the connection must be handed to another thread (or any other
  # owner) to do its work. Unlike +checkout+, ownership is not stored in the
  # calling thread's local variables: the returned Lease alone is enough to
  # release or invalidate the connection, and it records whether it has already
  # been settled.
  #
  #   lease = pool.lease
  #   Thread.new do
  #     begin
  #       do_work(lease.connection)
  #     rescue IOError
  #       lease.invalidate(&:close)
  #       raise
  #     else
  #       lease.release
  #     end
  #   end
  #
  # The current thread's reentrant +checkout+/+with+ state is deliberately left
  # untouched, so nested +with+ nesting counts continue to work independently.
  def lease(timeout: @timeout, **kwargs)
    conn = @available.pop(timeout:, **kwargs)
    Lease.new(self, conn, kwargs)
  end

  def checkin(force: false)
    if ::Thread.current[@key]
      if ::Thread.current[@key_count] == 1 || force
        begin
          if ::Thread.current[@discard_key]
            begin
              @available.decrement_created
              ::Thread.current[@discard_key].call(::Thread.current[@key])
            rescue
              nil
            ensure
              ::Thread.current[@discard_key] = nil
            end
          else
            @available.push(::Thread.current[@key], **::Thread.current[@key_kwargs])
          end
        ensure
          ::Thread.current[@key] = nil
          ::Thread.current[@key_count] = nil
          ::Thread.current[@key_kwargs] = nil
        end
      else
        ::Thread.current[@key_count] -= 1
      end
    elsif !force
      raise ConnectionPool::Error, "no connections are checked out"
    end

    nil
  end

  ##
  # Returns a connection owned by a Lease. Called only from
  # ConnectionPool::Lease#release; it never touches thread-local checkout
  # state. Late releases after +shutdown+ are cleaned up by the TimedStack's
  # shutdown block.
  #
  # :nodoc:
  def return_leased_connection(conn, kwargs)
    @available.push(conn, **kwargs)
    nil
  end

  ##
  # Removes a connection owned by a Lease from the pool's accounting. Called
  # only from ConnectionPool::Lease#invalidate; it never touches thread-local
  # checkout state.
  #
  # :nodoc:
  def discard_leased_connection(_conn)
    @available.decrement_created
    nil
  end
  protected :return_leased_connection, :discard_leased_connection

  ##
  # Shuts down the ConnectionPool by passing each connection to +block+ and
  # then removing it from the pool. Attempting to checkout a connection after
  # shutdown will raise +ConnectionPool::PoolShuttingDownError+.
  def shutdown(&block)
    @available.shutdown(&block)
  end

  ##
  # Reloads the ConnectionPool by passing each connection to +block+ and then
  # removing it the pool. Subsequent checkouts will create new connections as
  # needed.
  def reload(&block)
    @available.shutdown(reload: true, &block)
  end

  ## Reaps idle connections that have been idle for over +idle_seconds+.
  # +idle_seconds+ defaults to 60.
  def reap(idle_seconds: 60, &block)
    @available.reap(idle_seconds:, &block)
  end

  # Number of pool entries available for checkout at this instant.
  def available
    @available.length
  end

  # Number of pool entries created and idle in the pool.
  def idle
    @available.idle
  end
end

require_relative "connection_pool/timed_stack"
require_relative "connection_pool/lease"
require_relative "connection_pool/wrapper"
require_relative "connection_pool/fork"
