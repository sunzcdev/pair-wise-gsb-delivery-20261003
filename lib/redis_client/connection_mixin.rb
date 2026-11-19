# frozen_string_literal: true

class RedisClient
  module ConnectionMixin
    attr_accessor :retry_attempt
    attr_reader :config

    def initialize(config)
      @pending_reads = 0
      @retry_attempt = nil
      @config = config
      @server_key = nil
      @established = false
      @exit_recorded = false
    end

    def reconnect
      close
      connect
    end

    # The server key this connection was last opened against. Used to attribute
    # a close to the generation it actually belonged to, even when the published
    # address has moved on since.
    def _connected_server_key
      @server_key
    end

    # Marks this connection as having completed its prelude/role check. Only
    # established connections produce an exit event; a half-open attempt that
    # failed during connect does not.
    def _mark_established
      @established = true
      @exit_recorded = false
    end

    def _established?
      !!@established
    end

    def close
      record_exit
      @pending_reads = 0
      nil
    end

    # Records this connection's exit against the Sentinel config, once, and only
    # if the connection was actually established. The address is the one this
    # connection was opened against, so a connection retired by a failover is
    # attributed to the generation it belonged to rather than to its successor.
    def record_exit
      return unless @established
      return unless @exit_recorded == false
      return unless config.respond_to?(:record_connection_closed)

      @exit_recorded = true
      config.record_connection_closed(@server_key) if config.sentinel?
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
      begin
        write(command)
        result = read(connection_timeout(timeout))
        @pending_reads -= 1
      rescue ConnectionError, ProtocolError => error
        # The command was handed to the socket but no reply was observed, so the
        # server may or may not have applied it. `@pending_reads` deliberately
        # stays positive so the connection is discarded rather than reused with
        # a reply still in flight, and the command is recorded so unsafe writes
        # are not replayed on another node.
        mark_in_flight(error, [command], [])
        raise error
      end

      if result.is_a?(Error)
        result._set_command(command)
        result._set_config(config)
        result._set_retry_attempt(@retry_attempt)
        raise result
      else
        result
      end
    end

    def call_pipelined(commands, timeouts, exception: true)
      first_error = last_error = nil

      size = commands.size
      results = Array.new(commands.size)
      @pending_reads += size
      index = 0
      begin
        write_multi(commands)

        while index < size
          timeout = timeouts && timeouts[index]
          result = read(connection_timeout(timeout))
          @pending_reads -= 1

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
          index += 1
        end
      rescue ConnectionError, ProtocolError => error
        # Replies already read are real results and are preserved on the error.
        # The commands whose replies were not observed are the ones whose
        # outcome is unknown.
        mark_in_flight(error, commands[index..-1], results[0...index])
        raise error
      end

      if first_error && exception
        raise first_error
      else
        results
      end
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

    # Attach the facts a caller needs to decide whether a retry is safe:
    # which commands have an unknown outcome, which results are already real,
    # and which node those commands went to.
    def mark_in_flight(error, unknown_commands, known_results)
      error._set_config(config)
      error._set_retry_attempt(@retry_attempt)
      error._set_in_flight_commands(unknown_commands) if unknown_commands
      error._set_in_flight_server_key(@server_key)
      error._set_partial_results(known_results) if known_results
      error
    end
  end
end
