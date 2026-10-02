module GraphQL::Batch
  class DependencyCycleError < StandardError
    attr_reader :chain, :loaders, :keys

    def initialize(chain)
      @chain = chain.map(&:dup).freeze
      @loaders = chain.map(&:first).freeze
      @keys = chain.map(&:last).freeze
      super("Loader dependency cycle detected: #{DependencyCycle.format_chain(chain)}")
    end
  end

  module LoaderPromise
    attr_accessor :loader, :key

    def unresolved_loader_request?
      pending? && @source.equal?(loader)
    end

    def graphql_batch_dependency_promises
      source = @source
      return [] if source.nil? || source.equal?(loader) || source.is_a?(Loader)
      return [source] if source.is_a?(::Promise) || ::Promise.const_get(:Group) === source

      []
    end
  end

  module DependencyCycle
    WAIT_STACK_KEY = :"#{name}.wait_stack"
    ACTIVE_RESOLVES_KEY = :"#{name}.active_resolves"
    private_constant :WAIT_STACK_KEY
    private_constant :ACTIVE_RESOLVES_KEY

    class << self
      def begin_resolve(requests)
        active_resolves << {
          requests: requests,
          starter: wait_stack.last,
        }
      end

      def end_resolve
        active_resolves.pop
      end

      def waiting(promise)
        tracked = track_wait(promise)
        yield
      ensure
        wait_stack.pop if tracked && wait_stack.last.equal?(promise)
      end

      def adding_dependency(request, value)
        return unless request.is_a?(LoaderPromise) &&
          request.pending? &&
          value.is_a?(::Promise) &&
          value.pending?

        path = find_cycle_path(value, request)
        if path
          requests = rotate_to_waiting_request(ordered_path(path, request))
          reject_cycle(requests)
        end
      end

      def format_chain(chain)
        chain.map do |loader, key|
          "#{safe_loader_name(loader)}[#{safe_key(key)}]"
        end.join(" -> ")
      end

      private

      def track_wait(promise)
        return false unless promise.is_a?(LoaderPromise) && promise.pending?

        repeated_index = wait_stack.index { |waiting| waiting.equal?(promise) }
        if repeated_index
          reject_cycle(wait_stack[repeated_index..] + [promise])
        end

        active_index = active_resolves.index do |resolve|
          resolve[:requests].any? do |request|
            request.equal?(promise) && request.unresolved_loader_request?
          end
        end
        if active_index
          starter = active_resolves[active_index][:starter]
          starter_index = starter ? wait_stack.index { |waiting| waiting.equal?(starter) } : nil
          chain = if starter_index
            wait_stack[starter_index..] + [promise, starter]
          else
            [promise, promise]
          end
          reject_cycle(chain)
        end

        wait_stack << promise
        true
      end

      def find_cycle_path(promise, target_request, path = [], visited = {}.compare_by_identity)
        if promise.is_a?(LoaderPromise)
          return unless promise.pending?
          return path + [promise] if promise.equal?(target_request)
          return if visited.key?(promise)

          visited[promise] = true
          path = path + [promise]
        end

        promise.graphql_batch_dependency_promises.each do |dependency|
          found = find_cycle_path(dependency, target_request, path, visited)
          return found if found
        end
        nil
      end

      def ordered_path(path, request)
        [request] + path[0...-1] + [request]
      end

      def rotate_to_waiting_request(requests)
        cycle_requests = requests[0...-1]
        start = wait_stack.find do |waiting|
          cycle_requests.any? { |request| request.equal?(waiting) }
        end
        return requests unless start

        start_index = cycle_requests.index { |request| request.equal?(start) }
        rotated = cycle_requests[start_index..] + cycle_requests[0...start_index]
        rotated + [rotated.first]
      end

      def reject_cycle(requests)
        error = DependencyCycleError.new(requests.map { |request| [request.loader, request.key] })
        cycle_requests = requests.uniq(&:object_id).select(&:pending?)

        cycle_requests.each do |request|
          request.loader.send(:reject_dependency_cycle, request.key, error)
        end

        raise error
      end

      def wait_stack
        Thread.current[WAIT_STACK_KEY] ||= []
      end

      def active_resolves
        Thread.current[ACTIVE_RESOLVES_KEY] ||= []
      end

      def safe_loader_name(loader)
        klass = Object.instance_method(:class).bind_call(loader)
        name = Class.instance_method(:name).bind_call(klass)
        name || "#<Class:0x#{klass.object_id.to_s(16)}>"
      rescue StandardError
        "<Loader>"
      end

      def safe_key(key)
        case key
        when nil then "nil"
        when true then "true"
        when false then "false"
        when String then String.instance_method(:inspect).bind_call(key)
        when Symbol then Symbol.instance_method(:inspect).bind_call(key)
        when Integer then Integer.instance_method(:inspect).bind_call(key)
        when Float then Float.instance_method(:inspect).bind_call(key)
        when Array
          items = Array.instance_method(:map).bind_call(key) { |item| safe_key(item) }
          "[#{Array.instance_method(:join).bind_call(items, ', ')}]"
        when Hash
          pairs = Hash.instance_method(:map).bind_call(key) do |hash_key, value|
            "#{safe_key(hash_key)}=>#{safe_key(value)}"
          end
          "{#{Array.instance_method(:join).bind_call(pairs, ', ')}}"
        else
          safe_object_key(key)
        end
      rescue StandardError
        "<uninspectable key>"
      end

      def safe_object_key(key)
        klass = Object.instance_method(:class).bind_call(key)
        class_name = Class.instance_method(:name).bind_call(klass)
        class_name ||= "#<Class:0x#{klass.object_id.to_s(16)}>"
        object_id = Object.instance_method(:object_id).bind_call(key).to_s(16)
        "#<#{class_name}:0x#{object_id}>"
      rescue StandardError
        "<uninspectable key>"
      end
    end
  end

  module PromiseWaitTracking
    def wait
      DependencyCycle.waiting(self) { super }
    end
  end
end

class ::Promise
  prepend(GraphQL::Batch::PromiseWaitTracking)

  def graphql_batch_dependency_promises
    source = @source
    return [] if source.nil?
    return [source] if source.is_a?(::Promise) || ::Promise.const_get(:Group) === source

    []
  end

  class Group
    def graphql_batch_dependency_promises
      @inputs.each_with_object([]) do |input, promises|
        promises << input if input.is_a?(::Promise) && input.pending?
      end
    end
  end
end
