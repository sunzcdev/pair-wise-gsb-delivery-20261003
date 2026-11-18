# frozen_string_literal: true

require "redis_client/connection_mixin"

class RedisClient
  # An in-memory driver used by push_channel_test.rb to interleave RESP3 push
  # frames with normal command replies without needing a live Redis server.
  #
  # Frames to return are queued per connection instance via MemoryDriver.next.
  class MemoryConnection
    include ConnectionMixin

    class << self
      # Script the next connection's read stream. Each element is returned, in
      # order, by successive #read calls.
      def script(frames)
        @scripts ||= []
        @scripts << frames
      end

      def take_script
        (@scripts || []).shift || []
      end

      def reset!
        @scripts = []
      end

      def ssl_context(ssl_params)
        ssl_params
      end
    end

    def initialize(config, connect_timeout:, read_timeout:, write_timeout:)
      super(config)
      @connect_timeout = connect_timeout
      @read_timeout = read_timeout
      @write_timeout = write_timeout
      @frames = self.class.take_script.dup
      @closed = false
      connect
    end

    def connected?
      !@closed
    end

    def close
      @closed = true
      super
    end

    def connect
      @server_key = @config.server_key
      @closed = false
      true
    end

    def write(_command)
      raise ConnectionError, "connection closed" if @closed
    end

    def write_multi(_commands)
      raise ConnectionError, "connection closed" if @closed
    end

    def read(_timeout = nil)
      raise ConnectionError, "connection closed" if @closed
      raise ProtocolError, "no more scripted frames" if @frames.empty?

      @frames.shift
    end

    # Replace the in-memory read stream of an already established connection.
    def script=(frames)
      @frames = frames.dup
    end

    def remaining_frames
      @frames.dup
    end
  end
end
