module GraphQL::Batch
  class Loader
    class RequestPromise < ::Promise
      attr_reader :loader, :load_key

      def initialize(loader = nil, load_key = nil)
        @loader = loader
        @load_key = load_key
        super()
      end

      def wait
        return super unless pending?

        tracked = false

        begin
          tracked = Loader.begin_wait(self)
          super
        ensure
          Loader.end_wait(self) if tracked
        end
      end

      def fulfill(value = nil)
        cycle = Loader.cycle_requests_for_link(self, value) || dependency_cycle_for(value)
        raise DependencyCycleError.for(cycle) if cycle

        super
      end

      private

      def dependency_cycle_for(value)
        return unless pending? && ::Promise === value && value.pending?

        requests = [self]
        current = value

        while ::Promise === current && current.pending?
          requests << current if current.is_a?(RequestPromise) && current.loader
          break if current.equal?(self)

          current = current.source
        end

        return unless current.equal?(self)

        requests
      end
    end

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
    end

    def load(key)
      cache[cache_key(key)] ||= begin
        promise = RequestPromise.new(self, key)
        promise.source = self
        queue << promise
        promise
      end
    end

    def load_many(keys)
      ::Promise.all(keys.map { |key| load(key) })
    end

    def prime(key, value)
      cache[cache_key(key)] ||= ::Promise.resolve(value).tap { |promise| promise.source = self }
    end

    def resolve(load_promises = nil) # :nodoc:
      return if resolved?
      load_promises ||= queue
      @queue = nil
      Loader.begin_resolve(self, load_promises)

      begin
        around_perform do
          perform(load_promises.map(&:load_key))
        end

        check_for_broken_promises(load_promises)
      ensure
        Loader.end_resolve(self, load_promises)
    end
    rescue => err
      reject_pending_promises(load_promises, err)
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
      finish_resolve(promise_for(key)) do |promise|
        promise.fulfill(value)
      end
    end

    def reject(key, reason)
      finish_resolve(promise_for(key)) do |promise|
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

    def finish_resolve(promise)
      return yield(promise) unless executor
      executor.around_promise_callbacks do
        yield promise
      end
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

    def reject_pending_promises(load_promises, err)
      load_promises.each do |promise|
        next unless promise.pending? && promise.source == self

        finish_resolve(promise) do
          promise.reject(err)
        end
      end
    end

    def check_for_broken_promises(load_promises)
      load_promises.each do |promise|
        # When a promise is fulfilled through this class, it will either:
        #   become not pending, if fulfilled with a literal value
        #   become pending with a new source if fulfilled with a promise
        # Either of these is acceptable, promise.rb will automatically re-wait
        # on the new source promise as needed.
        next unless promise.pending? && promise.source == self

        key = promise.load_key
        finish_resolve(promise) do
          promise.reject(::Promise::BrokenError.new("#{self.class} didn't fulfill promise for key #{key.inspect}"))
        end
      end
    end

    class << self
      def begin_wait(request)
        return false unless tracked_wait?(request)

        cycle_requests = cycle_requests_for_wait(request)
        raise DependencyCycleError.for(cycle_requests) if cycle_requests

        active_dependencies.push([:wait, request])
        true
      end

      def end_wait(request)
        index = active_dependencies.rindex { |type, entry| type == :wait && entry.equal?(request) }
        active_dependencies.delete_at(index) if index
      end

      def cycle_requests_for_link(request, value)
        return unless value.is_a?(RequestPromise) && value.loader

        active_dependencies.each_with_index do |frame, index|
          next unless frame[0] == :resolve
          next unless frame[1].equal?(value.loader)
          next unless value.pending? && value.source.equal?(frame[1])
          next unless frame[2].any? { |entry| entry.equal?(value) }

          entries = active_dependencies[index..]
          next unless active_chain_ends_with?(entries, request)

          return requests_for_cycle(entries, value)
        end

        nil
      end

      def begin_resolve(loader, requests)
        trigger_index = active_dependencies.rindex do |type, entry|
          type == :wait && entry.loader.equal?(loader)
        end
        trigger = trigger_index && active_dependencies.delete_at(trigger_index)[1]
        active_dependencies.push([:resolve, loader, requests, trigger])
      end

      def end_resolve(loader, requests)
        index = active_dependencies.rindex do |type, entry_loader, entry_requests|
          type == :resolve &&
            entry_loader.equal?(loader) &&
          entry_requests.equal?(requests)
        end
        return unless index

        active_dependencies.delete_at(index)
      end

      private

      def tracked_wait?(request)
        request.pending? && request.loader
      end

      def active_dependencies
        Thread.current[ACTIVE_DEPENDENCIES_THREAD_KEY] ||= []
      end

      def active_chain_ends_with?(entries, request)
        entries.reverse_each do |type, _loader, _requests, trigger|
          return trigger.equal?(request) if type == :resolve && trigger
        end

        false
      end

      def cycle_requests_for_wait(request)
        active_dependencies.reverse.each_with_index do |entry, reverse_index|
          index = active_dependencies.length - 1 - reverse_index

          if entry[0] == :wait
            return requests_for_cycle(active_dependencies[index..], request) if entry[1].equal?(request)
          elsif active_frame_includes_request?(entry, request)
            return requests_for_cycle(active_dependencies[index..], request)
          end
        end

        nil
      end

      def active_frame_includes_request?(frame, request)
        matching_request = frame[2].find { |entry_request| entry_request.equal?(request) }
        frame_index = active_dependencies.each_index.find { |index| active_dependencies[index].equal?(frame) }
        nested_entries = active_dependencies[(frame_index + 1)..] if frame_index
        return false unless matching_request
        return false unless frame[1].equal?(matching_request.loader)

        !frame[3].equal?(request) || nested_entries.any? { |entry| entry[0] == :resolve } ||
          frame[2].length > 1
      end

      def requests_for_cycle(entries, target = nil)
        requests = []

        entries.each do |entry|
          request = entry[0] == :wait ? entry[1] : entry[3]
          next unless request
          requests << request
        end

        first_entry = entries.first
        if target && first_entry && first_entry[0] == :resolve &&
            first_entry[2].any? { |request| request.equal?(target) }
          requests << target unless requests.last&.equal?(target)
          closure = first_entry[3] || target
          requests << closure unless requests.last&.equal?(closure)
        elsif target && (requests.empty? || !requests.last.equal?(target))
          requests << target
        end

        requests
      end

      ACTIVE_DEPENDENCIES_THREAD_KEY = :"#{name}.active_dependencies"
      private_constant :ACTIVE_DEPENDENCIES_THREAD_KEY
    end
  end
end
