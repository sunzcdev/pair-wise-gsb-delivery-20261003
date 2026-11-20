# frozen_string_literal: true

require_relative "helper"
require "json"
require "puma/server"

# An application that records what it sees on its processor thread and stores
# request-scoped state both in the fiber local storage (Thread#[]) and in the
# true thread local storage (Thread#thread_variable_set).
#
# On the first request it also registers long-lived markers the way Puma or a
# plugin registers values for the whole lifetime of a processor thread.
class CleanThreadLocalsApplication
  # A request-scoped key that must be handed back after every request.
  FIBER_REQUEST_KEY  = :clean_thread_locals_request_fiber
  THREAD_REQUEST_KEY = :clean_thread_locals_request_thread

  # Long-lived keys that a plugin (or Puma itself) registers on the thread.
  FIBER_PLUGIN_KEY  = :clean_thread_locals_plugin_fiber
  THREAD_PLUGIN_KEY = :clean_thread_locals_plugin_thread

  def self.reset!
    @observations = []
  end

  def self.observations
    @observations
  end

  def call(env)
    thread = Thread.current

    # Register the long-lived markers once, before any request writes its
    # request-scoped fields.
    unless thread.key?(FIBER_PLUGIN_KEY)
      thread[FIBER_PLUGIN_KEY] = "plugin-fiber"
      thread.thread_variable_set(THREAD_PLUGIN_KEY, "plugin-thread")
    end

    observation = {
      "fiber_value_before"   => thread[FIBER_REQUEST_KEY],
      "thread_value_before"  => thread.thread_variable_get(THREAD_REQUEST_KEY),
      "puma_server"          => thread.puma_server.to_s,
      "plugin_marker"        => thread[FIBER_PLUGIN_KEY],
      "thread_plugin_marker" => thread.thread_variable_get(THREAD_PLUGIN_KEY),
    }
    self.class.observations << observation

    if env["PATH_INFO"] == "/raise"
      raise "boom from the rack app"
    end

    thread[FIBER_REQUEST_KEY] = "set-by-request"
    thread.thread_variable_set(THREAD_REQUEST_KEY, "set-by-request")

    [200, {"Content-Type" => "application/json"}, [JSON.generate(observation)]]
  end
end

class CleanThreadLocalsTest < PumaTest
  # The application above accumulates observations in a class-level array, so
  # the tests cannot run in parallel.
  def setup
    CleanThreadLocalsApplication.reset!
    @tester = CleanThreadLocalsApplication.new
    @server = Puma::Server.new @tester, nil, server_options
    @port = (@server.add_tcp_listener "127.0.0.1", 0).addr[1]
    @server.run
  end

  def teardown
    @server.stop(true)
  end

  def server_options
    {
      log_writer: Puma::LogWriter.strings,
      clean_thread_locals: true,
      # A single processor thread means every request is observed on the same
      # thread, so leaked request locals are visible to the next request.
      min_threads: 1,
      max_threads: 1,
    }
  end

  def get(path = "/")
    Net::HTTP.get(URI.parse("http://127.0.0.1:#{@port}#{path}"))
  end

  # Three requests over one persistent connection all run on the single
  # processor thread. Request-scoped fields from earlier requests must not be
  # visible to later requests.
  def test_request_locals_are_cleaned_between_requests
    Net::HTTP.start("127.0.0.1", @port) do |connection|
      3.times { connection.get("/") }
    end

    assert_equal 3, CleanThreadLocalsApplication.observations.length

    # The first request starts with a clean thread; later requests must too.
    CleanThreadLocalsApplication.observations.each do |observation|
      assert_nil observation["fiber_value_before"],
        "fiber local leaked into a later request"
      assert_nil observation["thread_value_before"],
        "thread local leaked into a later request"
    end
  end

  # Values Puma or a plugin registered on the thread for its whole lifetime
  # must survive the cleanup, as must the Puma::Server.current thread local.
  def test_long_lived_thread_markers_are_preserved
    get
    get

    CleanThreadLocalsApplication.observations.each do |observation|
      assert_equal "plugin-fiber", observation["plugin_marker"]
      assert_equal "plugin-thread", observation["thread_plugin_marker"]
      assert_equal @server.to_s, observation["puma_server"]
    end
  end

  # The cleanup runs in an ensure, so an application exception must still
  # return the request fields before the next request is processed.
  def test_request_locals_are_cleaned_after_an_exception
    get                                  # sets the request fields
    response = get("/raise")             # raises while handling
    assert_match(/500|Internal Server Error|Puma caught this error/, response)
    get                                  # must observe a clean thread

    observations = CleanThreadLocalsApplication.observations
    assert_equal 3, observations.length
    assert_nil observations.last["fiber_value_before"]
    assert_nil observations.last["thread_value_before"]

    # Long-lived markers survive the exception path as well.
    assert_equal "plugin-fiber", observations.last["plugin_marker"]
    assert_equal "plugin-thread", observations.last["thread_plugin_marker"]
    assert_equal @server.to_s, observations.last["puma_server"]
  end

  # A client that disconnects while the app is working leaves request fields
  # behind in the ensure cleanup rather than on the next request.
  def test_request_locals_are_cleaned_after_client_disconnect
    disconnected = Queue.new
    finished = Queue.new
    app = ->(env) do
      Thread.current[CleanThreadLocalsApplication::FIBER_REQUEST_KEY] =
        "set-by-request"
      Thread.current.thread_variable_set(
        CleanThreadLocalsApplication::THREAD_REQUEST_KEY, "set-by-request")
      disconnected << true
      sleep 0.2
      [200, {}, ["ok"]]
    ensure
      finished << true
    end

    server = Puma::Server.new app, nil,
      log_writer: Puma::LogWriter.strings, clean_thread_locals: true,
      min_threads: 1, max_threads: 1
    port = (server.add_tcp_listener "127.0.0.1", 0).addr[1]
    server.run

    socket = TCPSocket.new "127.0.0.1", port
    socket.syswrite "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
    disconnected.pop
    socket.close

    # Block until the in-flight request has unwound and run its ensure cleanup.
    finished.pop

    seen = nil
    server.app = ->(env) do
      seen = {
        fiber: Thread.current[CleanThreadLocalsApplication::FIBER_REQUEST_KEY],
        thread: Thread.current.thread_variable_get(
          CleanThreadLocalsApplication::THREAD_REQUEST_KEY),
        server: Thread.current.puma_server,
      }
      [200, {}, ["probed"]]
    end
    Net::HTTP.get(URI.parse("http://127.0.0.1:#{port}/"))

    refute_nil seen
    assert_nil seen[:fiber], "fiber local survived a client disconnect"
    assert_nil seen[:thread], "thread local survived a client disconnect"
    assert_equal server, seen[:server]
  ensure
    server&.stop(true)
  end

  # With the option disabled, request fields naturally persist across
  # requests handled by the same thread.
  def test_locals_persist_when_cleaning_is_disabled
    disabled = Puma::Server.new CleanThreadLocalsApplication.new, nil,
      log_writer: Puma::LogWriter.strings, clean_thread_locals: false,
      min_threads: 1, max_threads: 1
    port = (disabled.add_tcp_listener "127.0.0.1", 0).addr[1]
    disabled.run

    Net::HTTP.start("127.0.0.1", port) do |connection|
      2.times { connection.get("/") }
    end

    observations = CleanThreadLocalsApplication.observations
    assert_equal 2, observations.length
    assert_nil observations.first["fiber_value_before"]
    assert_equal "set-by-request", observations.last["fiber_value_before"]
    assert_equal "set-by-request", observations.last["thread_value_before"]
  ensure
    disabled&.stop(true)
  end
end

# Focused unit tests for the snapshot/restore helper itself.
class ThreadLocalCleanerTest < PumaTest
  parallelize_me!

  def test_restores_fiber_locals_including_changed_and_added_keys
    thread = Thread.new do
      Thread.current[:kept] = "original"
      Puma::ThreadLocalCleaner.wrap(Thread.current) do
        Thread.current[:kept] = "changed"
        Thread.current[:added] = "request-only"
      end
      [Thread.current[:kept], Thread.current[:added], Thread.current.keys]
    end

    kept, added, keys = thread.value
    assert_equal "original", kept
    assert_nil added
    assert_equal [:kept], keys
  end

  def test_restores_thread_variables_on_the_exception_path
    thread = Thread.new do
      current = Thread.current
      current.thread_variable_set(:kept, "original")
      begin
        Puma::ThreadLocalCleaner.wrap(current) do
          current.thread_variable_set(:kept, "changed")
          current.thread_variable_set(:added, "request-only")
          raise "unwind"
        end
      rescue RuntimeError
      end
      [current.thread_variable_get(:kept),
       current.thread_variable?(:added)]
    end

    kept, added_present = thread.value
    assert_equal "original", kept
    refute added_present
  end

  def test_does_not_touch_thread_attr_accessor_values
    Thread.attr_accessor(:cleaner_test_puma_server)

    thread = Thread.new do
      current = Thread.current
      current.cleaner_test_puma_server = :server
      Puma::ThreadLocalCleaner.wrap(current) do
        current[:request] = true
      end
      [current.cleaner_test_puma_server, current[:request], current.keys]
    end

    server, request_value, keys = thread.value
    assert_equal :server, server
    assert_nil request_value
    refute_includes keys, :request
  end
end
