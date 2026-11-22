# frozen_string_literal: true

require_relative "helper"
require_relative "helpers/test_puma/puma_socket"
require "puma/events"
require "puma/server"
require "puma/server_plugin_control"

class TestServerPluginControl < PumaTest
  parallelize_me!

  include TestPuma
  include TestPuma::PumaSocket

  HOST = HOST4

  def setup
    @host = HOST
    @app = ->(env) { [200, {}, [env["rack.url_scheme"]]] }

    @log_writer = Puma::LogWriter.strings
    @events = Puma::Events.new
    @server = Puma::Server.new @app, @events, {log_writer: @log_writer}
    @control = Puma::ServerPluginControl.new(@server)
  end

  def teardown
    # Release any request threads still blocked on the gate so the server
    # can shut down promptly. Signaling once per known pool thread covers
    # threads that have not reached the gate yet; extra items stay in the
    # queue and are harmless.
    if defined?(@release) && @release
      (@pool&.spawned || 8).times { @release << true }
    end
    @server.stop(true)
  end

  def run_server(**options, &block)
    options[:log_writer]  ||= @log_writer
    options[:min_threads] ||= 1
    @server = Puma::Server.new(block || @app, @events, options)
    @control = Puma::ServerPluginControl.new(@server)
    @bind_port = (@server.add_tcp_listener @host, 0).addr[1]
    @server.run
  ensure
    @pool = @server.instance_variable_get(:@thread_pool)
  end

  # A gated app blocks each request until @release is signaled. `started`
  # receives once when a thread picks up a request and `completed` once when
  # it has finished, allowing tests to observe both states precisely.
  def gated_app(started, completed)
    ->(_env) do
      started << true
      @release.pop
      completed << true
      [200, {}, ["ok"]]
    end
  end

  def test_exposes_current_bounds
    run_server(min_threads: 2, max_threads: 5)

    assert_equal 2, @control.min_threads
    assert_equal 5, @control.max_threads
  end

  def test_update_thread_pool_min_max_delegates_to_server
    run_server(min_threads: 1, max_threads: 1)

    @control.update_thread_pool_min_max(min: 2, max: 4)

    assert_equal 2, @control.min_threads
    assert_equal 4, @control.max_threads
    assert_equal 2, @pool.min
    assert_equal 4, @pool.max
  end

  def test_update_keeps_omitted_bound
    run_server(min_threads: 2, max_threads: 4)

    @control.update_thread_pool_min_max(max: 3)
    assert_equal 2, @control.min_threads
    assert_equal 3, @control.max_threads

    @control.update_thread_pool_min_max(min: 1)
    assert_equal 1, @control.min_threads
    assert_equal 3, @control.max_threads
  end

  def test_invalid_bounds_raise_and_preserve_state
    run_server(min_threads: 2, max_threads: 4)

    assert_raises(ArgumentError) do
      @control.update_thread_pool_min_max(min: 5, max: 1)
    end

    assert_equal 2, @control.min_threads
    assert_equal 4, @control.max_threads
    assert_equal 2, @pool.min
    assert_equal 4, @pool.max
    assert_equal 2, @pool.spawned
  end

  def test_scale_up_processes_queued_requests_and_reports_completion
    started = Queue.new
    completed = Queue.new
    @release = Queue.new
    run_server(min_threads: 1, max_threads: 1, force_shutdown_after: 2,
      &gated_app(started, completed))

    # The first request occupies the only thread; the rest are buffered until
    # the pool grows and a thread is available to process them.
    requests = send_http_array 4
    started.pop
    Timeout.timeout(2) { sleep 0.01 until @release.num_waiting == 1 }
    assert_equal 1, @pool.spawned

    @control.update_thread_pool_min_max(min: 4, max: 4)

    # Growing the pool spawns threads that pick up the queued requests at once.
    3.times { started.pop }
    Timeout.timeout(2) { sleep 0.01 until @release.num_waiting == 4 }
    assert_equal 4, @pool.spawned

    4.times { @release << true }
    Timeout.timeout(2) { sleep 0.01 until completed.length == 4 }
    requests.each { |req| req.read_response }

    # New config, running threads and completed requests are all observable.
    assert_equal 4, @control.min_threads
    assert_equal 4, @control.max_threads
    assert_equal 4, @pool.spawned
    assert_equal 4, @server.running
    assert_equal 4, @server.requests_count
  end

  def test_scale_down_reclaims_idle_threads_but_finishes_busy_requests
    started = Queue.new
    completed = Queue.new
    @release = Queue.new
    # No auto_trim_time: only the explicit bounds update may trim threads.
    run_server(min_threads: 3, max_threads: 3, force_shutdown_after: 2,
      &gated_app(started, completed))

    busy = send_http_array 2
    2.times { started.pop }
    # Wait until both request threads are actually parked on the gate,
    # guaranteeing neither request can complete before the bounds change.
    Timeout.timeout(2) { sleep 0.01 until @release.num_waiting == 2 }
    assert_equal 3, @pool.spawned

    @control.update_thread_pool_min_max(min: 1, max: 1)

    # One idle thread is reclaimed; the two busy threads are not killed.
    Timeout.timeout(2) { sleep 0.01 until @pool.spawned == 2 }
    assert_equal 0, completed.length

    2.times { @release << true }
    busy.each { |req| req.read_response }

    # The in-flight requests run to completion and their threads then exit.
    Timeout.timeout(2) { sleep 0.01 until completed.length == 2 }
    Timeout.timeout(2) { sleep 0.01 until @pool.spawned == 1 }
    assert_equal 1, @pool.spawned
    assert_equal 2, @server.requests_count
  end
end
