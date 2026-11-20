# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2023, by Samuel Williams.

require_relative "helper"

require "puma/server"

class PumaServerCurrentApplication
  def call(env)
    [200, {"Content-Type" => "text/plain"}, [Puma::Server.current.to_s]]
  end
end

class PumaServerCurrentTest < PumaTest
  parallelize_me!

  def setup
    @tester = PumaServerCurrentApplication.new
    @server = Puma::Server.new @tester, nil, {log_writer: Puma::LogWriter.strings, clean_thread_locals: true}
    @port = (@server.add_tcp_listener "127.0.0.1", 0).addr[1]
    @tcp = "http://127.0.0.1:#{@port}"
    @url = URI.parse(@tcp)
    @server.run
  end

  def teardown
    @server.stop(true)
  end

  def test_clean_thread_locals
    server_string = @server.to_s
    responses = []

    # This must be a persistent connection to hit the `clean_thread_locals` code path.
    Net::HTTP.new(@url.host, @url.port).start do |connection|
      3.times do
        responses << connection.get("/").body
      end
    end

    assert_equal [server_string]*3, responses
  end

  # Puma::Server.current is a long-lived thread local, advertised onto the
  # processor thread when it is spawned. Cleaning request-scoped thread locals
  # must never remove it, including after the app raises.
  def test_server_current_survives_cleanup_after_an_exception
    raising_app = ->(env) do
      raise "boom" if env["PATH_INFO"] == "/raise"

      [200, {"Content-Type" => "text/plain"}, [Puma::Server.current.to_s]]
    end
    @server.app = raising_app

    server_string = @server.to_s

    Net::HTTP.new(@url.host, @url.port).start do |connection|
      error_response = connection.get("/raise")
      assert_match(/500/, error_response.code)

      # The thread local advertising the server must still be in place.
      assert_equal server_string, connection.get("/").body
      assert_equal server_string, connection.get("/").body
    end
  end
end
