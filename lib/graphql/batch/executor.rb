module GraphQL::Batch
  class Executor
    THREAD_KEY = :"#{name}.batched_queries"
    PARENT_THREAD_KEY = :"#{name}.parent_batched_queries"
    private_constant :THREAD_KEY, :PARENT_THREAD_KEY

    class << self
      def current
        Thread.current[THREAD_KEY]
      end

      def current=(executor)
        Thread.current[THREAD_KEY] = executor
      end

      def start_batch(executor_class)
        executor = Thread.current[THREAD_KEY] ||= executor_class.new
        executor.increment_level
      end

      def end_batch
        executor = current
        unless executor
          raise NoExecutorError, 'Cannot end a batch without an Executor.'
        end
        return unless executor.decrement_level < 1
        self.current = nil
      end

      # Install a fresh executor, saving the current one (if any) so that it can
      # be restored by .end_isolated_batch. The isolated executor has its own
      # loader registry and load queue and starts at the first nesting level.
      def start_isolated_batch(executor_class)
        Thread.current[PARENT_THREAD_KEY] = [current, *Thread.current[PARENT_THREAD_KEY]]
        self.current = executor_class.new
        current.increment_level
      end

      def end_isolated_batch
        parent_stack = Thread.current[PARENT_THREAD_KEY]
        parent_executor = parent_stack&.first
        unless current
          raise NoExecutorError, 'Cannot end a batch without an Executor.'
        end
        rest = parent_stack ? parent_stack.drop(1) : []
        Thread.current[PARENT_THREAD_KEY] = rest.empty? ? nil : rest
        self.current = parent_executor
      end
    end

    # Set to true when performing a batch query, otherwise, it is false.
    #
    # Can be used to detect unbatched queries in an ActiveSupport::Notifications.subscribe block.
    attr_reader :loading

    def initialize
      @loaders = {}
      @loading = false
      @nesting_level = 0
    end

    def loader(key)
      @loaders[key] ||= yield.tap do |loader|
        loader.executor = self
        loader.loader_key = key
      end
    end

    def resolve(loader)
      was_loading = @loading
      @loading = true
      loader.resolve
    ensure
      @loading = was_loading
    end

    def tick
      resolve(@loaders.shift.last)
    end

    def wait_all
      tick until @loaders.empty?
    end

    def clear
      @loaders.clear
    end

    def increment_level
      @nesting_level += 1
    end

    def decrement_level
      @nesting_level -= 1
    end

    def around_promise_callbacks
      # We need to set #loading to false so that any queries that happen in the promise
      # callback aren't interpreted as being performed in GraphQL::Batch::Loader#perform
      was_loading = @loading
      @loading = false
      yield
    ensure
      @loading = was_loading
    end
  end
end
