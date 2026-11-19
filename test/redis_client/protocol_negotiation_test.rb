# frozen_string_literal: true

require "test_helper"

class RedisClient
  class ProtocolNegotiationTest < RedisClientTestCase
    # A fake Redis server speaking RESP. Each accepted connection is served
    # from a preset sequence of replies (one script per connection, in accept
    # order). Every received command is recorded so tests can assert exactly
    # what the client sent, on which connection, and in which order.
    #
    # Script entries are raw reply payloads. The special `:close` entry closes
    # the connection without replying. When the script is exhausted, the
    # connection is closed.
    class ScriptedServer
      attr_reader :port

      def initialize(*scripts)
        @scripts = scripts
        @connections = []
        @tcp_server = TCPServer.new("127.0.0.1", 0)
        @tcp_server.setsockopt(Socket::SOL_SOCKET, Socket::SO_REUSEADDR, true)
        @port = @tcp_server.addr[1]
        @mutex = Mutex.new
        @threads = []
        @accept_thread = Thread.new { accept_loop }
      end

      # Commands received on the connection at +index+, in order.
      def commands(index)
        @mutex.synchronize { @connections[index]&.dup || [] }
      end

      def connection_count
        @mutex.synchronize { @connections.size }
      end

      def close
        @accept_thread.kill
        @mutex.synchronize { @threads.each(&:kill) }
        @tcp_server.close
      end

      private

      def accept_loop
        loop do
          socket = @tcp_server.accept
          script = nil
          log = nil
          @mutex.synchronize do
            script = (@scripts[@connections.size] || []).dup
            log = []
            @connections << log
            @threads << Thread.new { serve(socket, script, log) }
          end
        end
      rescue IOError, SystemCallError
        # Server was closed.
      end

      def serve(socket, script, log)
        io = RedisClient::RubyConnection::BufferedIO.new(socket, read_timeout: 5, write_timeout: 5)
        while (command = RedisClient::RESP3.load(io))
          @mutex.synchronize { log << command }
          reply = script.shift
          break if reply.nil? || reply == :close

          socket.write(reply)
        end
      rescue StandardError
        # The client went away, or the script was exhausted.
      ensure
        socket.close
      end
    end

    HELLO_UNKNOWN_COMMAND = "-ERR unknown command `HELLO`, with args beginning with: `3`,\r\n"
    RESP3_HELLO_REPLY = "%1\r\n+proto\r\n:3\r\n"

    def test_resp3_negotiation_success
      server = ScriptedServer.new([RESP3_HELLO_REPLY, "+PONG\r\n"])
      client = new_client(server)

      assert_equal "PONG", client.call("PING")
      assert_equal 3, client.negotiated_protocol
      assert_equal [["HELLO", "3"], ["PING"]], server.commands(0)
      assert_equal 1, server.connection_count
    ensure
      server&.close
    end

    def test_protocol_2_does_not_send_hello
      server = ScriptedServer.new(["+PONG\r\n"])
      client = new_client(server, protocol: 2)

      assert_equal "PONG", client.call("PING")
      assert_equal 2, client.negotiated_protocol
      assert_equal [["PING"]], server.commands(0)
    ensure
      server&.close
    end

    def test_resp3_only_refuses_downgrade_by_default
      server = ScriptedServer.new([HELLO_UNKNOWN_COMMAND])
      client = new_client(server)

      error = assert_raises(RedisClient::UnsupportedServer) { client.call("PING") }
      assert_includes error.message, "protocol_fallback"
      assert_nil client.negotiated_protocol
      assert_equal [["HELLO", "3"]], server.commands(0)
      assert_equal 1, server.connection_count
    ensure
      server&.close
    end

    def test_explicit_resp3_only_refuses_downgrade
      server = ScriptedServer.new([HELLO_UNKNOWN_COMMAND])
      client = new_client(server, protocol: 3, protocol_fallback: false)

      assert_raises(RedisClient::UnsupportedServer) { client.call("PING") }
      assert_nil client.negotiated_protocol
      assert_equal 1, server.connection_count
    ensure
      server&.close
    end

    def test_fallback_to_resp2_when_hello_is_an_unknown_command
      server = ScriptedServer.new(
        [HELLO_UNKNOWN_COMMAND, "+OK\r\n"], # refused HELLO, then SELECT on the doomed connection
        ["+OK\r\n", "+OK\r\n", "+PONG\r\n"], # RESP2 prelude: AUTH, SELECT, then PING
      )
      client = new_client(server, password: "secret", db: 2, protocol_fallback: true)

      assert_equal "PONG", client.call("PING")
      assert_equal 2, client.negotiated_protocol
      assert_equal [
        ["HELLO", "3", "AUTH", "default", "secret"],
        ["SELECT", "2"],
      ], server.commands(0)
      assert_equal [
        ["AUTH", "secret"],
        ["SELECT", "2"],
        ["PING"],
      ], server.commands(1)
      assert_equal 2, server.connection_count
    ensure
      server&.close
    end

    def test_fallback_does_not_replay_business_commands
      server = ScriptedServer.new(
        [HELLO_UNKNOWN_COMMAND],
        ["+PONG\r\n", "+OK\r\n"],
      )
      client = new_client(server, protocol_fallback: true)

      assert_equal "PONG", client.call("PING")
      assert_equal "OK", client.call("SET", "foo", "bar")

      all_commands = server.commands(0) + server.commands(1)
      assert_equal 1, all_commands.count { |command| command.first == "PING" }
      assert_equal 1, all_commands.count { |command| command.first == "SET" }
      assert_equal [], server.commands(0).select { |command| %w[PING SET].include?(command.first) }
    ensure
      server&.close
    end

    def test_no_fallback_on_authentication_failure
      server = ScriptedServer.new(
        ["-WRONGPASS invalid username-password pair or user is disabled.\r\n"],
      )
      client = new_client(server, password: "wrong", protocol_fallback: true)

      assert_raises(RedisClient::AuthenticationError) { client.call("PING") }
      assert_nil client.negotiated_protocol
      assert_equal [["HELLO", "3", "AUTH", "default", "wrong"]], server.commands(0)
      assert_equal 1, server.connection_count
    ensure
      server&.close
    end

    def test_no_fallback_on_permission_error
      server = ScriptedServer.new(
        ["-NOPERM this user has no permissions to run the 'HELLO' command\r\n"],
      )
      client = new_client(server, protocol_fallback: true)

      assert_raises(RedisClient::PermissionError) { client.call("PING") }
      assert_nil client.negotiated_protocol
      assert_equal 1, server.connection_count
    ensure
      server&.close
    end

    def test_no_fallback_on_malformed_hello_reply
      server = ScriptedServer.new(["this is not a RESP reply\r\n"])
      client = new_client(server, protocol_fallback: true)

      assert_raises(RedisClient::ProtocolError) { client.call("PING") }
      assert_nil client.negotiated_protocol
      assert_equal 1, server.connection_count
    ensure
      server&.close
    end

    def test_no_fallback_when_server_drops_connection
      server = ScriptedServer.new([:close])
      client = new_client(server, protocol_fallback: true)

      assert_raises(RedisClient::ConnectionError) { client.call("PING") }
      assert_nil client.negotiated_protocol
      assert_equal 1, server.connection_count
    ensure
      server&.close
    end

    def test_fallback_resp2_authentication_failure_is_not_retried
      server = ScriptedServer.new(
        [HELLO_UNKNOWN_COMMAND],
        ["-WRONGPASS invalid username-password pair or user is disabled.\r\n"],
      )
      client = new_client(server, password: "bad", protocol_fallback: true)

      assert_raises(RedisClient::AuthenticationError) { client.call("PING") }
      assert_nil client.negotiated_protocol
      assert_equal [["AUTH", "bad"]], server.commands(1)
      assert_equal 2, server.connection_count
    ensure
      server&.close
    end

    def test_fallback_resp2_select_failure_is_accurate
      server = ScriptedServer.new(
        [HELLO_UNKNOWN_COMMAND, "+OK\r\n"],
        ["+OK\r\n", "-ERR DB index is out of range\r\n"],
      )
      client = new_client(server, password: "secret", db: 42, protocol_fallback: true)

      error = assert_raises(RedisClient::CommandError) { client.call("PING") }
      assert_includes error.message, "DB index is out of range"
      assert_nil client.negotiated_protocol
      assert_equal [
        ["AUTH", "secret"],
        ["SELECT", "42"],
      ], server.commands(1)
    ensure
      server&.close
    end

    def test_negotiated_protocol_is_cached_for_the_same_server
      server = ScriptedServer.new(
        [HELLO_UNKNOWN_COMMAND],
        ["+PONG\r\n"],
        ["+PONG\r\n"], # reconnection: no HELLO, straight to RESP2
      )
      client = new_client(server, protocol_fallback: true)

      assert_equal "PONG", client.call("PING")
      assert_equal 2, client.negotiated_protocol

      client.close
      assert_equal "PONG", client.call("PING")
      assert_equal 2, client.negotiated_protocol

      assert_equal [["HELLO", "3"]], server.commands(0)
      assert_equal [["PING"]], server.commands(1)
      assert_equal [["PING"]], server.commands(2)
      assert_equal 3, server.connection_count
    ensure
      server&.close
    end

    def test_negotiation_is_performed_again_when_the_node_changes
      server1 = ScriptedServer.new(
        [HELLO_UNKNOWN_COMMAND],
        ["+PONG\r\n"],
      )
      server2 = ScriptedServer.new(
        [HELLO_UNKNOWN_COMMAND],
        ["+PONG\r\n"],
      )
      client = new_client(server1, protocol_fallback: true)

      assert_equal "PONG", client.call("PING")
      assert_equal 2, client.negotiated_protocol

      client.close
      client.instance_variable_set(:@config, RedisClient.config(**client_config(server2, protocol_fallback: true)))
      # The cached negotiation is bound to the previous node identity.
      assert_nil client.negotiated_protocol
      assert_equal "PONG", client.call("PING")
      assert_equal 2, client.negotiated_protocol

      # The new node was negotiated from scratch: HELLO was attempted again.
      assert_equal [["HELLO", "3"]], server2.commands(0)
      assert_equal [["PING"]], server2.commands(1)
    ensure
      server1&.close
      server2&.close
    end

    def test_negotiated_protocol_is_nil_before_connecting
      client = RedisClient.new(host: "127.0.0.1", port: 1, protocol_fallback: true)
      assert_nil client.negotiated_protocol
    end

    private

    def client_config(server, **overrides)
      { host: "127.0.0.1", port: server.port, timeout: 1 }.merge(overrides)
    end

    def new_client(server, **overrides)
      RedisClient.new(**client_config(server, **overrides))
    end
  end
end
