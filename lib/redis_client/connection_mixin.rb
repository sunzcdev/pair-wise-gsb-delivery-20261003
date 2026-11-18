# frozen_string_literal: true

class RedisClient
  module ConnectionMixin
    # Mixed into connection errors raised after some command bytes were already
    # written to the socket. It lets the diagnostics layer distinguish "never
    # sent" from "sent but the outcome is unknown" without re-executing the
    # command.
    module SentButUnknown
      attr_accessor :sent_commands, :acknowledged_commands

      def unknown_outcome?
        sent_commands && sent_commands > (acknowledged_commands || 0)
      end
    end

    attr_accessor :retry_attempt
    attr_reader :config

    def initialize(config)
      @pending_reads = 0
      @retry_attempt = nil
      @config = config
      @server_key = nil
      @observing_context = nil
    end

    # The client sets the current Observability::AttemptContext here while a
    # logical command is in flight, so the connection can report partial sends.
    attr_accessor :observing_context

    def reconnect
      close
      connect
    end

    def close
      @pending_reads = 0
      nil
    end

    def revalidate
      if @pending_reads > 0 || @server_key != @config.server_key
        close
        false
      else
        connected?
      end
    end

    def call(command, timeout)
      @pending_reads += 1
      write(command)
      # The command is now on the wire: if reading fails below the server may
      # already have executed it.
      mark_command_sent!(1)
      result = read(connection_timeout(timeout))
      @pending_reads -= 1
      if result.is_a?(Error)
        result._set_command(command)
        result._set_config(config)
        result._set_retry_attempt(@retry_attempt)
        raise result
      else
        result
      end
    rescue ConnectionError, ProtocolError => error
      annotate_unknown_outcome!(error, 1)
      raise
    end

    def call_pipelined(commands, timeouts, exception: true)
      first_error = last_error = nil

      size = commands.size
      results = Array.new(commands.size)
      @pending_reads += size
      write_multi(commands)

      # The whole batch is buffered in one write. Once it succeeds every command
      # is potentially in flight; track how many replies we did manage to read.
      mark_command_sent!(size)
      acknowledged = 0

      size.times do |index|
        timeout = timeouts && timeouts[index]
        result = read(connection_timeout(timeout))
        @pending_reads -= 1
        acknowledged += 1

        # A multi/exec command can return an array of results.
        # An error from a multi/exec command is handled in Multi#_coerce!.
        if result.is_a?(Array)
          result.each do |res|
            res._set_config(config) if res.is_a?(Error)
          end
        elsif result.is_a?(Error)
          result._set_command(commands[index])
          result._set_config(config)
          result._set_retry_attempt(@retry_attempt)

          last_error&._set_next_error(result)
          first_error ||= result
          last_error = result
        end

        results[index] = result
      end

      if first_error && exception
        raise first_error
      else
        results
      end
    rescue ConnectionError, ProtocolError => error
      annotate_unknown_outcome!(error, size, acknowledged)
      raise
    end

    def connection_timeout(timeout)
      return timeout unless timeout && timeout > 0

      # Can't use the command timeout argument as the connection timeout
      # otherwise it would be very racy. So we add the regular read_timeout on top
      # to account for the network delay.
      timeout + config.read_timeout
    end

    def protocol_error(message)
      error = ProtocolError.with_config(message, config)
      error._set_retry_attempt(@retry_attempt)
      error
    end

    def connection_error(message)
      error = ConnectionError.with_config(message, config)
      error._set_retry_attempt(@retry_attempt)
      error
    end

    private

    def mark_command_sent!(count)
      @observing_context&.mark_sent!(count)
    end

    # Record on +error+ that up to +sent+ commands were written but only
    # +acknowledged+ replies were received, and mirror that into the active
    # diagnostics context. This metadata never triggers a retry by itself; the
    # client's existing reconnection policy remains the sole decision maker.
    def annotate_unknown_outcome!(error, sent, acknowledged = 0)
      error.extend(SentButUnknown) unless error.is_a?(SentButUnknown)
      error.sent_commands = sent
      error.acknowledged_commands = acknowledged
      context = @observing_context
      if context
        context.mark_sent!(sent)
        context.mark_acknowledged!(acknowledged)
        context.last_error = error
      end
      error
    end
  end
end
