require 'digest/sha1'

module Rack::Cache

  # Entity stores are used to cache response bodies across requests. All
  # Implementations are required to calculate a SHA checksum of the data written
  # which becomes the response body's key.
  class EntityStore

    # Read body calculating the SHA1 checksum and size while
    # yielding each chunk to the block. If the body responds to close,
    # call it after iteration is complete. Return a two-tuple of the form:
    # [ hexdigest, size ].
    def slurp(body)
      digest, size = Digest::SHA1.new, 0
      body.each do |part|
        size += bytesize(part)
        digest << part
        yield part
      end
      body.close if body.respond_to? :close
      [digest.hexdigest, size]
    end

    if ''.respond_to?(:bytesize)
      def bytesize(string); string.bytesize; end
    else
      def bytesize(string); string.size; end
    end

    private :slurp, :bytesize

    # Record that one additional MetaStore entry now refers to the body
    # stored at +key+. Returns a truthy value when the body exists and the
    # reference was accepted; false when it has already been reclaimed and
    # the calling entry must be dropped.
    #
    # The default implementation is a no-op for backends that perform their
    # own reclamation (or none at all). Reference counting is only
    # implemented by the Heap store.
    def retain(key)
      true
    end

    # Record that one MetaStore entry stopped referring to the body stored
    # at +key+. The body is only reclaimed once the final entry reference
    # goes away and any bodies already handed out to readers have been
    # released. The default implementation is a no-op.
    def release(key)
      nil
    end


    # Stores entity bodies on the heap using a Hash object.
    #
    # Because two different MetaStore entries (e.g. two URLs serving
    # identical content) share the same body when their SHA1 digest matches,
    # the store keeps a reference count of entries for every body and a
    # count of in-progress readers. A body is reclaimed only when both
    # counts fall to zero, so entries may be replaced or invalidated without
    # corrupting shared or currently-streamed bodies.
    class Heap < EntityStore

      # Body wrapper returned by #open. Reading the wrapped chunks does not
      # touch the store, which means the backing body may be reclaimed as
      # soon as the wrapper is closed without affecting an iteration already
      # in progress. Closing the wrapper drops the in-progress reader pin.
      class Body
        include Enumerable

        def initialize(chunks, &on_close)
          @chunks = chunks
          @on_close = on_close
          @closed = false
        end

        def each
          @chunks.each { |part| yield part }
        end

        def join
          @chunks.join
        end

        def to_a
          @chunks.dup
        end

        def close
          return if @closed
          @closed = true
          @on_close.call
        end

        def closed?
          @closed
        end
      end

      # Create the store with the specified backing Hash.
      def initialize(hash={}, options = {})
        @hash = hash
        @options = options
        # Number of committed MetaStore entries referring to each body.
        @references = Hash.new(0)
        # Number of bodies handed out by #open that have not been closed.
        @readers = Hash.new(0)
        @mutex = Mutex.new
      end

      # Determine whether the response body with the specified key (SHA1)
      # exists in the store.
      def exist?(key)
        @hash.include?(key)
      end

      # Return an object suitable for use as a Rack response body for the
      # specified key. The returned body remains readable even if the last
      # MetaStore entry referencing it is removed before it is closed; the
      # underlying body is only reclaimed after the body is closed.
      def open(key)
        @mutex.synchronize do
          return nil unless @hash.include?(key)
          @readers[key] += 1
          Body.new(@hash[key]) { close_reader(key) }
        end
      end

      # Read all data associated with the given key and return as a single
      # String.
      def read(key)
        @mutex.synchronize do
          (body = @hash[key]) && body.join
        end
      end

      # Write the Rack response body immediately and return the SHA1 key.
      # The body is written with a tentative reference count of zero; the
      # matching MetaStore entry claims it with #retain when it commits. If
      # that commit never happens, #purge_orphan reclaims it.
      def write(body, ttl=nil)
        buf = []
        key, size = slurp(body) { |part| buf << part }
        @mutex.synchronize do
          @hash[key] = buf unless @hash.include?(key)
        end
        [key, size]
      end

      # Remove the body corresponding to key regardless of references;
      # return nil. Used when the caller knows the body is no longer needed
      # (e.g. test cleanup). In-progress readers keep their copy and the
      # last close becomes a no-op.
      def purge(key)
        @mutex.synchronize do
          @hash.delete(key)
          @references.delete(key)
        end
        nil
      end

      # Accept a reference from a committing MetaStore entry. Returns false
      # when the body no longer exists, signalling that the entry must not
      # be committed.
      def retain(key)
        @mutex.synchronize do
          return false unless @hash.include?(key)
          @references[key] += 1
          true
        end
      end

      # Drop a reference held by a committed MetaStore entry. Reclaims the
      # body immediately when no entries or readers remain, otherwise waits
      # for the last reader (#invalidate does not release entries, so stale
      # entries keep their body referenced).
      def release(key)
        @mutex.synchronize do
          next unless @references.key?(key)
          count = (@references[key] -= 1)
          if count <= 0
            @references.delete(key)
            reclaim_locked(key)
          end
        end
        nil
      end

      # Reclaim a body written by #write whose MetaStore entry was never
      # committed. Does nothing while readers still hold the body or while a
      # committed entry references it.
      def purge_orphan(key)
        @mutex.synchronize do
          reclaim_locked(key) unless @references.key?(key)
        end
        nil
      end

      def self.resolve(uri, options = {})
        new({}, options)
      end

      private

      # Drop a reader pin registered in #open and reclaim the body if it was
      # the last thing keeping it alive. Safe to call after the body was
      # force removed by #purge.
      def close_reader(key)
        @mutex.synchronize do
          if @readers.key?(key)
            count = @readers[key] - 1
            if count <= 0
              @readers.delete(key)
            else
              @readers[key] = count
            end
          end
          reclaim_locked(key) unless @references.key?(key)
        end
      end

      # Delete the body when no committed entry references it and no reader
      # is streaming it. Must be called with @mutex held.
      def reclaim_locked(key)
        if @references[key].to_i <= 0 && @readers[key].to_i <= 0
          @hash.delete(key)
          @references.delete(key)
          @readers.delete(key)
        end
      end

    end

    HEAP = Heap
    MEM  = Heap

    # Stores entity bodies on disk at the specified path.
    class Disk < EntityStore

      # Path where entities should be stored. This directory is
      # created the first time the store is instansiated if it does not
      # already exist.
      attr_reader :root

      def initialize(root)
        @root = root
        FileUtils.mkdir_p root, :mode => 0755
      end

      def exist?(key)
        File.exist?(body_path(key))
      end

      def read(key)
        File.open(body_path(key), 'rb') { |f| f.read }
      rescue Errno::ENOENT
        nil
      end

      class Body < ::File #:nodoc:
        def each
          while part = read(8192)
            yield part
          end
        end
        alias_method :to_path, :path
      end

      # Open the entity body and return an IO object. The IO object's
      # each method is overridden to read 8K chunks instead of lines.
      def open(key)
        Body.open(body_path(key), 'rb')
      rescue Errno::ENOENT
        nil
      end

      def write(body, ttl=nil)
        filename = ['buf', $$, Thread.current.object_id].join('-')
        temp_file = storage_path(filename)
        key, size =
          File.open(temp_file, 'wb') { |dest|
            slurp(body) { |part| dest.write(part) }
          }

        path = body_path(key)
        if File.exist?(path)
          File.unlink temp_file
        else
          FileUtils.mkdir_p File.dirname(path), :mode => 0755
          FileUtils.mv temp_file, path
        end
        [key, size]
      end

      def purge(key)
        File.unlink body_path(key)
        nil
      rescue Errno::ENOENT
        nil
      end

    protected
      def storage_path(stem)
        File.join root, stem
      end

      def spread(key)
        key = key.dup
        key[2,0] = '/'
        key
      end

      def body_path(key)
        storage_path spread(key)
      end

      def self.resolve(uri)
        path = File.expand_path(uri.opaque || uri.path)
        new path
      end
    end

    DISK = Disk
    FILE = Disk

    # Base class for memcached entity stores.
    class MemCacheBase < EntityStore
      # The underlying Memcached instance used to communicate with the
      # memcached daemon.
      attr_reader :cache

      extend Rack::Utils

      def open(key)
        data = read(key)
        data && [data]
      end

      def self.resolve(uri)
        if uri.respond_to?(:scheme)
          server = "#{uri.host}:#{uri.port || '11211'}"
          options = parse_query(uri.query)
          options.keys.each do |key|
            value =
              case value = options.delete(key)
              when 'true' ; true
              when 'false' ; false
              else value.to_sym
              end
            options[key.to_sym] = value
          end
          options[:namespace] = uri.path.sub(/^\//, '')
          new server, options
        else
          # if the object provided is not a URI, pass it straight through
          # to the underlying implementation.
          new uri
        end
      end
    end

    # Uses the Dalli ruby library. This is the default unless
    # the memcached library has already been required.
    class Dalli < MemCacheBase
      def initialize(server="localhost:11211", options={})
        @cache =
          if server.respond_to?(:stats)
            server
          else
            require 'dalli'
            ::Dalli::Client.new(server, options)
          end
      end

      def exist?(key)
        !cache.get(key).nil?
      end

      def read(key)
        data = cache.get(key)
        data.force_encoding('BINARY') if data.respond_to?(:force_encoding)
        data
      end

      def write(body, ttl=nil)
        buf = StringIO.new
        key, size = slurp(body){|part| buf.write(part) }
        [key, size] if cache.set(key, buf.string, ttl)
      end

      def purge(key)
        cache.delete(key)
        nil
      end
    end

    # Uses the memcached client library. The ruby based memcache-client is used
    # in preference to this store unless the memcached library has already been
    # required.
    class MemCached < MemCacheBase
      def initialize(server="localhost:11211", options={})
        options[:prefix_key] ||= options.delete(:namespace) if options.key?(:namespace)
        @cache =
          if server.respond_to?(:stats)
            server
          else
            require 'memcached'
            ::Memcached.new(server, options)
          end
      end

      def exist?(key)
        cache.append(key, '')
        true
      rescue ::Memcached::NotStored
        false
      end

      def read(key)
        cache.get(key, false)
      rescue ::Memcached::NotFound
        nil
      end

      def write(body, ttl=0)
        buf = StringIO.new
        key, size = slurp(body){|part| buf.write(part) }
        cache.set(key, buf.string, ttl, false)
        [key, size]
      end

      def purge(key)
        cache.delete(key)
        nil
      rescue ::Memcached::NotFound
        nil
      end
    end

    MEMCACHE = Dalli
    MEMCACHED = MEMCACHE

    class GAEStore < EntityStore
      attr_reader :cache

      def initialize(options = {})
        require 'rack/cache/app_engine'
        @cache = Rack::Cache::AppEngine::MemCache.new(options)
      end

      def exist?(key)
        cache.contains?(key)
      end

      def read(key)
        cache.get(key)
      end

      def open(key)
        if data = read(key)
          [data]
        else
          nil
        end
      end

      def write(body, ttl=nil)
        buf = StringIO.new
        key, size = slurp(body){|part| buf.write(part) }
        cache.put(key, buf.string, ttl)
        [key, size]
      end

      def purge(key)
        cache.delete(key)
        nil
      end

      def self.resolve(uri)
        self.new(:namespace => uri.host)
      end

    end

    GAECACHE = GAEStore
    GAE = GAEStore

    # Noop Entity Store backend.
    #
    # Set `entitystore` to 'noop:/'.
    # Does not persist response bodies (no disk/memory used).
    # Responses from the cache will have an empty body.
    # Clients must ignore these empty cached response (check for x-rack-cache response header).
    # Atm cannot handle streamed responses, patch needed.
    #
    class Noop < EntityStore
      def exist?(key)
        true
      end

      def read(key)
        ''
      end

      def open(key)
        []
      end

      def write(body, ttl=nil)
        key, size = slurp(body) { |part| part }
        [key, size]
      end

      def purge(key)
        nil
      end

      def self.resolve(uri)
        new
      end
    end

    NOOP = Noop
  end

end
