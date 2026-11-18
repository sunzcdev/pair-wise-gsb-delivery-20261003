# frozen_string_literal: true

require "redis_client/ruby_connection/resp3"

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
      result = read_reply(connection_timeout(timeout))
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
      first_error = last_error = nil

      size = commands.size
      results = Array.new(commands.size)
      @pending_reads += size
      write_multi(commands)

      size.times do |index|
        timeout = timeouts && timeouts[index]
        result = read_reply(connection_timeout(timeout))
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

    # Reads the next command reply, transparently processing any interleaved
    # RESP3 push messages (e.g. client tracking invalidations) as they arrive.
    def read_reply(timeout)
      reply = read(timeout)
      while reply.is_a?(RESP3::Push)
        handle_push(reply)
        reply = read(timeout)
      end
      reply
    end

    def handle_push(push)
      if push.first == "invalidate" && (cache = @config.cache)
        cache.invalidate(push[1])
      end
      nil
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
  end
end
