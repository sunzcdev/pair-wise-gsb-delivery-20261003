class ConnectionPool
  ##
  # A connection pool whose connections are partitioned by an explicit,
  # caller-supplied partition key. Clients of the same platform that borrow
  # connections by address and session configuration only ever reuse idle
  # connections from the same partition, never an idle object created for a
  # different configuration.
  #
  # The caller owns key normalization: pass +normalize_key+ to map a raw
  # configuration (host, port, session options, ...) onto the canonical
  # partition key used for reuse. Every partition still shares the pool-wide
  # capacity limit (+size+), and connections are created lazily -- a
  # partition that was never checked out holds no resources.
  #
  # The factory block receives the normalized partition key (its "tag"), so
  # the created connection's identity can be verified against what was
  # actually borrowed:
  #
  #    pool = ConnectionPool::Partitioned.new(size: 5, normalize_key: ->(cfg) { [cfg[:host], cfg[:db]] }) do |key|
  #      Redis.new(host: key[0], db: key[1])
  #    end
  #
  #    pool.with(key: {host: "a.example", db: 0}) do |redis|
  #      redis.lpop("my-list")
  #    end
  #
  # Nesting +with+ for the same partition reuses the current connection (as
  # with ConnectionPool), but nesting a *different* partition raises
  # ConnectionPool::Error rather than silently handing out the wrong
  # connection.
  #
  # Partition keys may embed credentials; diagnostics (error messages, etc.)
  # only ever contain a one-way fingerprint of the key, never the raw key.
  class Partitioned < ConnectionPool
    def initialize(timeout: 5, size: 5, auto_reload_after_fork: true, normalize_key: nil, &block)
      raise ArgumentError, "Connection pool requires a block" unless block_given?

      @normalize_key = normalize_key

      @size = Integer(size)
      raise ArgumentError, "Connection pool size cannot be negative" if @size.negative?

      @timeout = Float(timeout)
      @available = PartitionedTimedStack.new(size: @size, &block)
      @key = :"pool-#{@available.object_id}"
      @key_count = :"pool-#{@available.object_id}-count"
      @key_partition = :"pool-#{@available.object_id}-partition"
      @discard_key = :"pool-#{@available.object_id}-discard"
      INSTANCES[self] = self if auto_reload_after_fork && INSTANCES
    end

    ##
    # Checks out a connection for the given partition +key+. The key is
    # normalized with the pool's +normalize_key+ callable (identity by
    # default). If the current thread already holds a connection, it is
    # reused only when it belongs to the same partition; requesting a
    # different partition raises ConnectionPool::Error instead of silently
    # reusing the wrong connection.
    def checkout(key:, timeout: @timeout)
      partition = normalize_partition_key(key)
      if (conn = ::Thread.current[@key])
        current = ::Thread.current[@key_partition]
        unless current == partition
          raise ConnectionPool::Error,
            "this thread already holds a connection from " \
            "#{PartitionedTimedStack.partition_label(current)}, cannot nest a checkout for " \
            "#{PartitionedTimedStack.partition_label(partition)}"
        end
        ::Thread.current[@key_count] += 1
        conn
      else
        conn = @available.pop(timeout:, key: partition)
        ::Thread.current[@key] = conn
        ::Thread.current[@key_count] = 1
        ::Thread.current[@key_partition] = partition
        conn
      end
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
              @available.push(::Thread.current[@key], key: ::Thread.current[@key_partition])
            end
          ensure
            ::Thread.current[@key] = nil
            ::Thread.current[@key_count] = nil
            ::Thread.current[@key_partition] = nil
          end
        else
          ::Thread.current[@key_count] -= 1
        end
      elsif !force
        raise ConnectionPool::Error, "no connections are checked out"
      end

      nil
    end

    ## Reaps idle connections of the given partition that have been idle
    # for over +idle_seconds+ (default 60). Only the owning partition is
    # reaped; other partitions are untouched and unknown partitions are a
    # no-op that creates nothing.
    def reap(idle_seconds: 60, key:, &block)
      @available.reap(idle_seconds:, key: normalize_partition_key(key), &block)
    end

    # Number of pool entries created and idle. With +key+, only that
    # partition is counted.
    def idle(key: nil)
      @available.idle(key: key.nil? ? nil : normalize_partition_key(key))
    end

    private

    def normalize_partition_key(key)
      partition = @normalize_key ? @normalize_key.call(key) : key
      raise ArgumentError, "partition key must not be nil" if partition.nil?
      partition
    end
  end
end
