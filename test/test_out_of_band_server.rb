# frozen_string_literal: true

require_relative "helper"
require_relative "helpers/test_puma/puma_socket"

class TestOutOfBandServer < PumaTest
  parallelize_me!

  include TestPuma
  include TestPuma::PumaSocket

  HOST = HOST4

  def setup
    @host = HOST
    @server = nil
    @oob_finished = ConditionVariable.new
    @app_finished = ConditionVariable.new
  end

  def teardown
    @app_released = true
    @oob_finished.broadcast
    @app_finished.broadcast
    @mutex&.synchronize { @parallel_release&.broadcast }
    @server&.stop true
  end

  def oob_server(**options)
    @request_count = 0
    @oob_count = 0
    in_oob = Mutex.new
    @mutex = Mutex.new
    oob_wait = options.delete(:oob_wait)
    oob = {
      block: -> do
        in_oob.synchronize do
          @mutex.synchronize do
            @oob_count += 1
            @oob_finished.signal
            @oob_finished.wait(@mutex, 1) if oob_wait
          end
        end
      end
    }
    app_wait = options.delete(:app_wait)
    app = ->(_) do
      raise 'OOB conflict' if in_oob.locked?
      @mutex.synchronize do
        @request_count += 1
        @app_finished.signal
        if app_wait
          count = @request_count
          @app_finished.wait(@mutex, 1) until @request_count > count || @app_released
        end
      end
      [200, {}, [""]]
    end

    options[:min_threads] ||= 1
    options[:max_threads] ||= 1
    options[:log_writer]  ||= Puma::LogWriter.strings

    @server = Puma::Server.new app, nil, out_of_band: [oob], **options
    @bind_port = (@server.add_tcp_listener @host, 0).addr[1]
    @server.run
    sleep 0.15 if Puma.jruby?
  end

  # A server carrying both kinds of out-of-band hooks. The parallel hook may
  # overlap requests; the quiescent hook must never overlap a request or the
  # parallel hook.
  def mixed_oob_server(**options)
    @request_count = 0
    @oob_count = 0
    @parallel_count = 0
    @in_hook = Mutex.new
    @mutex = Mutex.new
    parallel_wait = options.delete(:parallel_wait)
    quiescent_wait = options.delete(:quiescent_wait)
    @parallel_release = ConditionVariable.new

    quiescent = {
      block: -> do
        locked = @in_hook.try_lock
        raise 'Quiescent hook overlapped another hook' unless locked
        @mutex.synchronize do
          @oob_count += 1
          @oob_finished.signal
          @oob_finished.wait(@mutex, 1) if quiescent_wait
        end
      ensure
        @in_hook.unlock if locked
      end
    }
    parallel = {
      block: -> do
        locked = @in_hook.try_lock
        raise 'Parallel hook overlapped the quiescent hook' unless locked
        @mutex.synchronize do
          @parallel_count += 1
          @parallel_release.signal
          @parallel_release.wait(@mutex, 1) if parallel_wait
        end
      ensure
        @in_hook.unlock if locked
      end
    }
    app = ->(_) do
      @mutex.synchronize do
        @request_count += 1
        @app_finished.signal
      end
      [200, {}, [""]]
    end

    options[:min_threads] ||= 1
    options[:max_threads] ||= 2
    options[:log_writer]  ||= Puma::LogWriter.strings

    @server = Puma::Server.new app, nil,
      out_of_band: [quiescent],
      out_of_band_parallel: [parallel],
      **options
    @bind_port = (@server.add_tcp_listener @host, 0).addr[1]
    @server.run
    sleep 0.15 if Puma.jruby?
  end

  # Sequential requests should trigger out_of_band after every request.
  def test_sequential
    n = 100
    oob_server
    n.times do
      @mutex.synchronize do
        send_http GET_10
        @oob_finished.wait(@mutex, 1)
      end
    end
    assert_equal n, @request_count
    assert_equal n, @oob_count
  end

  # Stream of requests on concurrent connections should trigger
  # out_of_band hooks only once after the final request.
  def test_stream
    oob_server app_wait: true, max_threads: 2
    n = 100
    Array.new(n) { send_http GET_10 }
    Thread.pass until @request_count == n
    @mutex.synchronize do
      @app_released = true
      @app_finished.broadcast
      @oob_finished.wait(@mutex, 1)
    end
    assert_equal n, @request_count
    assert_equal 1, @oob_count
  end

  # New requests should not get processed while OOB is running.
  def test_request_overlapping_hook
    oob_server oob_wait: true, max_threads: 2

    # Establish connection for Req2 before OOB
    req2 = new_socket
    sleep 0.01

    @mutex.synchronize do
      send_http GET_10
      @oob_finished.wait(@mutex) # enter OOB

      # Send Req2
      req2 << GET_10
      # If Req2 is processed now it raises 'OOB Conflict' in the response.
      sleep 0.01

      @oob_finished.signal # exit OOB
      # Req2 should be processed now.
      @oob_finished.wait(@mutex, 1) # enter OOB
      @oob_finished.signal # exit OOB
    end

    refute_match(/OOB conflict/, req2.read)
  end

  # Partial requests should not trigger OOB.
  def test_partial_request
    oob_server
    new_socket.close
    sleep 0.01
    assert_equal 0, @oob_count
  end

  # OOB should be triggered following a completed request
  # concurrent with other partial requests.
  def test_partial_concurrent
    oob_server max_threads: 2
    @mutex.synchronize do
      send_http GET_10
      100.times { new_socket.close }
      @oob_finished.wait(@mutex, 1)
    end
    assert_equal 1, @oob_count
  end

  # OOB should block new connections from being accepted.
  def test_blocks_new_connection
    oob_server oob_wait: true, max_threads: 2
    @mutex.synchronize do
      send_http GET_10
      @oob_finished.wait(@mutex)
    end
    accepted = false
    io = @server.binder.ios.last
    io.stub(:accept_nonblock, -> { accepted = true; new_socket }) do
      new_socket.close
      sleep 0.01
    end
    refute accepted, 'New connection accepted during out of band'
  end

  # Sequential requests trigger both hooks after each request, and the two
  # hook kinds never overlap each other or a request.
  def test_mixed_hooks_sequential_requests
    mixed_oob_server
    n = 20
    n.times do |i|
      send_http GET_10
      target = i + 1
      Timeout.timeout(2) do
        @mutex.synchronize do
          @parallel_release.wait(@mutex) until @parallel_count >= target
          @oob_finished.wait(@mutex) until @oob_count >= target
        end
      end
    end
    assert_equal n, @request_count
    assert_equal n, @parallel_count
    assert_equal n, @oob_count
  end

  # While a parallel hook is stuck, new requests must still be accepted and
  # processed on another pool thread. The quiescent hook waits until the
  # parallel hook has finished.
  def test_parallel_hook_does_not_block_requests
    mixed_oob_server parallel_wait: true, max_threads: 2

    # First request triggers the parallel hook, which then blocks.
    @mutex.synchronize do
      send_http GET_10
      @parallel_release.wait(@mutex, 1) until @parallel_count == 1
    end
    assert_predicate @server.instance_variable_get(:@thread_pool), :out_of_band_parallel_running
    assert_equal 0, @oob_count, 'Quiescent hook ran while parallel hook was active'

    # A second request must still complete despite the stuck parallel hook.
    Timeout.timeout(2) do
      send_http GET_10
      @mutex.synchronize { @app_finished.wait(@mutex, 1) until @request_count == 2 }
    end
    assert_equal 2, @request_count

    # Releasing the parallel hook eventually lets the quiescent hook run.
    @mutex.synchronize do
      @parallel_release.broadcast
      @oob_finished.wait(@mutex, 2) until @oob_count >= 1
    end
    assert_operator @oob_count, :>=, 1
  end
end
