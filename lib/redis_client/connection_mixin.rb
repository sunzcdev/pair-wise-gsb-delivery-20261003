# frozen_string_literal: true

require "redis_client/pipelined_result"

class RedisClient
  module ConnectionMixin
    attr_accessor :retry_attempt
    attr_reader :config

    def initialize(config)
      @pending_reads = 0
      @retry_attempt = nil
      @config = config
      @server_key = nil
    end

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
      result = read(connection_timeout(timeout))
      while resp3_push?(result)
        # RESP3 push messages are out-of-band, they must not be confused
        # with the reply of the command that was just sent.
        result = read(connection_timeout(timeout))
      end
      @pending_reads -= 1
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
      outcome = call_pipelined_detailed(commands, timeouts)

      if outcome.failure
        # Even though the pipeline as a whole failed, some commands may
        # already have been replied to, or even executed. Attach the detailed
        # outcome to the error so this partial progress isn't entirely lost.
        outcome.failure._set_pipeline_result(outcome)
        raise outcome.failure
      end

      if exception && (first_error = outcome.errors.first)
        raise first_error
      end

      outcome.results
    end

    # Detailed pipelined mode.
    #
    # Unlike #call_pipelined, it doesn't raise when the pipeline is
    # interrupted by a connection or protocol error. Instead it returns a
    # PipelinedResult describing exactly which commands were replied to,
    # which commands are in an unknown state, and whether the write or the
    # read phase was interrupted.
    #
    # Commands in the unknown state may already have been executed by the
    # server, so they are never resent by this method.
    def call_pipelined_detailed(commands, timeouts)
      size = commands.size
      results = Array.new(size) { PipelinedResult::UNKNOWN }
      errors = []
      pushes = []
      confirmed_count = 0
      failure = nil
      failure_stage = nil

      @pending_reads += size

      begin
        write_multi(commands)
      rescue ConnectionError, ProtocolError => error
        # The write was interrupted: no reply is confirmed, and it is
        # unknown which commands, if any, reached the server.
        failure = error
        failure_stage = :write
      else
        index = 0
        while index < size
          timeout = timeouts && timeouts[index]
          begin
            result = read(connection_timeout(timeout))
          rescue ConnectionError, ProtocolError => error
            # The read was interrupted: every command before `index` got its
            # reply confirmed, anything from `index` onward is unknown.
            failure = error
            failure_stage = :read
            break
          end

          if resp3_push?(result)
            # RESP3 push messages are out-of-band, they don't occupy a
            # command position.
            pushes << result
            next
          end
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

            errors.last&._set_next_error(result)
            errors << result
          end

          results[index] = result
          confirmed_count += 1
          index += 1
        end
      end

      PipelinedResult.new(
        commands: commands,
        results: results,
        errors: errors,
        confirmed_count: confirmed_count,
        failure: failure,
        failure_stage: failure_stage,
        pushes: pushes,
      )
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

    def resp3_push?(reply)
      # Only the pure Ruby RESP3 parser tags push messages. Other drivers
      # (e.g. hiredis) don't distinguish them from regular replies.
      defined?(RedisClient::RESP3::Push) && RedisClient::RESP3::Push === reply
    end
  end
end
