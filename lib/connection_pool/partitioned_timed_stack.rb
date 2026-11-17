require "digest"

##
# A TimedStack which partitions idle connections by an explicit partition
# key. Callers borrowing a connection for a specific address or session
# configuration only ever reuse idle connections from the same partition,
# never an idle object created for a different configuration.
#
# All partitions share a single capacity ledger: the total number of
# connections created across every partition is still bounded by +size+,
# and concurrent creation for the same key is accounted against the same
# ledger under the same mutex.
#
# Connections are created lazily: a partition that has never been checked
# out holds no resources and reaping or inspecting an unknown partition
# creates nothing.
#
# The factory block receives the partition key (its "tag") so the created
# connection's identity can be verified against what was actually borrowed:
#
#    ts = ConnectionPool::PartitionedTimedStack.new(size: 5) { |key| MyConnection.new(key) }
#    conn = ts.pop(key: "redis://host-a/0")
#    ts.push(conn, key: "redis://host-a/0")
#    ts.reap(idle_seconds: 60, key: "redis://host-a/0") { |conn| conn.close }
#
# Partition keys may contain credentials; diagnostics never include the raw
# key, only a one-way fingerprint (see .partition_label).
class ConnectionPool::PartitionedTimedStack < ConnectionPool::TimedStack
  ##
  # Returns a diagnostic-safe label for +key+. The raw key (which may embed
  # credentials) is never written into error messages or logs; only this
  # one-way fingerprint is.
  def self.partition_label(key)
    "partition-#{Digest::SHA256.hexdigest(key.inspect)[0, 12]}"
  end

  def initialize(size: 0, &block)
    super
    # partition key => [[connection, checked_in_at], ...]
    # Entries are only created when a connection is stored; unknown
    # partitions hold no state at all.
    @ques = {}
  end

  ##
  # Retrieves a connection for +key+, reusing an idle connection from that
  # partition or creating a new one via the tagged factory. Shares the
  # pool-wide capacity ledger with every other partition.
  def pop(timeout: 0.5, exception: ConnectionPool::TimeoutError, key: nil, **kwargs)
    super
  rescue ConnectionPool::TimeoutError => err
    raise err.exception("#{err.message}, #{self.class.partition_label(key)}")
  end

  ##
  # Reaps idle connections belonging to the given partition only. Idle
  # connections in other partitions are left untouched. Reaping an unknown
  # partition is a no-op and creates nothing.
  def reap(idle_seconds:, key: nil)
    raise ArgumentError, "reap must receive a block" unless block_given?
    raise ArgumentError, "idle_seconds must be a number" unless idle_seconds.is_a?(Numeric)
    raise ConnectionPool::PoolShuttingDownError if @shutdown_block

    count = idle(key:)
    count.times do
      conn = @mutex.synchronize do
        raise ConnectionPool::PoolShuttingDownError if @shutdown_block
        reserve_idle_connection(idle_seconds, key:)
      end
      break unless conn

      yield conn
    end
  end

  ##
  # Returns +true+ if there are no available connections.
  def empty?
    (@created - idle) >= @max
  end

  ##
  # The number of connections available on the stack, across all partitions.
  def length
    @max - @created + idle
  end

  ##
  # The number of created, idle connections. With +key+, only that
  # partition is counted; an unknown partition is 0 and creates nothing.
  def idle(key: nil)
    if key.nil?
      @ques.sum { |_, que| que.length }
    else
      @ques[key]&.length || 0
    end
  end

  private

  ##
  # Called with a mutex. True if an idle connection exists for +key+
  # (or in any partition when +key+ is nil, e.g. during shutdown).
  def connection_stored?(key: nil, **)
    if key.nil?
      @ques.each_value.any? { |que| !que.empty? }
    else
      (que = @ques[key]) && !que.empty?
    end
  end

  ##
  # Called with a mutex. Fetches an idle connection from the +key+
  # partition (or any partition when +key+ is nil). Empty partitions are
  # dropped so unknown partitions never accumulate state.
  def fetch_connection(key: nil, **)
    pair = nil
    if key.nil?
      pair = @ques.find { |_, que| !que.empty? }
    elsif @ques.key?(key)
      pair = [key, @ques[key]]
    end
    return unless pair

    k, que = pair
    conn = que.pop.first
    @ques.delete(k) if que.empty?
    conn
  end

  ##
  # Called with a mutex. Returns +obj+ to its partition's idle stack.
  def store_connection(obj, key: nil, **)
    (@ques[key] ||= []).push([obj, current_time])
    obj
  end

  ##
  # Called with a mutex. Creates a connection via the tagged factory if and
  # only if the shared, pool-wide capacity ledger has not been exhausted.
  def try_create(key: nil, **)
    unless @created == @max
      object = @create_block.call(key)
      @created += 1
      object
    end
  end

  ##
  # Called with a mutex. Reserves the oldest idle connection of the +key+
  # partition if it has been idle for more than +idle_seconds+. Other
  # partitions are never touched.
  def reserve_idle_connection(idle_seconds, key: nil)
    pair = reservable_partition(idle_seconds, key)
    return unless pair

    k, que = pair
    @created -= 1 unless @created == 0

    # Most active elements are at the tail of the array.
    # Most idle will be at the head so `shift` rather than `pop`.
    conn = que.shift.first
    @ques.delete(k) if que.empty?
    conn
  end

  def reservable_partition(idle_seconds, key)
    if key.nil?
      @ques.each do |k, que|
        next if que.empty?
        return [k, que] if (current_time - que.first.last) > idle_seconds
      end
      nil
    else
      que = @ques[key]
      return unless que && !que.empty?
      ((current_time - que.first.last) > idle_seconds) ? [key, que] : nil
    end
  end
end
