# frozen_string_literal: true

require "dry/system/constants"

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
        @step_running = nil

        # Coordinates concurrent `start`/`resolve` calls for this provider. A single
        # attempt runs the lifecycle while every other caller waits for its outcome, so
        # concurrent resolves never re-run the steps or observe a half-finished start.
        @start_mutex = Mutex.new
        @start_condition = ConditionVariable.new
        @start_in_progress = false
        @start_owner = nil
        @start_error = nil

        # Keys staged by a step that failed. They stay available for a subsequent
        # `stop`/cleanup, and are discarded immediately before that step is retried, so a
        # fresh attempt never collides with or re-publishes the previous attempt's objects.
        @pending_rollback_keys = []

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
      # Concurrent calls while a start is already running do not run the lifecycle again.
      # They wait for the in-flight attempt to settle, then return the same result or
      # raise the same failure. After a failed attempt, the next call runs a fresh,
      # cleanly rolled-back attempt, so a corrected provider can start successfully.
      #
      # @return [self]
      #
      # @api public
      def start
        claim = claim_start

        # Another thread owns the in-flight attempt: wait for its outcome and either
        # re-raise its failure or return once the provider has started.
        return join_start if claim == :join

        # A re-entrant call from the thread running the attempt (e.g. a resolve of the
        # provider's own root key from inside its start block) must not re-run the steps.
        return self if claim == :reentrant

        # The provider has already started successfully.
        return self if claim == :done

        run_start_attempt

        self
      end

      # Runs the `stop` lifecycle step.
      #
      # Also runs any callbacks for the step.
      #
      # A provider whose start attempt failed may also be stopped, so its cleanup can
      # release the objects that were actually created before the failure. A provider
      # that was never started remains a no-op.
      #
      # @return [self]
      #
      # @api public
      def stop
        return self unless started? || start_failed?

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

      # Returns true if the most recent start attempt failed and has not since been
      # retried successfully.
      #
      # @api private
      def start_failed?
        !started? && !@start_error.nil?
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
        return self if step_running? || statuses.include?(step_name)

        # Discard the partial staging left by a previous failed attempt before running a
        # fresh `prepare`/`start`. This is deliberately not done for `stop`, whose
        # cleanup needs to resolve the objects that failed attempt registered.
        remove_pending_rollback_keys! if step_name == :prepare || step_name == :start

        @step_running = step_name
        staged_keys = staged_provider_keys

        begin
          source.run_callback(:before, step_name)
          source.public_send(step_name)
          source.run_callback(:after, step_name)

          # Registrations are published after prepare/start only. Running `apply` after
          # `stop` would re-publish partial staging left by a failed start while its
          # cleanup runs, leaking an old attempt's objects into the target container.
          apply unless step_name == :stop

          # Record the step only once its registrations have been published, so a failure
          # while applying does not leave the provider believing the step completed.
          statuses << step_name
        ensure
          # On failure, remember exactly the registrations this step added. They remain
          # resolvable via the provider container so a `stop` cleanup can release the
          # created objects, and are discarded before the next `prepare`/`start` attempt.
          # `stop` failures do not replace rollback state held for a failed start.
          if $! && step_name != :stop
            @pending_rollback_keys = added_staged_keys_since(staged_keys)
          end

          @step_running = nil
        end

        self
      end

      # Claims the right to run a start attempt for the current thread.
      #
      # @return [Symbol] one of `:run`, `:join`, `:reentrant`, or `:done`
      #
      # @api private
      def claim_start
        @start_mutex.synchronize do
          if @start_in_progress && @start_owner == Thread.current
            :reentrant
          elsif @start_in_progress
            :join
          elsif started?
            :done
          else
            @start_in_progress = true
            @start_owner = Thread.current
            @start_error = nil
            :run
          end
        end
      end

      # Waits for an in-flight start attempt to settle and shares its outcome.
      #
      # A failed attempt's exception is re-raised in every joining thread, giving all
      # concurrent resolves one deterministic result instead of a mix of key errors and
      # half-published values.
      #
      # @api private
      def join_start
        error = @start_mutex.synchronize do
          @start_condition.wait(@start_mutex) while @start_in_progress
          @start_error
        end

        raise error if error

        self
      end

      # Runs the prepare and start steps for the thread that claimed the attempt,
      # recording the outcome and releasing all waiting threads when it settles.
      #
      # @api private
      def run_start_attempt
        run_step(:prepare)
        run_step(:start)
      ensure
        # Capture any in-flight failure (including non-StandardError exits) so joining
        # threads settle with the same deterministic result instead of waiting forever.
        error = $!

        @start_mutex.synchronize do
          @start_error = error if error
          @start_in_progress = false
          @start_owner = nil
          @start_condition.broadcast
        end
      end

      # Snapshot of the keys currently staged in the provider's own container.
      #
      # @return [Array<String>]
      #
      # @api private
      def staged_provider_keys
        [].tap { |keys| provider_container.each_key { |key| keys << key } }
      end

      # Keys present now that were not present in the given snapshot, i.e. exactly the
      # registrations added by the step that just ran. This is what lets cleanup know
      # which objects were actually created rather than touching unrelated registrations.
      #
      # @return [Array<String>]
      #
      # @api private
      def added_staged_keys_since(before_keys)
        staged_provider_keys.reject { |key| before_keys.include?(key) }
      rescue StandardError
        # Never let rollback bookkeeping raise from inside the step's `ensure` and mask
        # the original lifecycle failure.
        []
      end

      # Removes the partial registrations recorded from a previous failed attempt. Scoped
      # strictly to the keys that attempt added; earlier successful steps are preserved.
      #
      # @return [self]
      #
      # @api private
      def remove_pending_rollback_keys!
        return self if @pending_rollback_keys.empty?

        @pending_rollback_keys.each do |key|
          provider_container._container.delete(key)
        end

        @pending_rollback_keys.clear

        self
      rescue StandardError
        # Rollback bookkeeping must never mask the lifecycle failure or block a retry.
        @pending_rollback_keys.clear
        self
      end

      # Returns true if a step is currenly running.
      #
      # This is important for short-circuiting the invocation of {#run_step} and avoiding
      # infinite loops if a provider step happens to result in resolution of a component
      # with the same root key as the provider's own name (which ordinarily results in
      # that provider being started).
      #
      # @return [Boolean]
      #
      # @see {#run_step}
      #
      # @api private
      def step_running?
        !!step_running
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
