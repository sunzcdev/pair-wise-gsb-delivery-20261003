# frozen_string_literal: true

require "thread"

require "dry/system/constants"
require "dry/system/errors"

module Dry
  module System
    # Providers can prepare and register one or more objects and typically work with third
    # party code. A typical provider might be for a database library, or an API client.
    #
    # The particular behavior for any provider is defined in a {Provider::Source}, which
    # is a subclass created when you run {Container.register_provider} or
    # {Dry::System.register_provider_source}. The Source provides this behavior through
    # methods for each of the steps in the provider lifecycle: `prepare`, `start`, and
    # `run`. These methods typically create and configure various objects, then register
    # them with the {#provider_container}.
    #
    # The Provider manages this lifecycle by implementing common behavior around the
    # lifecycle steps, such as running step callbacks, and only running steps when
    # appropriate for the current status of the lifecycle.
    #
    # Providers can be registered via {Container.register_provider}.
    #
    # @example Simple provider
    #   class App < Dry::System::Container
    #     register_provider(:logger) do
    #       prepare do
    #         require "logger"
    #       end
    #
    #       start do
    #         register(:logger, Logger.new($stdout))
    #       end
    #     end
    #   end
    #
    #   App[:logger] # returns configured logger
    #
    # @example Using an external Provider Source
    #   class App < Dry::System::Container
    #     register_provider(:logger, from: :some_external_provider_source) do
    #       configure do |config|
    #         config.log_level = :debug
    #       end
    #
    #       after :start do
    #         register(:my_extra_logger, resolve(:logger))
    #       end
    #     end
    #   end
    #
    #   App[:my_extra_logger] # returns the extra logger registered in the callback
    #
    # @api public
    class Provider
      # Fiber-local key holding the names of the providers whose lifecycle steps are
      # currently running on the current thread, in resolution order. Used to tell a
      # genuine circular dependency apart from a provider resolving its own root key.
      #
      # @api private
      RESOLUTION_CHAIN_KEY = :__dry_system_provider_resolution_chain__

      # Returns the provider's unique name.
      #
      # @return [Symbol]
      #
      # @api public
      attr_reader :name

      # Returns the default namespace for the provider's container keys.
      #
      # @return [Symbol,String]
      #
      # @api public
      attr_reader :namespace

      # Returns an array of lifecycle steps that have been run.
      #
      # @return [Array<Symbol>]
      #
      # @example
      #   provider.statuses # => [:prepare, :start]
      #
      # @api public
      attr_reader :statuses

      # Returns the name of the currently running step, if any.
      #
      # @return [Symbol, nil]
      #
      # @api private
      attr_reader :step_running
      private :step_running

      # Returns the container for the provider.
      #
      # This is where the provider's source will register its components, which are then
      # later marged into the target container after the `prepare` and `start` lifecycle
      # steps.
      #
      # @return [Dry::Core::Container]
      #
      # @api public
      attr_reader :provider_container
      alias_method :container, :provider_container

      # Returns the target container for the provider.
      #
      # This is the container with which the provider is registered (via
      # {Dry::System::Container.register_provider}).
      #
      # Registered components from the provider's container will be merged into this
      # container after the `prepare` and `start` lifecycle steps.
      #
      # @return [Dry::System::Container]
      #
      # @api public
      attr_reader :target_container
      alias_method :target, :target_container

      # Returns the provider's source
      #
      # The source provides the specific behavior for the provider via methods
      # implementing the lifecycle steps.
      #
      # The provider's source is defined when registering a provider with the container,
      # or an external provider source.
      #
      # @see Dry::System::Container.register_provider
      # @see Dry::System.register_provider_source
      #
      # @return [Dry::System::Provider::Source]
      #
      # @api private
      attr_reader :source

      # @api private
      # rubocop:disable Style/KeywordParametersOrder
      def initialize(name:, namespace: nil, target_container:, source_class:, source_options: {}, &)
        @name = name
        @namespace = namespace
        @target_container = target_container

        @provider_container = build_provider_container
        @statuses = []

        # Synchronizes lifecycle step access across threads. A provider step runs once:
        # other threads resolving the same provider wait on the condition variable and
        # either observe its success or receive the failure that ended this attempt,
        # leaving the provider free to be retried afterwards.
        @mutex = Mutex.new
        @condition = ConditionVariable.new
        @step_running = nil
        @step_thread = nil
        @step_error = nil

        @source = source_class.new(
          **source_options,
          provider_container: provider_container,
          target_container: target_container,
          &
        )
      end
      # rubocop:enable Style/KeywordParametersOrder

      # Runs the `prepare` lifecycle step.
      #
      # Also runs any callbacks for the step, and then merges any registered components
      # from the provider container into the target container.
      #
      # @return [self]
      #
      # @api public
      def prepare
        run_step(:prepare)
      end

      # Runs the `start` lifecycle step.
      #
      # Also runs any callbacks for the step, and then merges any registered components
      # from the provider container into the target container.
      #
      # @return [self]
      #
      # @api public
      def start
        run_step(:prepare)
        run_step(:start)
      end

      # Runs the `stop` lifecycle step.
      #
      # Also runs any callbacks for the step.
      #
      # @return [self]
      #
      # @api public
      def stop
        return self unless started?

        run_step(:stop)
      end

      # Returns true if the provider's `prepare` lifecycle step has run
      #
      # @api public
      def prepared?
        statuses.include?(:prepare)
      end

      # Returns true if the provider's `start` lifecycle step has run
      #
      # @api public
      def started?
        statuses.include?(:start)
      end

      # Returns true if the provider's `stop` lifecycle step has run
      #
      # @api public
      def stopped?
        statuses.include?(:stop)
      end

      private

      # @api private
      def build_provider_container
        container = Core::Container.new

        case namespace
        when String, Symbol
          container.namespace(namespace) { |c| return c }
        when true
          container.namespace(name) { |c| return c }
        when nil
          container
        else
          raise ArgumentError,
            "+namespace:+ must be true, string or symbol: #{namespace.inspect} given."
        end
      end

      # @api private
      def run_step(step_name)
        @mutex.synchronize do
          return self if statuses.include?(step_name)

          # A step for this provider is already running.
          if step_running
            if @step_thread == Thread.current
              # On this thread, the provider is being resolved again while its own step
              # is in progress. A single re-entry (a chain of length one) supports
              # resolving a component sharing the provider's own root key; any deeper
              # re-entry closes a loop and is a genuine circular dependency.
              if resolution_chain.one?
                return self
              end

              raise_circular_dependency_error
            else
              # Another thread is running the step. Wait for it to finish, then share
              # its outcome: success returns normally, failure re-raises the error.
              wait_for_step(step_name)
              return self
            end
          end

          # A previous attempt failed before completing this step. Drop the recorded
          # error so the step can be retried with the corrected dependencies.
          @step_error = nil
          @step_running = step_name
          @step_thread = Thread.current
        end

        begin
          begin_step(step_name)
        rescue StandardError => error
          fail_step(error)
          raise
        else
          complete_step(step_name)
        end

        self
      end

      # Runs the step's source behavior with this provider tracked on the current
      # thread's resolution chain, allowing genuine cycles to be detected.
      #
      # @return [void]
      #
      # @api private
      def begin_step(step_name)
        resolution_chain.push(name)

        begin
          source.run_callback(:before, step_name)
          source.public_send(step_name)
          source.run_callback(:after, step_name)
        ensure
          resolution_chain.pop
        end
      end

      # Marks the step complete, records its status, and applies the registered
      # components before waking any threads waiting for this provider.
      #
      # @return [void]
      #
      # @api private
      def complete_step(step_name)
        @mutex.synchronize do
          statuses << step_name
          apply
          @step_running = nil
          @step_thread = nil
          @condition.broadcast
        end
      end

      # Records the failure, clears the running step, and wakes every waiting thread so
      # they observe the same error. The error is retained only until the next attempt.
      #
      # @return [void]
      #
      # @api private
      def fail_step(error)
        @mutex.synchronize do
          @step_error = error
          @step_running = nil
          @step_thread = nil
          @condition.broadcast
        end
      end

      # Waits for another thread's running step to settle, then re-raises its error.
      #
      # @return [void]
      #
      # @api private
      def wait_for_step(step_name)
        while step_running && !statuses.include?(step_name)
          @condition.wait(@mutex)
        end

        raise @step_error if @step_error
        raise ProviderCircularDependencyError.new([name]) unless statuses.include?(step_name)
      end

      # Raises an error describing the cycle formed by the providers currently on the
      # resolution chain, from the first occurrence of this provider back to itself.
      #
      # @return [void]
      #
      # @api private
      def raise_circular_dependency_error
        start_index = resolution_chain.index(name)
        cycle = resolution_chain[start_index..] + [name]

        raise ProviderCircularDependencyError.new(cycle)
      end

      # The names of the providers whose steps are running on the current thread, in the
      # order in which they were resolved. Fiber-local so distinct threads (and fibers)
      # keep independent chains and never mistake shared initialization for a cycle.
      #
      # @return [Array<Symbol>]
      #
      # @api private
      def resolution_chain
        Thread.current[RESOLUTION_CHAIN_KEY] ||= []
      end

      # Registers any components from the provider's container in the main container.
      #
      # Called after each lifecycle step runs.
      #
      # @return [self]
      #
      # @api private
      def apply
        provider_container.each_key do |key|
          next if target_container.registered?(key)

          # Access the provider's container items directly so that we can preserve all
          # their options when we merge them with the target container (e.g. if a
          # component in the provider container was registered with a block, we want block
          # registration behavior to be exhibited when later resolving that component from
          # the target container). TODO: Make this part of dry-system's public API.
          item = provider_container._container[key]

          if item.callable?
            target_container.register(key, **item.options, &item.item)
          else
            target_container.register(key, item.item, **item.options)
          end
        end

        self
      end
    end
  end
end
