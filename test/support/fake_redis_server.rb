# frozen_string_literal: true

require "socket"
require "redis_client/ruby_connection/buffered_io"
require "redis_client/ruby_connection/resp3"

# A tiny in-process RESP3 server used to drive the client through controlled
# failure sequences, without requiring a real Redis server.
#
# The `handler` is called as `handler.call(session, command)` for every command
# (except the `HELLO` prelude, which is answered automatically), and writes the
# reply it wants on `session`. Closing the session simulates a dropped
# connection. Passing no handler runs a minimal, well behaved `MULTI` server.
class FakeRedisServer
  # RESP fragments for the handlers.
  module Wire
    module_function

    def ok
      "+OK\r\n"
    end

    def pong
      "+PONG\r\n"
    end

    def queued
      "+QUEUED\r\n"
    end

    def null
      "$-1\r\n"
    end

    def int(value)
      ":#{value}\r\n"
    end

    def error(message)
      "-#{message}\r\n"
    end

    # `values` are raw RESP fragments, e.g. `array(ok, error("ERR nope"))`.
    def array(values)
      "*#{values.size}\r\n#{values.join}"
    end
  end

  class Session
    attr_reader :commands

    def initialize(socket)
      @socket = socket
      @io = RedisClient::RubyConnection::BufferedIO.new(socket, read_timeout: 1, write_timeout: 1)
      @commands = []
    end

    def read_command
      RedisClient::RESP3.load(@io)
    rescue EOFError, IOError, Errno::ECONNRESET, RedisClient::Error
      nil
    end

    def <<(bytes)
      @io.write(bytes)
      self
    rescue Errno::EPIPE, Errno::ECONNRESET, IOError
      close
      self
    end

    def close
      @socket.close unless @socket.closed?
    rescue IOError
      nil
    end

    def closed?
      @socket.closed?
    end
  end

  attr_reader :port

  def initialize(handler = nil, &block)
    @handler = block || handler || method(:default_handler)
    @server = TCPServer.new("127.0.0.1", 0)
    @server.setsockopt(Socket::SOL_SOCKET, Socket::SO_REUSEADDR, true)
    @port = @server.addr[1]
    @accepted = Queue.new
    @threads = []
    @queue = []
    @in_multi = false
    @accept_thread = Thread.new { accept_loop }
  end

  def client(**overrides)
    RedisClient.new(host: "127.0.0.1", port: port, timeout: 1.0, **overrides)
  end

  # Blocks until a connection was accepted, then returns its {Session}.
  def session
    @accepted.pop
  end

  def close
    begin
      @server.close
    rescue IOError
      nil
    end

    @accept_thread&.kill
    @threads.each(&:kill)
    @threads.each(&:join)
    nil
  end

  private

  def accept_loop
    loop do
      session = Session.new(@server.accept)
      @accepted << session
      @threads << Thread.new(session) { |s| serve(s) }
    end
  rescue IOError, Errno::EBADF
    nil
  end

  def serve(session)
    while (command = session.read_command)
      session.commands << command
      if command.first.upcase == "HELLO"
        # The connection prelude isn't part of the scenario under test.
        session << "_\r\n"
      else
        @handler.call(session, command)
      end
      break if session.closed?
    end
  ensure
    session.close
  end

  # Minimal Redis compatible implementation, enough to exercise transactions.
  def default_handler(session, command)
    case command.first.upcase
    when "MULTI"
      @in_multi = true
      @queue = []
      session << Wire.ok
    when "EXEC"
      @in_multi = false
      session << Wire.array(@queue.map { |queued| reply_for(queued) })
    when "DISCARD"
      @in_multi = false
      @queue = []
      session << Wire.ok
    when "WATCH", "UNWATCH"
      session << Wire.ok
    else
      if @in_multi
        @queue << command
        session << Wire.queued
      else
        session << reply_for(command)
      end
    end
  end

  def reply_for(command)
    case command.first.upcase
    when "PING" then Wire.pong
    when "GET" then Wire.null
    when "INCR" then Wire.int(1)
    else Wire.ok
    end
  end
end
