# frozen-string-literal: true

# The base connection pool class, which all other connection pools are based
# on.  This class is not instantiated directly, but subclasses should at
# the very least implement the following API:
#
# initialize(Database, Hash) :: Initialize using the passed Sequel::Database
#                               object and options hash.
# hold(Symbol, &block) :: Yield a connection object (obtained from calling
#                         the block passed to +initialize+) to the current block. For sharded
#                         connection pools, the Symbol passed is the shard/server to use.
# disconnect(Symbol) :: Disconnect the connection object.  For sharded
#                       connection pools, the Symbol passed is the shard/server to use.
# servers :: An array of shard/server symbols for all shards/servers that this
#            connection pool recognizes.
# size :: an integer representing the total number of connections in the pool,
#         or for the given shard/server if sharding is supported.
# max_size :: an integer representing the maximum size of the connection pool,
#             or the maximum size per shard/server if sharding is supported.
#
# For sharded connection pools, the sharded API adds the following methods:
#
# add_servers(Array of Symbols) :: start recognizing all shards/servers specified
#                                  by the array of symbols.
# remove_servers(Array of Symbols) :: no longer recognize all shards/servers
#                                     specified by the array of symbols.
class Sequel::ConnectionPool
  OPTS = Sequel::OPTS
  POOL_CLASS_MAP = {
    :threaded => :ThreadedConnectionPool,
    :single => :SingleConnectionPool,
    :sharded_threaded => :ShardedThreadedConnectionPool,
    :sharded_single => :ShardedSingleConnectionPool,
    :timed_queue => :TimedQueueConnectionPool,
    :sharded_timed_queue => :ShardedTimedQueueConnectionPool,
  }
  POOL_CLASS_MAP.to_a.each{|k, v| POOL_CLASS_MAP[k.to_s] = v}
  POOL_CLASS_MAP.freeze

  # Class methods used to return an appropriate pool subclass, separated
  # into a module for easier overriding by extensions.
  module ClassMethods
    # Return a pool subclass instance based on the given options.  If a <tt>:pool_class</tt>
    # option is provided is provided, use that pool class, otherwise
    # use a new instance of an appropriate pool subclass based on the
    # +SEQUEL_DEFAULT_CONNECTION_POOL+ environment variable if set, or
    # the <tt>:single_threaded</tt> and <tt>:servers</tt> options, otherwise.
    def get_pool(db, opts = OPTS)
      connection_pool_class(opts).new(db, opts)
    end
    
    private
    
    # Return a connection pool class based on the given options.
    def connection_pool_class(opts)
      if pc = opts[:pool_class]
        unless pc.is_a?(Class)
          unless name = POOL_CLASS_MAP[pc]
            raise Sequel::Error, "unsupported connection pool type, please pass appropriate class as the :pool_class option"
          end

          require_relative "connection_pool/#{pc}"
          pc = Sequel.const_get(name)
        end

        pc
      elsif pc = ENV['SEQUEL_DEFAULT_CONNECTION_POOL']
        pc = "sharded_#{pc}" if opts[:servers] && !pc.start_with?('sharded_')
        connection_pool_class(:pool_class=>pc)
      else
        pc = if opts[:single_threaded]
          opts[:servers] ? :sharded_single : :single
        elsif RUBY_VERSION >= '3.2'
          opts[:servers] ? :sharded_timed_queue : :timed_queue
        # simplecov:disable
        else
          opts[:servers] ? :sharded_threaded : :threaded
        end
        # simplecov:enable

        connection_pool_class(:pool_class=>pc)
      end
    end
  end
  extend ClassMethods

  # Mutex protecting the wait graph data structures.
  WAIT_GRAPH_MUTEX = Mutex.new

  # Wait graph of checked out connections, used for wait cycle detection.
  # Maps owners (Sequel.current values) to hashes of [pool, server] pairs
  # to checkout counts.  Only used by pools with the
  # :pool_wait_cycle_detection option enabled.
  WAIT_GRAPH_HOLDS = {}.compare_by_identity

  # Wait graph of connection waits, used for wait cycle detection.
  # Maps owners (Sequel.current values) to the [pool, server] pair the
  # owner is currently waiting on.  Only used by pools with the
  # :pool_wait_cycle_detection option enabled.
  WAIT_GRAPH_WAITS = {}.compare_by_identity

  private_constant :WAIT_GRAPH_MUTEX, :WAIT_GRAPH_HOLDS, :WAIT_GRAPH_WAITS

  # Register in the wait graph that the owner holds a connection to the
  # given server on the given pool.
  def self.wait_graph_register_hold(owner, pool, server)
    target = [pool, server].freeze
    WAIT_GRAPH_MUTEX.synchronize do
      targets = (WAIT_GRAPH_HOLDS[owner] ||= {})
      targets[target] = targets.fetch(target, 0) + 1
    end
    nil
  end

  # Remove a previously registered hold from the wait graph.  Does nothing
  # if the owner does not have a registered hold for the pool and server.
  def self.wait_graph_unregister_hold(owner, pool, server)
    WAIT_GRAPH_MUTEX.synchronize do
      if targets = WAIT_GRAPH_HOLDS[owner]
        target = [pool, server]
        case targets[target]
        when nil
          # Nothing registered, nothing to do
        when 1
          targets.delete(target)
          WAIT_GRAPH_HOLDS.delete(owner) if targets.empty?
        else
          targets[target] -= 1
        end
      end
    end
    nil
  end

  # Register in the wait graph that the owner is waiting for a connection
  # to the given server on the given pool.  Raises Sequel::PoolDeadlock
  # without registering the wait if the wait would create a cycle in the
  # wait graph.
  def self.wait_graph_register_wait(owner, pool, server)
    target = [pool, server].freeze
    WAIT_GRAPH_MUTEX.synchronize do
      if chain = wait_graph_cycle(owner, target)
        raise Sequel::PoolDeadlock, wait_graph_cycle_message(chain)
      end
      WAIT_GRAPH_WAITS[owner] = target
    end
    nil
  end

  # Remove a previously registered wait from the wait graph.
  def self.wait_graph_unregister_wait(owner)
    WAIT_GRAPH_MUTEX.synchronize{WAIT_GRAPH_WAITS.delete(owner)}
    nil
  end

  # If the owner waiting for the given target (pool and server pair) would
  # create a cycle in the wait graph, return an array of [owner, target]
  # pairs forming the cycle.  The calling code should hold WAIT_GRAPH_MUTEX.
  def self.wait_graph_cycle(owner, target)
    visited = {}.compare_by_identity
    stack = wait_graph_holders(target).map{|holder| [holder, [[owner, target]]]}

    until stack.empty?
      current, chain = stack.pop
      return chain if current.equal?(owner)
      next if visited[current]
      visited[current] = true

      if current_target = WAIT_GRAPH_WAITS[current]
        new_chain = chain + [[current, current_target]]
        wait_graph_holders(current_target).each do |holder|
          stack.push([holder, new_chain])
        end
      end
    end

    nil
  end
  private_class_method :wait_graph_cycle

  # An array of owners currently holding connections for the given target
  # (pool and server pair).  The calling code should hold WAIT_GRAPH_MUTEX.
  def self.wait_graph_holders(target)
    holders = []
    WAIT_GRAPH_HOLDS.each_pair do |owner, targets|
      holders << owner if targets.key?(target)
    end
    holders
  end
  private_class_method :wait_graph_holders

  # Error message for the given wait graph cycle, describing each owner in
  # the cycle and the pool server it is waiting on.  This intentionally
  # does not include any connection credentials.
  def self.wait_graph_cycle_message(chain)
    parts = chain.map do |owner, (pool, server)|
      "#{owner.inspect} waiting on #{pool.pool_type} pool server: #{server.inspect}"
    end
    parts << chain.first[0].inspect
    "connection pool wait cycle detected: #{parts.join(' -> ')}"
  end
  private_class_method :wait_graph_cycle_message

  # The after_connect proc used for this pool.  This is called with each new
  # connection made, and is usually used to set custom per-connection settings.
  # Deprecated.
  attr_reader :after_connect # SEQUEL6: Remove

  # Override the after_connect proc for the connection pool. Deprecated.
  # Disables support for shard-specific :after_connect and :connect_sqls if used.
  def after_connect=(v) # SEQUEL6: Remove
    @use_old_connect_api = true
    @after_connect = v
  end

  # An array of sql strings to execute on each new connection. Deprecated.
  attr_reader :connect_sqls # SEQUEL6: Remove

  # Override the connect_sqls for the connection pool. Deprecated.
  # Disables support for shard-specific :after_connect and :connect_sqls if used.
  def connect_sqls=(v) # SEQUEL6: Remove
    @use_old_connect_api = true
    @connect_sqls = v
  end

  # The Sequel::Database object tied to this connection pool.
  attr_accessor :db

  # Instantiates a connection pool with the given Database and options.
  def initialize(db, opts=OPTS) # SEQUEL6: Remove second argument, always use db.opts
    @db = db
    @use_old_connect_api = false # SEQUEL6: Remove
    @after_connect = opts[:after_connect] # SEQUEL6: Remove
    @connect_sqls = opts[:connect_sqls] # SEQUEL6: Remove
    @wait_cycle_detection = opts[:pool_wait_cycle_detection]
    @error_classes = db.send(:database_error_classes).dup.freeze
  end
  
  # An array of symbols for all shards/servers, which is a single <tt>:default</tt> by default.
  def servers
    [:default]
  end
  
  private

  # Remove the connection from the pool.  For threaded connections, this should be
  # called without the mutex, because the disconnection may block.
  def disconnect_connection(conn)
    db.disconnect_connection(conn)
  end

  # Only for use by extension that need to disconnect a connection inside acquire.
  # Takes the connection and any arguments accepted by acquire.
  def disconnect_acquired_connection(conn, *)
    disconnect_connection(conn)
  end

  # Whether the given exception is a disconnect exception.
  def disconnect_error?(exception)
    exception.is_a?(Sequel::DatabaseDisconnectError) || db.send(:disconnect_error?, exception, OPTS)
  end
  
  # Return a new connection by calling the connection proc with the given server name,
  # and checking for connection errors.
  def make_new(server)
    begin
      if @use_old_connect_api
        # SEQUEL6: Remove block
        conn = @db.connect(server)

        if ac = @after_connect
          if ac.arity == 2
            ac.call(conn, server)
          else
            ac.call(conn)
          end
        end
  
        if cs = @connect_sqls
          cs.each do |sql|
            db.send(:log_connection_execute, conn, sql)
          end
        end

        conn
      else
        @db.new_connection(server)
      end
    rescue Exception=>exception
      raise Sequel.convert_exception_class(exception, Sequel::DatabaseConnectionError)
    end || raise(Sequel::DatabaseConnectionError, "Connection parameters not valid")
  end

  # If wait cycle detection is enabled for the pool, register in the wait
  # graph that the owner holds a connection to the given server.
  def register_wait_cycle_hold(owner, server)
    Sequel::ConnectionPool.wait_graph_register_hold(owner, self, server) if @wait_cycle_detection
  end

  # If wait cycle detection is enabled for the pool, remove the owner's
  # hold on the given server from the wait graph.
  def unregister_wait_cycle_hold(owner, server)
    Sequel::ConnectionPool.wait_graph_unregister_hold(owner, self, server) if @wait_cycle_detection
  end

  # Register in the wait graph that the owner is waiting for a connection
  # to the given server, yield to wait for the connection, and remove the
  # wait from the wait graph when the wait is over.  Raises
  # Sequel::PoolDeadlock if the wait would create a cycle in the wait
  # graph.  This should only be called if wait cycle detection is enabled.
  def with_wait_cycle_wait(owner, server)
    Sequel::ConnectionPool.wait_graph_register_wait(owner, self, server)
    begin
      yield
    ensure
      Sequel::ConnectionPool.wait_graph_unregister_wait(owner)
    end
  end
end
