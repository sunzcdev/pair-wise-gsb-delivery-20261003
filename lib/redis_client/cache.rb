# frozen_string_literal: true

class RedisClient
  # Client-side cache for explicitly declared, read-only commands, backed by
  # RESP3 client tracking (`CLIENT TRACKING ON`).
  #
  # Only commands listed in `CACHEABLE_COMMANDS` (or in a custom `commands:`
  # map) are ever cached; the read/write nature of arbitrary commands is never
  # guessed, and anything the cache doesn't explicitly support transparently
  # goes through the regular code path.
  #
  # The server notifies the client of key modifications with `invalidate`
  # push messages, which are processed as they are read from the connection
  # and promptly evict the matching entries. Because invalidations can be
  # processed while a read is in flight, reads are tagged with a token when
  # they start and their late replies are only stored if the token is still
  # fresh, so a stale reply can never re-populate the cache.
  #
  # The cache `generation` is advanced (and all entries dropped) whenever the
  # server reports a full invalidation (e.g. `FLUSHALL`), when the connection
  # is lost, and when tracking is (re)established, because invalidations may
  # have been missed in between.
  #
  # Values stored in the cache are deeply frozen and shared between callers:
  # the cache retains ownership of the returned (potentially mutable) objects.
  # Callers must not mutate them; `dup` them first if you need to.
  #
  #   cache = RedisClient::Cache.new(max_size: 1_000)
  #   redis = RedisClient.config(cache: cache).new_client
  #
  class Cache
    # Returned by #lookup when the command isn't present in the cache.
    UNDEFINED = Object.new.freeze

    DEFAULT_MAX_SIZE = 10_000

    # Only matches the single key form, e.g. `EXISTS some-key`.
    SINGLE_KEY = ->(command) { command.size == 2 ? command[1] : nil }

    # Explicitly declared read-only, deterministic, single-key commands.
    # Maps the command name to the position of the key argument, or to a
    # callable extracting the key (returning nil when the command shape
    # isn't supported, in which case the regular code path is used).
    CACHEABLE_COMMANDS = {
      "GET" => 1,
      "GETRANGE" => 1,
      "SUBSTR" => 1,
      "STRLEN" => 1,
      "GETBIT" => 1,
      "BITCOUNT" => 1,
      "BITPOS" => 1,
      "HGET" => 1,
      "HMGET" => 1,
      "HGETALL" => 1,
      "HKEYS" => 1,
      "HVALS" => 1,
      "HLEN" => 1,
      "HEXISTS" => 1,
      "HSTRLEN" => 1,
      "LINDEX" => 1,
      "LLEN" => 1,
      "LPOS" => 1,
      "LRANGE" => 1,
      "SCARD" => 1,
      "SISMEMBER" => 1,
      "SMISMEMBER" => 1,
      "SMEMBERS" => 1,
      "ZCARD" => 1,
      "ZSCORE" => 1,
      "ZMSCORE" => 1,
      "ZCOUNT" => 1,
      "ZLEXCOUNT" => 1,
      "ZRANGE" => 1,
      "ZREVRANGE" => 1,
      "ZRANGEBYSCORE" => 1,
      "ZREVRANGEBYSCORE" => 1,
      "ZRANGEBYLEX" => 1,
      "ZREVRANGEBYLEX" => 1,
      "ZRANK" => 1,
      "ZREVRANK" => 1,
      "TYPE" => 1,
      "EXISTS" => SINGLE_KEY,
    }.freeze

    attr_reader :generation, :size

    def initialize(max_size: DEFAULT_MAX_SIZE, commands: CACHEABLE_COMMANDS)
      @max_size = max_size
      @commands = commands
      @store = {}
      @size = 0
      @generation = 0
      @inflight = Hash.new(0)
      @versions = {}
      @mutex = Mutex.new
    end

    # Returns the Redis key if the command is an explicitly declared cacheable
    # read-only command, or nil if it must go through the regular code path.
    def cacheable_key(command)
      extractor = @commands[command[0].to_s.upcase]
      case extractor
      when Integer
        command[extractor]
      when nil
        nil
      else
        extractor.call(command)
      end
    end

    # Returns the cached value for the command, or UNDEFINED.
    #
    # The returned object is owned by the cache and deeply frozen; callers
    # must not mutate it.
    def lookup(redis_key, command)
      @mutex.synchronize do
        if (entries = @store[redis_key])
          entries.fetch(command, UNDEFINED)
        else
          UNDEFINED
        end
      end
    end

    # Marks the beginning of an in-flight read for the given key and returns
    # an opaque token to be passed to #store. Must be balanced by a call to
    # #end_read.
    def begin_read(redis_key)
      @mutex.synchronize do
        @inflight[redis_key] += 1
        [@generation, @versions[redis_key] || 0].freeze
      end
    end

    # Stores the reply of an in-flight read started with #begin_read, unless
    # the key (or the whole cache) was invalidated while the read was in
    # flight, in which case the late reply is discarded.
    #
    # Returns the canonical, deeply frozen value owned by the cache.
    def store(redis_key, command, token, value)
      value = deep_freeze(value)
      @mutex.synchronize do
        fresh = token[0] == @generation && token[1] == (@versions[redis_key] || 0)
        if fresh && @max_size > 0
          entries = @store[redis_key]
          if entries.nil?
            # Evict the oldest keys first. Done before inserting the new key
            # so its entries can't be evicted from under it.
            evict! while @size >= @max_size && !@store.empty?
            entries = (@store[redis_key] = {})
          end
          @size += 1 unless entries.key?(command)
          entries[freeze_command(command)] = value
        end
      end
      value
    end

    # Balances a call to #begin_read.
    def end_read(redis_key)
      @mutex.synchronize do
        if (@inflight[redis_key] -= 1) <= 0
          @inflight.delete(redis_key)
          @versions.delete(redis_key)
        end
      end
      nil
    end

    # Processes an `invalidate` push message from the server. A nil key list
    # means a full invalidation (e.g. `FLUSHALL`), which advances the cache
    # generation; otherwise only the entries for the given keys are evicted.
    def invalidate(redis_keys)
      if redis_keys.nil?
        reset!
      else
        @mutex.synchronize do
          redis_keys.each do |key|
            if (entries = @store.delete(key))
              @size -= entries.size
            end
            # Poison reads of this key started before the invalidation, so
            # their late replies can't re-populate the cache.
            if @inflight[key] > 0
              @versions[key] = (@versions[key] || 0) + 1
            end
          end
        end
      end
      nil
    end

    # Drops all entries and advances the cache generation. Called when the
    # connection is lost and when tracking is (re)established, as
    # invalidations may have been missed in between.
    def reset!
      @mutex.synchronize do
        @store.clear
        @size = 0
        @versions.clear
        @generation += 1
      end
      nil
    end
    alias_method :clear, :reset!

    private

    def deep_freeze(value)
      case value
      when Array
        value.each { |element| deep_freeze(element) }
        value.freeze
      when Hash
        value.each do |key, element|
          deep_freeze(key)
          deep_freeze(element)
        end
        value.freeze
      when String
        value.freeze
      else
        value
      end
    end

    def freeze_command(command)
      command.map { |element| element.frozen? ? element : element.dup.freeze }.freeze
    end

    def evict!
      key, entries = @store.first
      if key
        @size -= entries.size
        @store.delete(key)
      end
    end
  end
end
