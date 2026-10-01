module GraphQL::Batch
  class Loader
    class << self
      # Maximum number of keys passed to a single {#perform} call. When more
      # unique keys accumulate, they are split into consecutive batches of at
      # most this size, in enqueue order. Defaults to +nil+, which keeps the
      # behavior of submitting every key in one batch.
      def max_batch_size(size = nil)
        if size.nil?
          if defined?(@max_batch_size)
            @max_batch_size
          elsif superclass&.respond_to?(:max_batch_size, true)
            superclass.max_batch_size
          end
        else
          validate_max_batch_size!(size)
          @max_batch_size = size
        end
      end

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

      def validate_max_batch_size!(size)
        unless size.is_a?(Integer) && size > 0
          raise ArgumentError, "max_batch_size must be a positive integer, got #{size.inspect}"
        end
      end

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
    end

    def load(key)
      cache[cache_key(key)] ||= begin
        queue << key
        ::Promise.new.tap { |promise| promise.source = self }
      end
    end

    def load_many(keys)
      ::Promise.all(keys.map { |key| load(key) })
    end

    def prime(key, value)
      cache[cache_key(key)] ||= ::Promise.resolve(value).tap { |p| p.source = self }
    end

    def resolve # :nodoc:
      return if resolved?
      load_keys = queue
      @queue = nil

      batch_size = validated_max_batch_size
      if batch_size
        load_keys.each_slice(batch_size) do |batch_keys|
          resolve_batch(batch_keys)
        end
      else
        resolve_batch(load_keys)
      end
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

    # Override per instance (or declare with {.max_batch_size}) to cap the
    # number of keys handed to {#perform} in a single call.
    def max_batch_size
      self.class.max_batch_size
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
      promise = promise_for(key)
      # When a promise is fulfilled through this class, it will either:
      #   become fulfilled, if fulfilled with a literal value
      #   become pending with a new source if fulfilled with a promise
      # Either of these is acceptable, promise.rb will automatically re-wait
      # on the new source promise as needed.
      return true if promise.fulfilled?

      promise.pending? && promise.source != self
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
      promise = promise_for(key)
      return yield(promise) unless executor
      executor.around_promise_callbacks do
        yield promise
      end
    end

    def validated_max_batch_size
      size = max_batch_size
      return nil if size.nil?

      self.class.send(:validate_max_batch_size!, size)
      size
    end

    def resolve_batch(load_keys)
      around_perform do
        perform(load_keys)
      end

      check_for_broken_promises(load_keys)
    rescue => err
      reject_pending_promises(load_keys, err)
    end

    def cache
      @cache ||= {}
    end

    def queue
      @queue ||= []
    end

    def promise_for(load_key)
      cache.fetch(cache_key(load_key))
    end

    def reject_pending_promises(load_keys, err)
      load_keys.each do |key|
        promise = promise_for(key)
        # A promise chained to another loader's promise is already considered
        # complete and must not be rolled back.
        next unless promise.pending? && promise.source == self

        reject(key, err)
      end
    end

    def check_for_broken_promises(load_keys)
      load_keys.each do |key|
        promise = promise_for(key)
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
