# frozen_string_literal: true

require_relative "test_helper"

# Verifies the execution-scoped semantics of request-level callbacks: a callback
# registered on a Request object may be scoped to the current execution (the default),
# in which case it is visible to every internal attempt of that execution (i.e.
# retries), and dropped when the execution finishes, with success or error, so that
# reusing the same Request object later doesn't trigger it anymore.
class CallbacksExecutionScopeTest < Minitest::Test
  include HTTPHelpers

  class FlakyApp < WEBrick::HTTPServlet::AbstractServlet
    def initialize(server, state)
      super
      @state = state
    end

    def do_GET(_req, res) # rubocop:disable Naming/MethodName
      @state[:requests] += 1

      if @state[:requests] <= @state[:failures]
        res.status = 503
      else
        res.status = 200
      end

      res.body = "attempt-#{@state[:requests]}"
    end
  end

  class FlakyServer < TestServer
    def initialize(**options)
      @state = { requests: 0, failures: 0 }
      super
      mount("/", FlakyApp, @state)
    end

    def failures=(num)
      @state[:failures] = num
    end

    def requests
      @state[:requests]
    end
  end

  # accepts the connection, reads the request, then drops the connection without
  # replying, forcing the client to surface an error.
  class DroppingServer
    attr_reader :origin

    def initialize
      @server = TCPServer.new("127.0.0.1", 0)
      @origin = "http://127.0.0.1:#{@server.addr[1]}"
      @thread = nil
    end

    def start
      @thread = Thread.new do
        sock = @server.accept
        begin
          while (line = sock.gets)
            break if line == "\r\n"
          end
        rescue StandardError
          # ignore
        ensure
          sock.close
        end
      end
      self
    end

    def shutdown
      @server.close
      @thread&.join(1)
    rescue IOError
      # ignore
    end
  end

  def test_reused_request_keeps_only_session_level_callbacks
    start_test_servlet(TestServer) do |server|
      server.mount_proc("/") { |_req, res| res.body = "ok" }

      http = HTTPX.plugin(:callbacks)

      request = http.build_request("GET", "#{server.origin}/")

      events = []

      request.on_request_started { events << :started }
      request.on_response_completed { events << :completed }

      first_events = []
      response = http.request(request)
      first_events.concat(events)

      events.clear

      response2 = http.request(request)

      assert_equal 200, response.status
      assert_equal 200, response2.status
      assert_equal %i[started completed], first_events, "local callbacks must fire on the first execution"
      assert_empty events, "local callbacks must not fire on the reused execution"
    end
  end

  def test_reused_request_fires_session_level_callbacks_on_every_execution
    start_test_servlet(TestServer) do |server|
      server.mount_proc("/") { |_req, res| res.body = "ok" }

      http = HTTPX.plugin(:callbacks)

      request = http.build_request("GET", "#{server.origin}/")

      events = []

      # opting out of the execution scope keeps the callback for every execution
      request.on_request_started(local: false) { events << :started }

      http.request(request)
      first_count = events.size

      http.request(request)

      assert_equal 1, first_count
      assert_equal 2, events.size, "non-local callbacks must fire on every execution"
    end
  end

  def test_reused_request_fires_session_callbacks_on_every_execution
    start_test_servlet(TestServer) do |server|
      server.mount_proc("/") { |_req, res| res.body = "ok" }

      events = []

      http = HTTPX.plugin(:callbacks).on_request_started { events << :started }

      request = http.build_request("GET", "#{server.origin}/")

      http.request(request)
      http.request(request)

      assert_equal %i[started started], events, "session callbacks must fire on every execution"
    end
  end

  def test_local_callbacks_are_visible_on_every_internal_attempt
    start_test_servlet(FlakyServer) do |server|
      server.failures = 1

      http = HTTPX.plugin(:callbacks).plugin(:retries, retry_on: ->(res) { res.respond_to?(:status) && res.status == 503 })

      request = http.build_request("GET", "#{server.origin}/")

      events = []

      request.on_request_started { events << :started }
      request.on_response_completed { |_req, res| events << [:completed, res.status] }

      response = http.request(request)

      assert_equal 200, response.status
      assert_equal 2, server.requests, "the request ought to have been retried once"
      assert_equal(
        [:started, [:completed, 503], :started, [:completed, 200]],
        events,
        "local callbacks must fire on every attempt of the same execution"
      )
    end
  end

  def test_local_callbacks_are_dropped_after_error
    server = DroppingServer.new.start

    http = HTTPX.plugin(:callbacks).with(timeout: { operation_timeout: 1 })

    request = http.build_request("GET", "#{server.origin}/")

    events = []

    request.on_request_started { events << :started }
    request.on_request_error { |_req, err| events << err.class }

    response = http.request(request)

    verify_error_response(response)

    assert_includes events, :started, "local callback ought to have fired before failing"
    assert events.any? { |event| event.is_a?(Class) }, "local error callback ought to have fired"
    refute request.local_callbacks_for?, "local callbacks must be dropped after the execution errored out"

    # reusing the request must not resurrect the local registration
    events.clear
    http.request(request)

    assert_empty events, "local callbacks must stay dropped after an errored execution"
  ensure
    server&.shutdown
  end

  def test_local_callbacks_fire_in_registration_order
    start_test_servlet(TestServer) do |server|
      server.mount_proc("/") { |_req, res| res.body = "ok" }

      http = HTTPX.plugin(:callbacks)

      request = http.build_request("GET", "#{server.origin}/")

      events = []

      request.on_request_started { events << :first }
      request.on_request_started { events << :second }
      request.on_request_started { events << :third }

      http.request(request)

      assert_equal %i[first second third], events, "callback order must be preserved"
    end
  end

  def test_request_callbacks_and_session_callbacks_coexist
    start_test_servlet(TestServer) do |server|
      server.mount_proc("/") { |_req, res| res.body = "ok" }

      events = []

      http = HTTPX.plugin(:callbacks).on_request_started { events << :session }

      request = http.build_request("GET", "#{server.origin}/")
      request.on_request_started { events << :request }

      http.request(request)

      assert_equal %i[session request], events, "request-level callbacks must fire along with session-level ones"
    end
  end
end
