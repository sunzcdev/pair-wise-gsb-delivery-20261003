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
      @queued = nil
      @resolving = nil
    end

    def load(key)
      cache_key = cache_key(key)
      cache[cache_key] ||= begin
        queue << key
        ::Promise.new.tap do |promise|
          promise.source = self
          (queued[cache_key] ||= []) << promise
        end
      end
    end

    def load_many(keys)
      ::Promise.all(keys.map { |key| load(key) })
    end

    # Clear the cached promise for +key+, so that subsequent calls to #load
    # return a new promise and queue the key to be loaded again.
    #
    # Promises already returned by #load are not affected: they are still
    # resolved by the batch they were queued in, even if that batch is
    # currently performing. A late #fulfill or #reject from that batch is
    # attributed to those promises only, never to the new generation.
    #
    # The key is normalized with #cache_key, so load keys that map to the
    # same cache key share the same invalidation.
    def clear(key)
      cache.delete(cache_key(key))
    end

    # Prime the cache with a value for +key+, unless an entry already
    # exists. An existing entry is never overwritten, even if it is a
    # still-pending promise from a previous #load; call #clear first to
    # replace an existing entry.
    def prime(key, value)
      cache[cache_key(key)] ||= ::Promise.resolve(value).tap { |p| p.source = self }
    end

    def resolve # :nodoc:
      return if resolved?
      load_keys = queue.uniq
      @queue = nil
      @resolving = @queued
      @queued = nil

      around_perform do
        perform(load_keys)
      end

      check_for_broken_promises(load_keys)
    rescue => err
      reject_pending_promises(load_keys, err)
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
      finish_resolve(key) do |promise|
        promise.fulfill(value)
      end
    end

    def reject(key, reason)
      finish_resolve(key) do |promise|
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

    def finish_resolve(key)
      promises = promises_for(key)
      return promises.each { |promise| yield(promise) } unless executor
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

    def queued
      @queued ||= {}
    end

    # Promises that a #fulfill or #reject for +load_key+ should settle.
    #
    # While a batch is resolving, these are the promises that were queued
    # for that batch, looked up by cache key. They may no longer be cached
    # if #clear was called for the key (e.g. from within #perform), and a
    # promise cached for a later generation must not be settled by this
    # batch.
    def promises_for(load_key)
      cache_key = cache_key(load_key)
      if @resolving && @resolving.key?(cache_key)
        @resolving[cache_key]
      else
        [cache.fetch(cache_key)]
      end
    end

    def reject_pending_promises(load_keys, err)
      load_keys.each do |key|
        next unless promises_for(key).any?(&:pending?)

        reject(key, err)
      end
    end

    def check_for_broken_promises(load_keys)
      load_keys.each do |key|
        promises_for(key).each do |promise|
          # When a promise is fulfilled through this class, it will either:
          #   become not pending, if fulfilled with a literal value
          #   become pending with a new source if fulfilled with a promise
          # Either of these is acceptable, promise.rb will automatically re-wait
          # on the new source promise as needed.
          next unless promise.pending? && promise.source == self

          reject(key, ::Promise::BrokenError.new("#{self.class} didn't fulfill promise for key #{key.inspect}"))
        end
      end
    end
  end
end
