# frozen_string_literal: true

class RedisClient
  module Observability
    # A single immutable observation emitted by the diagnostics layer.
    #
    # Events come in three hierarchical kinds:
    #
    # * +:command+  - one logical command. A command that is transparently
    #   retried after a reconnection still emits exactly one pair of these
    #   (+start+ / +finish+), sharing the same stable +call_id+.
    # * +:attempt+  - one concrete attempt at executing a command (or batch).
    #   Every attempt has its own +attempt_id+ and carries the parent
    #   +call_id+, the zero-based +attempt+ number and the actual terminal
    #   +status+ of that attempt.
    # * +:connect+  - a connection (re)establishment, including the
    #   authentication/selection prelude. Emitted inside the attempt that
    #   needed it.
    #
    # Events are plain value objects: observers must never mutate them.
    class Event
      # Terminal statuses:
      # * +:success+ - the operation completed and a reply was obtained
      # * +:error+   - the operation failed terminally (the error is in
      #   +error+, and +final?+ on it tells whether more retries were possible)
      # * +:retry+   - the attempt failed but the client will retry it
      # * +:unknown+ - the (write) command was already sent to the server but
      #   the outcome could not be determined before the connection was lost.
      #   The diagnostics layer never re-executes such commands.
      STATUSES = [:success, :error, :retry, :unknown].freeze

      KINDS = [:command, :attempt, :connect].freeze

      attr_reader :kind, :name, :call_id, :attempt_id, :pipeline_id,
        :attempt, :index, :size, :phase, :status, :command, :commands,
        :prelude, :retryable, :error, :started_at, :finished_at, :duration_ms,
        :config, :metadata

      def initialize(
        kind:,
        name:,
        call_id:,
        attempt_id: nil,
        pipeline_id: nil,
        attempt: nil,
        index: nil,
        size: nil,
        phase: nil,
        status: nil,
        command: nil,
        commands: nil,
        prelude: false,
        retryable: nil,
        error: nil,
        started_at: nil,
        finished_at: nil,
        duration_ms: nil,
        config: nil,
        metadata: nil
      )
        @kind = kind
        @name = name
        @call_id = call_id
        @attempt_id = attempt_id
        @pipeline_id = pipeline_id
        @attempt = attempt
        @index = index
        @size = size
        @phase = phase
        @status = status
        @command = command
        @commands = commands
        @prelude = prelude
        @retryable = retryable
        @error = error
        @started_at = started_at
        @finished_at = finished_at
        @duration_ms = duration_ms
        @config = config
        @metadata = metadata
        freeze
      end

      # Convenience predicates used by observers.
      def command?
        @kind == :command
      end

      def attempt?
        @kind == :attempt
      end

      def connect?
        @kind == :connect
      end

      def pipeline?
        !@pipeline_id.nil?
      end

      # Prelude commands are the authentication/selection commands issued
      # automatically while (re)connecting, never user traffic.
      def prelude?
        @prelude == true
      end

      def success?
        @status == :success
      end

      def unknown?
        @status == :unknown
      end

      def retried?
        @status == :retry
      end

      def to_h
        instance_variables.each_with_object({}) do |variable, hash|
          value = instance_variable_get(variable)
          hash[variable.to_s.delete_prefix("@").to_sym] = value
        end
      end

      # Return a new event with the given attributes overridden. Events are
      # immutable, so this never mutates the previously emitted one.
      def merge(**overrides)
        Event.new(**to_h.merge(overrides))
      end

      def inspect
        "#<#{self.class.name} kind=#{@kind.inspect} name=#{@name.inspect} " \
          "call_id=#{@call_id.inspect} attempt_id=#{@attempt_id.inspect} " \
          "status=#{@status.inspect}>"
      end
    end
  end
end
