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

    # Whether this store keeps a reference count for the bodies it holds and
    # can cooperate with a paired MetaStore to reclaim a body only once the
    # last cache entry referencing it has been removed. Backends without
    # reference management return false so callers can skip the bookkeeping.
    def reference_managed?
      false
    end

    # Record that one additional cache entry references the body stored under
    # +key+. Returns true when the referenced body exists in the store and
    # false otherwise (a false result must not leave a dangling reference
    # behind). Reference-managed backends override this; others treat the
    # calls as no-ops so a MetaStore may always issue them.
    def retain(key)
      false
    end

    # Drop one reference previously added with #retain for +key+. The body is
    # reclaimed only when no references and no in-flight readers remain.
    # Returns true when the body was actually removed by this call.
    def release(key)
      false
    end

    # Bodies handed out by #open may still be iterated after the cache entry
    # that referenced them is evicted. #open_reader pins the body until the
    # returned object is closed (or garbage collected), keeping such a read
    # valid even when the body is concurrently reclaimed.
    def open_reader(key)
      nil
    end

    # Release a reader pin previously obtained from #open_reader.
    def release_reader(key, reader)
      nil
    end

    # Return an independent, re-readable Rack body containing the data stored
    # under +key+ without registering a reader pin. This is used when a body
    # must be read back right after writing (e.g. to answer the request that
    # caused a one-shot stream body to be cached): the caller's own cache
    # entry already references the entity, so an extra in-flight pin would be
    # pointless and could defer reclamation. Returns nil when the key does
    # not exist. Backends without a cheap copy fall back to #open.
    def copy(key)
      open(key)
    end


    # Stores entity bodies on the heap using a Hash object.
    #
    # In addition to the body Hash, this implementation maintains a reference
    # count for every stored body. A paired in-memory MetaStore uses #retain
    # and #release while it adds, replaces, invalidates or evicts entries. A
    # body is reclaimed at exactly the moment its last reference disappears,
    # except that bodies currently handed out to readers are kept alive until
    # those reads finish.
    class Heap < EntityStore

      # Rack compatible body wrapper returned by #open. It carries a reader
      # pin for the underlying entity so the body can safely be iterated even
      # if the cache entry (and the body) is evicted mid-read. The pin is
      # released either by an explicit #close or when the wrapper is garbage
      # collected.
      class Body
        include Enumerable

        def initialize(chunks, pin)
          @chunks = chunks
          @pin = pin
          @closed = false
          # Finalizers run in a trap context where a Mutex cannot be locked,
          # so they only push the pin onto a Queue (push is signal safe).
          # The store drains the queue on every locked operation.
          ObjectSpace.define_finalizer(self, self.class.finalize(pin))
        end

        def each(&block)
          @chunks.each(&block)
        end

        def close
          return if @closed
          @closed = true
          @pin.close
          nil
        end

        def to_a
          @chunks.dup
        end

        def self.finalize(pin)
          proc { pin.abandon }
        end
      end

      # Private holder of a single reader pin. Releasing it unregisters the
      # pin exactly once whether triggered by Body#close or by the garbage
      # collector. An abandoned (GC'd without #close) pin is queued for the
      # store to reap, since finalizers cannot take the store's Mutex.
      class ReaderPin
        def initialize(store, key, abandoned)
          @store = store
          @key = key
          @abandoned = abandoned
          @released = false
        end

        # Invoked from Body#close (ordinary context): unregister immediately.
        def close
          return if @released
          @released = true
          @store.release_reader(@key, self)
          nil
        end

        # Invoked from the Body finalizer (trap context): never takes a
        # lock, just hands the pin to the store's reaper queue.
        def abandon
          return if @released
          @released = true
          @abandoned << [@key, self]
          nil
        end
      end

      # Create the store with the specified backing Hash.
      def initialize(hash={}, options = {})
        @hash = hash
        @options = options
        # Number of MetaStore entries referencing each body key.
        @refs = Hash.new(0)
        # ReaderPin objects currently held by open Body wrappers, keyed by body.
        @readers = Hash.new { |h, k| h[k] = {} }
        @mutex = Mutex.new
        # Pins abandoned by garbage collected Body wrappers.
        @abandoned = Queue.new
      end

      # Determine whether the response body with the specified key (SHA1)
      # exists in the store.
      def exist?(key)
        synchronize { @hash.include?(key) }
      end

      # Return an object suitable for use as a Rack response body for the
      # specified key. The returned object stays readable until it is closed
      # or garbage collected, even if the body is reclaimed meanwhile.
      def open(key)
        pin, chunks = nil
        synchronize do
          return nil unless @hash.include?(key)
          pin = pin_reader(key)
          chunks = @hash[key].dup
        end
        Body.new(chunks, pin)
      rescue
        pin.close if pin
        raise
      end

      # Read all data associated with the given key and return as a single
      # String. The data is copied while holding the lock so no reader pin is
      # required.
      def read(key)
        synchronize do
          (body = @hash[key]) && body.join
        end
      end

      # Write the Rack response body immediately and return the SHA1 key.
      def write(body, ttl=nil)
        buf = []
        key, size = slurp(body) { |part| buf << part }
        synchronize do
          # Never clobber an existing body: identical content has the same
          # SHA1 key and must remain shared (and keep its references).
          @hash[key] = buf unless @hash.include?(key)
        end
        [key, size]
      end

      # Unconditionally remove the body corresponding to key; return nil.
      # Outstanding readers keep their private copies and can finish reading;
      # their eventual pin releases are ignored and clean up the pin state.
      def purge(key)
        synchronize do
          @hash.delete(key)
          @refs.delete(key)
          # @readers is left intact until the last reader releases its pin.
        end
        nil
      end

      # This Heap backend performs reference accounting for its bodies.
      def reference_managed?
        true
      end

      # Add one reference to the body at +key+. Returns false (without
      # touching the reference count) when the body is not present so no
      # dangling reference can be created.
      def retain(key)
        synchronize do
          return false unless @hash.include?(key)
          @refs[key] += 1
          true
        end
      end

      # Drop one reference to the body at +key+. The body is reclaimed as
      # soon as both the reference count reaches zero and no reader is
      # iterating it. While a reader is still in flight the body is kept
      # (with a zero reference count) and the last #release_reader call
      # performs the reclamation. Returns true when this call removed the
      # body.
      def release(key)
        synchronize do
          next false unless @hash.include?(key)
          count = (@refs[key] -= 1)
          if count <= 0 && @readers[key].empty?
            delete_body(key)
            next true
          end
          false
        end
      end

      # Number of MetaStore entries currently referencing +key+.
      def refcount(key)
        synchronize { @refs[key] }
      end

      # Number of open Body wrappers currently reading +key+.
      def readers(key)
        synchronize { @readers[key].size }
      end

      # Return an independent chunk Array with the body data. No reader pin
      # is created: callers using #copy hold their own reference (typically
      # the cache entry being committed) and the data lives entirely on the
      # heap, so the copy stays valid independently of later reclamation.
      def copy(key)
        synchronize do
          return nil unless @hash.include?(key)
          @hash[key].dup
        end
      end

      def open_reader(key)
        synchronize do
          return nil unless @hash.include?(key)
          pin_reader(key)
        end
      end

      def release_reader(key, reader)
        synchronize do
          unregister_reader(key, reader)
        end
      end

      def self.resolve(uri, options = {})
        new({}, options)
      end

      private

      # Run a block under the store lock after reaping pins abandoned by
      # garbage collected Body wrappers. All public state changes go through
      # here so reclamation driven by GC is never delayed for long and
      # finalizer code never has to take a lock itself.
      def synchronize
        @mutex.synchronize do
          reap_abandoned
          yield
        end
      end

      # Drain the abandoned-pin queue. Must be called while holding @mutex.
      def reap_abandoned
        until @abandoned.empty?
          begin
            key, pin = @abandoned.pop(true)
          rescue ThreadError
            break
          end
          unregister_reader(key, pin)
        end
      end

      # Remove one reader pin and reclaim the body when it was the last
      # reader and the reference count is zero. Must be called under @mutex.
      def unregister_reader(key, reader)
        active = @readers[key]
        existed = active.delete(reader)
        if active.empty?
          @readers.delete(key)
          if @hash.include?(key) && @refs[key] <= 0
            delete_body(key)
          end
        end
        existed ? true : false
      end

      # Must be called while holding @mutex. Removes every trace of +key+.
      def delete_body(key)
        @hash.delete(key)
        @refs.delete(key)
        @readers.delete(key)
      end

      # Must be called while holding @mutex. Registers a new reader pin for
      # +key+ and returns it.
      def pin_reader(key)
        pin = ReaderPin.new(self, key, @abandoned)
        @readers[key][pin] = true
        pin
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
