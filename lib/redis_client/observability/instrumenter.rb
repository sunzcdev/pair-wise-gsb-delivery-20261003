# frozen_string_literal: true

class RedisClient
  module Observability
    # Generates the hierarchical events for a client and forwards them to the
    # configured observer. One instrumenter is owned per RedisClient instance.
    #
    # The instrumenter only *observes* the client's own reconnection loop: it
    # never wraps the business middleware stack and never sends Redis commands.
    class Instrumenter
      def initialize(config)
        @observer = config.observer || NullObserver.instance
        @log_arguments = config.log_arguments == true
        @config = config
      end

      # Run a logical command (single command or a pipeline). Exactly one
      # +command_started+ / +command_finished+ pair is emitted for the whole
      # block, even when the block internally reconnects and retries.
      #
      # +context+ is the client's attempt context (shared with #connect); it
      # carries the current attempt number and a flag indicating whether the
      # command bytes were already written when an error occurred.
      def instrument(name, command: nil, commands: nil, context:, &block)
        # The context, created by the client, owns the stable logical call id.
        # Generating another id here would split the command and its attempts.
        call_id = context.call_id
        pipeline_id = context.pipeline_id
        start = monotonic_ms

        command_event = build_event(
          kind: :command, name: name, call_id: call_id, pipeline_id: pipeline_id,
          command: command, commands: commands, size: commands&.size,
          started_at: start, context: context
        )
        @observer.command_started(command_event)

        begin
          result = block.call
          finish = monotonic_ms
          @observer.command_finished(command_event.merge(status: :success, finished_at: finish,
                                                         duration_ms: finish - start))
          result
        rescue Exception => error # rubocop:disable Lint/RescueException
          finish = monotonic_ms
          status = terminal_status(error, context)
          @observer.command_finished(command_event.merge(status: status, error: error,
                                                         finished_at: finish,
                                                         duration_ms: finish - start))
          raise
        end
      end

      # Run one concrete attempt of the surrounding command. Emits an attempt
      # pair with its own attempt_id, while reusing the parent call_id so the
      # logical command and all of its tries can be correlated.
      def instrument_attempt(name, command:, commands:, context:, &block)
        call_id = context.call_id
        attempt = context.attempt
        # The client (observe_attempt_body) already allocated this attempt's id
        # on the shared context so that nested connect events reuse it. Honor it
        # instead of minting a second, divergent id.
        attempt_id = context.attempt_id ||= new_id
        pipeline_id = context.pipeline_id
        start = monotonic_ms

        event = build_event(
          kind: :attempt, name: name, call_id: call_id, attempt_id: attempt_id,
          pipeline_id: pipeline_id, attempt: attempt, command: command,
          commands: commands, size: commands&.size, phase: :command,
          started_at: start, context: context
        )
        @observer.attempt_started(event)

        begin
          result = block.call
          finish = monotonic_ms
          @observer.attempt_finished(event.merge(status: :success, finished_at: finish,
                                                 duration_ms: finish - start))
          result
        rescue Exception => error # rubocop:disable Lint/RescueException
          finish = monotonic_ms
          @observer.attempt_finished(event.merge(status: attempt_status(error, context),
                                                 error: error, finished_at: finish,
                                                 duration_ms: finish - start))
          raise
        end
      end

      # Observe a connection (re)establishment. The prelude (AUTH/HELLO/SELECT
      # and friends) is reported separately and explicitly marked as such, so
      # authentication commands are never confused with user traffic.
      def instrument_connect(context:, reconnect:, &block)
        start = monotonic_ms
        event = build_event(
          kind: :connect, name: :connect, call_id: context.call_id,
          attempt_id: context.attempt_id, pipeline_id: context.pipeline_id,
          attempt: context.attempt, phase: :connect,
          started_at: start, context: context,
          metadata: { reconnect: reconnect }
        )
        @observer.connect_started(event)

        begin
          result = block.call
          finish = monotonic_ms
          @observer.connect_finished(event.merge(status: :success, finished_at: finish,
                                                  duration_ms: finish - start))
          result
        rescue Exception => error # rubocop:disable Lint/RescueException
          finish = monotonic_ms
          @observer.connect_finished(event.merge(status: :error, error: error,
                                                  finished_at: finish,
                                                  duration_ms: finish - start))
          raise
        end
      end

      # Emit detail events for the prelude commands of a freshly established
      # connection. These are always flagged +prelude+ and never contain raw
      # secrets unless argument logging was explicitly opted into.
      def observe_prelude(commands, results, context:)
        commands.each_with_index do |command, index|
          start = monotonic_ms
          event = build_event(
            kind: :attempt, name: :prelude, call_id: context.call_id,
            attempt_id: context.attempt_id, pipeline_id: context.pipeline_id,
            attempt: context.attempt, phase: :connect, command: command,
            index: index, size: commands.size, prelude: true,
            started_at: start, context: context
          )
          @observer.attempt_started(event)

          result = results&.at(index)
          finish = monotonic_ms
          if result.is_a?(Error)
            @observer.attempt_finished(event.merge(status: :error, error: result,
                                                   finished_at: finish,
                                                   duration_ms: finish - start))
          else
            @observer.attempt_finished(event.merge(status: :success,
                                                   finished_at: finish,
                                                   duration_ms: finish - start))
          end
        end
      end

      # Build a fresh per-command context shared by the retry loop, the
      # instrumenter and the connection hooks.
      def new_context(call_id, pipeline_id, name: nil, command: nil, commands: nil)
        AttemptContext.new(call_id: call_id, pipeline_id: pipeline_id, name: name,
                           command: command, commands: commands)
      end

      def new_id
        SecureRandom.hex(8)
      end

      private

      # The logical command is :unknown when any physical attempt wrote the
      # (write) command and then lost the connection, even if a *later* attempt
      # fails earlier (e.g. cannot reconnect). The context accumulates the fact
      # that bytes were sent across all attempts, so this does not depend on the
      # final error object carrying the marker.
      def terminal_status(error, context)
        if context.unknown_outcome?(error)
          :unknown
        else
          :error
        end
      end

      def attempt_status(error, context)
        if context.attempt_unknown?
          :unknown
        elsif error.is_a?(ConnectionError) || error.is_a?(ProtocolError)
          # The client tells us, via the context, whether another physical try
          # is scheduled for this reconnectable failure. :unknown wins above.
          context.will_retry? ? :retry : :error
        else
          :error
        end
      end

      def build_event(kind:, name:, call_id:, context:, **fields)
        fields[:command] = Observability.redact(fields[:command], raw: @log_arguments) if fields.key?(:command)
        if fields.key?(:commands)
          fields[:commands] = Observability.redact_batch(fields[:commands], raw: @log_arguments)
        end

        defaults = {
          attempt_id: context.attempt_id,
          attempt: context.attempt,
          retryable: context.retryable,
          config: @config
        }
        Event.new(kind: kind, name: name, call_id: call_id, **defaults.merge(fields))
      end

      def monotonic_ms
        Process.clock_gettime(Process::CLOCK_MONOTONIC, :float_millisecond)
      end
    end

    # Mutable per-logical-command state carried through the retry loop. It is
    # internal plumbing, never exposed to observers.
    class AttemptContext
      attr_accessor :call_id, :pipeline_id, :name, :command, :commands,
        :attempt, :attempt_id, :retryable, :last_error, :retry_policy,
        :reconnecting

      def initialize(call_id:, pipeline_id:, name: nil, command: nil, commands: nil)
        @call_id = call_id
        @pipeline_id = pipeline_id
        @name = name
        @command = command
        @commands = commands
        @attempt = 0
        @attempt_id = nil
        @retryable = true
        @reconnecting = false
        @last_error = nil
        # Number of commands written during the current physical attempt, and
        # the highest number written in any attempt so far. The latter is what
        # makes the logical command :unknown even if a later attempt fails
        # before reconnecting.
        @attempt_sent = 0
        @attempt_acknowledged = 0
        @ever_sent = 0
        # Pure predicate answering "is another physical try scheduled for
        # attempt N?". Injected by the client; defaults to never retrying.
        @retry_policy = ->(_attempt) { false }
      end

      # Reset the per-attempt counters at the start of each physical try. The
      # cumulative +ever_sent+ counter is deliberately preserved.
      def begin_attempt!
        @attempt_sent = 0
        @attempt_acknowledged = 0
        @reconnecting = false
      end

      def reconnecting?
        @reconnecting == true
      end

      # Called by the connection layer when it fails after having written some
      # bytes. +count+ is the number of commands from the batch already sent.
      def mark_sent!(count = 1)
        @attempt_sent = count if count > @attempt_sent
        @ever_sent = @attempt_sent if @attempt_sent > @ever_sent
      end

      def mark_acknowledged!(count)
        @attempt_acknowledged = count if count > @attempt_acknowledged
      end

      # Whether the current attempt sent commands whose result is unknown.
      def attempt_unknown?
        return false if @attempt_sent.zero?

        @attempt_sent > @attempt_acknowledged && write_command?
      end

      # Whether any attempt of this logical command left a write of unknown
      # outcome. This survives a later, earlier-failing reconnection attempt.
      def unknown_outcome?(_error = nil)
        return false if @ever_sent.zero?

        write_command?
      end

      # Whether the failure of the current attempt will be followed by another
      # try. Only meaningful for reconnectable error classes, which the
      # instrumenter checks separately.
      def will_retry?
        @retryable == true && @retry_policy.call(@attempt) == true
      end

      private

      # A batch is treated as a write if any of its commands is not known to be
      # read-only. Single commands use the same conservative classification.
      def write_command?
        list = @commands || (@command ? [@command] : [])
        list.any? { |cmd| !Observability.read_only_command?(cmd) }
      end
    end
  end
end
