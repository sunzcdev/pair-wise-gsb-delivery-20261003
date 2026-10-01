module GraphQL::Batch
  class Loader
    class << self
      def for(*group_args, **group_kwargs)
        current_executor.loader(loader_key_for(*group_args, **group_kwargs)) do
          new(*group_args, **group_kwargs)
        end
      end

      def loader_key_for(*group_args, **group_kwargs)
        [self, group_kwargs, group_args]
      end

      def load(key)
        self.for.load(key)
      end

      def load_many(keys)
        self.for.load_many(keys)
      end

      private

      def current_executor
        executor = Executor.current

        unless executor
          raise GraphQL::Batch::NoExecutorError, 'Cannot create loader without'\
            ' an Executor. Wrap the call to `for` with `GraphQL::Batch.batch`'\
            ' or use `GraphQL::Batch::SetupMultiplex` as a query instrumenter if'\
            ' using with `graphql-ruby`'
        end

        executor
      end
    end

    attr_accessor :loader_key, :executor

    def initialize
      @loader_key = nil
      @executor = nil
      @queue = nil
      @cache = nil
      @resolving_promises = nil
    end

    def load(key)
      cache[cache_key(key)] ||= begin
        promise = ::Promise.new.tap { |promise| promise.source = self }
        queue << [key, promise]
        promise
      end
    end

    def load_many(keys)
      ::Promise.all(keys.map { |key| load(key) })
    end

    # Clear the cached entry for the given key so that subsequent calls to
    # #load queue a fresh load instead of reusing the previous result.
    #
    # Clearing only affects loads started after the clear. Promises that were
    # already handed out are still settled by the batch they belong to, even
    # if that batch fulfills or rejects the key after the clear. A late
    # fulfill or reject from the previous batch never writes into the promise
    # of the new generation.
    #
    # The key is normalized through #cache_key, so any load key that maps to
    # the same cache entry invalidates it.
    def clear(key)
      cache.delete(cache_key(key))
      nil
    end

    # Prime the cache with a value for the given key.
    #
    # Priming never overwrites an existing cache entry: if the key was
    # already loaded, primed, or is currently queued for loading, the prime
    # is a no-op and the existing promise is settled by its own batch. Call
    # #clear first to invalidate the existing entry before priming a
    # replacement value.
    def prime(key, value)
      cache[cache_key(key)] ||= ::Promise.resolve(value).tap { |p| p.source = self }
    end

    def resolve # :nodoc:
      return if resolved?
      queued = queue
      @queue = nil
      load_keys = queued.map(&:first).uniq

      # Remember which promises belong to the batch being resolved so that
      # fulfill/reject calls settle them, even if the cache entry for a key
      # was cleared (and possibly reloaded) while #perform was running.
      previous_resolving = @resolving_promises
      @resolving_promises = {}
      queued.each do |key, promise|
        (@resolving_promises[cache_key(key)] ||= []) << promise
      end

      begin
        around_perform do
          perform(load_keys)
        end

        check_for_broken_promises(load_keys)
      rescue => err
        reject_pending_promises(load_keys, err)
      ensure
        @resolving_promises = previous_resolving
      end
    end

    # Interface to add custom code for purposes such as instrumenting the performance of the loader.
    def around_perform
      yield
    end

    # For Promise#sync
    def wait # :nodoc:
      if executor
        executor.resolve(self)
      else
        resolve
      end
    end

    def resolved?
      @queue.nil? || @queue.empty?
    end

    protected

    # Fulfill the key with provided value, for use in #perform
    def fulfill(key, value)
      finish_resolve(promises_for(key)) do |promise|
        promise.fulfill(value)
      end
    end

    def reject(key, reason)
      finish_resolve(promises_for(key)) do |promise|
        promise.reject(reason)
      end
    end

    # Returns true when the key has already been fulfilled, otherwise returns false
    def fulfilled?(key)
      # When a promise is fulfilled through this class, it will either:
      #   become fulfilled, if fulfilled with a literal value
      #   become pending with a new source if fulfilled with a promise
      # Either of these is acceptable, promise.rb will automatically re-wait
      # on the new source promise as needed.
      promises_for(key).all? do |promise|
        promise.fulfilled? || (promise.pending? && promise.source != self)
      end
    end

    # Must override to load the keys and call #fulfill for each key
    def perform(keys)
      raise NotImplementedError
    end

    # Override to use a different key for the cache than the load key
    def cache_key(load_key)
      load_key
    end

    private

    def finish_resolve(promises)
      return promises.each { |promise| yield promise } unless executor
      executor.around_promise_callbacks do
        promises.each { |promise| yield promise }
      end
    end

    def cache
      @cache ||= {}
    end

    def queue
      @queue ||= []
    end

    # Promises that #fulfill and #reject currently settle for the given load
    # key. While a batch is resolving, these are the promises queued for that
    # batch rather than whatever the cache holds now, so a clear (and reload)
    # during #perform cannot redirect the previous batch's settlement into
    # the new generation.
    def promises_for(load_key)
      key = cache_key(load_key)
      promises = @resolving_promises&.fetch(key, nil)
      promises || [cache.fetch(key)]
    end

    def reject_pending_promises(load_keys, err)
      load_keys.each do |key|
        next if promises_for(key).none?(&:pending?)

        reject(key, err)
      end
    end

    def check_for_broken_promises(load_keys)
      load_keys.each do |key|
        # When a promise is fulfilled through this class, it will either:
        #   become not pending, if fulfilled with a literal value
        #   become pending with a new source if fulfilled with a promise
        # Either of these is acceptable, promise.rb will automatically re-wait
        # on the new source promise as needed.
        promises_for(key).each do |promise|
          next unless promise.pending? && promise.source == self

          finish_resolve([promise]) do |broken_promise|
            broken_promise.reject(::Promise::BrokenError.new("#{self.class} didn't fulfill promise for key #{key.inspect}"))
          end
        end
      end
    end
  end
end
