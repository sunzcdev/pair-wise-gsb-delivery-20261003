# frozen_string_literal: true

class RedisClient
  module ConnectionMixin
    attr_accessor :retry_attempt, :push_channel
    attr_reader :config

    def initialize(config)
      @pending_reads = 0
      @retry_attempt = nil
      @config = config
      @server_key = nil
      @read_mutex = Mutex.new
      @push_channel = nil
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
      if dispatching?
        # This connection is currently delivering a push on the calling thread.
        # The outstanding @pending_reads belongs to the outer command whose
        # reply has not been consumed yet; tearing the connection down here
        # would lose that reply, so keep using it.
        return connected?
      end

      if @pending_reads > 0 || @server_key != @config.server_key
        close
        false
      else
        connected?
      end
    end

    def call(command, timeout)
      prevent_reentrant_dispatch!
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
      prevent_reentrant_dispatch!
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

    # Whether the calling thread is currently running a push callback that was
    # delivered by this very connection.
    def dispatching?
      Thread.current[:redis_client_push_dispatch].equal?(self)
    end

    # Fail fast when a push callback re-enters the very connection that is
    # dispatching the push: doing so would deadlock against the protocol read
    # lock. Commands on other connections remain allowed.
    def prevent_reentrant_dispatch!
      return unless dispatching?

      raise PushChannel::ReentrantDispatchError,
        "cannot read from a connection while it is dispatching a push; use another connection"
    end

    # Read a single command reply from the wire. RESP3 push frames never count
    # as the reply of a pending command: they are buffered inside the lock and
    # dispatched to the push channel after the protocol read lock has been
    # released, so subscriber callbacks are free to issue their own commands.
    def read_reply(timeout)
      prevent_reentrant_dispatch!

      pending_pushes = nil

      reply = nil
      @read_mutex.synchronize do
        loop do
          frame = read(timeout)
          if frame.is_a?(Push)
            (pending_pushes ||= []) << frame
          else
            reply = frame
            break
          end
        end
      end

      # The protocol read lock is no longer held from here on.
      dispatch_pushes(pending_pushes) if pending_pushes

      reply
    end

    # Hand the pushes collected during a read to the push channel without
    # holding the protocol read lock. A thread-local marker prevents a callback
    # from re-entering this exact connection (which would deadlock against the
    # read lock) while still allowing commands on other connections.
    def dispatch_pushes(pushes)
      channel = @push_channel or raise ProtocolError.with_config(
        "received RESP3 push but no push channel is configured", config
      )

      previous = Thread.current[:redis_client_push_dispatch]
      Thread.current[:redis_client_push_dispatch] = self
      begin
        pushes.each { |push| channel.dispatch(push) }
      ensure
        Thread.current[:redis_client_push_dispatch] = previous
      end
    end
  end
end
