# frozen_string_literal: true

module SolidQueue
  class Worker < Processes::Poller
    include LifecycleHooks

    after_boot :run_start_hooks
    before_shutdown :run_stop_hooks
    after_shutdown :run_exit_hooks

    attr_reader :queues, :queue_strategy, :pool

    def initialize(**options)
      execution_pool_type = options.key?(:fibers) ? :fiber : :thread

      options = options.dup.with_defaults(SolidQueue::Configuration::WORKER_DEFAULTS)
      execution_pool_size = execution_pool_type == :fiber ? options[:fibers] : options[:threads]

      # Ensure that the queues array is deep frozen to prevent accidental modification
      @queues = Array(options[:queues]).map(&:freeze).freeze

      # The strategy keeps its rotation state across polling rounds, so it is
      # built once for the lifetime of the worker instead of per claim. Accepts
      # either a plain name (`:fair`), a strategy instance, or a hash with
      # options, as produced by the YAML configuration (`name: fair, window: 2`).
      strategy_name, strategy_options = extract_queue_strategy(options)
      @queue_strategy = SolidQueue::QueueSelector.build_strategy(strategy_name, **strategy_options)

      @pool = Pool.build \
        type: execution_pool_type,
        size: execution_pool_size,
        on_idle: -> { wake_up },
        on_unrecoverable_error: -> { request_termination }

      super(**options)
    end

    def metadata
      super.merge(queues: queues.join(","), queue_strategy: queue_strategy_name, pool_type: pool.type, pool_size: pool.size)
    end

    private
      def extract_queue_strategy(options)
        configured = options[:queue_strategy] || :strict

        case configured
        when Hash
          configured = configured.symbolize_keys
          [ configured.fetch(:name, :strict), configured.except(:name) ]
        else
          [ configured, options.fetch(:queue_strategy_options, {}) ]
        end
      end

      def queue_strategy_name
        queue_strategy.class.name.demodulize.underscore.sub(/_strategy\z/, "")
      end

      def poll
        claim_executions.then do |executions|
          executions.each do |execution|
            pool.post(execution)
          end

          pool.idle? ? polling_interval : 10.minutes
        end
      end

      def claim_executions
        with_polling_volume do
          SolidQueue::ReadyExecution.claim(queues, pool.available_capacity, process_id, strategy: queue_strategy)
        end
      end

      def request_termination
        # Signal the poller to shut down without joining from the pool thread.
        # Runnable#stop joins when unsupervised, which would deadlock once
        # shutdown waits for this pool thread to finish.
        @stopped = true
        wake_up
      end

      def shutdown
        pool.shutdown
        pool.wait_for_termination(SolidQueue.shutdown_timeout)

        super
      end

      def all_work_completed?
        SolidQueue::ReadyExecution.aggregated_count_across(queues).zero?
      end

      def set_procline
        procline "waiting for jobs in #{queues.join(",")}"
      end
  end
end
