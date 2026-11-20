# coding: US-ASCII
# frozen_string_literal: false
require_relative "utils"
require "webrick"
require "test/unit"

class TestWEBrickCGI < Test::Unit::TestCase
  CRLF = "\r\n"

  def teardown
    WEBrick::Utils::TimeoutHandler.terminate
    super
  end

  def test_cgi
    TestWEBrick.start_cgi_server{|server, addr, port, log|
      http = Net::HTTP.new(addr, port)
      req = Net::HTTP::Get.new("/webrick.cgi")
      http.request(req){|res| assert_equal("/webrick.cgi", res.body, log.call)}
      req = Net::HTTP::Get.new("/webrick.cgi/path/info")
      http.request(req){|res| assert_equal("/path/info", res.body, log.call)}
      req = Net::HTTP::Get.new("/webrick.cgi/%3F%3F%3F?foo=bar")
      http.request(req){|res| assert_equal("/???", res.body, log.call)}
      unless RUBY_PLATFORM =~ /mswin|mingw|cygwin|bccwin32|java/
        # Path info of res.body is passed via ENV.
        # ENV[] returns different value on Windows depending on locale.
        req = Net::HTTP::Get.new("/webrick.cgi/%A4%DB%A4%B2/%A4%DB%A4%B2")
        http.request(req){|res|
          assert_equal("/\xA4\xDB\xA4\xB2/\xA4\xDB\xA4\xB2", res.body, log.call)}
      end
      req = Net::HTTP::Get.new("/webrick.cgi?a=1;a=2;b=x")
      http.request(req){|res| assert_equal("a=1, a=2, b=x", res.body, log.call)}
      req = Net::HTTP::Get.new("/webrick.cgi?a=1&a=2&b=x")
      http.request(req){|res| assert_equal("a=1, a=2, b=x", res.body, log.call)}

      req = Net::HTTP::Post.new("/webrick.cgi?a=x;a=y;b=1")
      req["Content-Type"] = "application/x-www-form-urlencoded"
      http.request(req, "a=1;a=2;b=x"){|res|
        assert_equal("a=1, a=2, b=x", res.body, log.call)}
      req = Net::HTTP::Post.new("/webrick.cgi?a=x&a=y&b=1")
      req["Content-Type"] = "application/x-www-form-urlencoded"
      http.request(req, "a=1&a=2&b=x"){|res|
        assert_equal("a=1, a=2, b=x", res.body, log.call)}
      req = Net::HTTP::Get.new("/")
      http.request(req){|res|
        ary = res.body.lines.to_a
        assert_match(%r{/$}, ary[0], log.call)
        assert_match(%r{/webrick.cgi$}, ary[1], log.call)
      }

      req = Net::HTTP::Get.new("/webrick.cgi")
      req["Cookie"] = "CUSTOMER=WILE_E_COYOTE; PART_NUMBER=ROCKET_LAUNCHER_0001"
      http.request(req){|res|
        assert_equal(
          "CUSTOMER=WILE_E_COYOTE\nPART_NUMBER=ROCKET_LAUNCHER_0001\n",
          res.body, log.call)
      }

      req = Net::HTTP::Get.new("/webrick.cgi")
      cookie =  %{$Version="1"; }
      cookie << %{Customer="WILE_E_COYOTE"; $Path="/acme"; }
      cookie << %{Part_Number="Rocket_Launcher_0001"; $Path="/acme"; }
      cookie << %{Shipping="FedEx"; $Path="/acme"}
      req["Cookie"] = cookie
      http.request(req){|res|
        assert_equal("Customer=WILE_E_COYOTE, Shipping=FedEx",
                     res["Set-Cookie"], log.call)
        assert_equal("Customer=WILE_E_COYOTE\n" +
                     "Part_Number=Rocket_Launcher_0001\n" +
                     "Shipping=FedEx\n", res.body, log.call)
      }
    }
  end

  def test_bad_request
    log_tester = lambda {|log, access_log|
      assert_match(/BadRequest/, log.join)
    }
    TestWEBrick.start_cgi_server({}, log_tester) {|server, addr, port, log|
      sock = TCPSocket.new(addr, port)
      begin
        sock << "POST /webrick.cgi HTTP/1.0" << CRLF
        sock << "Content-Type: application/x-www-form-urlencoded" << CRLF
        sock << "Content-Length: 1024" << CRLF
        sock << CRLF
        sock << "a=1&a=2&b=x"
        sock.close_write
        assert_match(%r{\AHTTP/\d.\d 400 Bad Request}, sock.read, log.call)
      ensure
        sock.close
      end
    }
  end

  def test_cgi_env
    TestWEBrick.start_cgi_server do |server, addr, port, log|
      http = Net::HTTP.new(addr, port)
      req = Net::HTTP::Get.new("/webrick.cgi/dumpenv")
      req['proxy'] = 'http://example.com/'
      req['hello'] = 'world'
      http.request(req) do |res|
        env = Marshal.load(res.body)
        assert_equal 'world', env['HTTP_HELLO']
        assert_not_operator env, :include?, 'HTTP_PROXY'
      end
    end
  end

  CtrlSeq = [0x7f, *(1..31)].pack("C*").gsub(/\s+/, '')
  CtrlPat = /#{Regexp.quote(CtrlSeq)}/o
  DumpPat = /#{Regexp.quote(CtrlSeq.dump[1...-1])}/o

  def test_bad_uri
    log_tester = lambda {|log, access_log|
      assert_equal(1, log.length)
      assert_match(/ERROR bad URI/, log[0])
    }
    TestWEBrick.start_cgi_server({}, log_tester) {|server, addr, port, log|
      res = TCPSocket.open(addr, port) {|sock|
        sock << "GET /#{CtrlSeq}#{CRLF}#{CRLF}"
        sock.close_write
        sock.read
      }
      assert_match(%r{\AHTTP/\d.\d 400 Bad Request}, res)
      s = log.call.each_line.grep(/ERROR bad URI/)[0]
      assert_match(DumpPat, s)
      assert_not_match(CtrlPat, s)
    }
  end

  def test_bad_header
    log_tester = lambda {|log, access_log|
      assert_equal(1, log.length)
      assert_match(/ERROR bad header/, log[0])
    }
    TestWEBrick.start_cgi_server({}, log_tester) {|server, addr, port, log|
      res = TCPSocket.open(addr, port) {|sock|
        sock << "GET / HTTP/1.0#{CRLF}#{CtrlSeq}#{CRLF}#{CRLF}"
        sock.close_write
        sock.read
      }
      assert_match(%r{\AHTTP/\d.\d 400 Bad Request}, res)
      s = log.call.each_line.grep(/ERROR bad header/)[0]
      assert_match(DumpPat, s)
      assert_not_match(CtrlPat, s)
    }
  end

  # A CGI may emit a large response before it consumes the request body.
  # The handler must pump stdin, stdout and stderr concurrently instead of
  # filling a pipe buffer and deadlocking either end.
  def test_cgi_output_before_consuming_input
    TestWEBrick.start_cgi_server {|server, addr, port, log|
      http = Net::HTTP.new(addr, port)
      http.read_timeout = 10
      body = "z" * (2 * 1024 * 1024)
      req = Net::HTTP::Post.new("/webrick_eager.cgi")
      req["Content-Type"] = "application/octet-stream"
      res = http.request(req, body)
      assert_equal("200", res.code, log.call)
      expected = "x" * (2 * 1024 * 1024) + body
      assert_equal(expected.bytesize, res.body.bytesize, log.call)
      assert_equal(expected, res.body, log.call)
    }
  end

  # stderr is delivered on its own pipe: a chatty script must neither block
  # the response nor let diagnostics leak into it.
  def test_cgi_large_stderr_does_not_corrupt_response
    log_tester = lambda {|log, access_log|
      assert_match(/webrick_stderr\.cgi/, log.join)
    }
    TestWEBrick.start_cgi_server({}, log_tester) {|server, addr, port, log|
      http = Net::HTTP.new(addr, port)
      http.read_timeout = 10
      req = Net::HTTP::Post.new("/webrick_stderr.cgi")
      req["Content-Type"] = "application/octet-stream"
      res = http.request(req, "input" * 1024)
      assert_equal("200", res.code, log.call)
      assert_equal("stderr-done", res.body, log.call)
      # Diagnostics must stay out of the business response: the 2MB stream
      # of repeated "e" would dwarf the short body if it leaked through.
      assert_equal("stderr-done".bytesize, res.body.bytesize, log.call)
    }
  end

  # When pumping fails while the CGI is still running the handler must
  # terminate the process and reclaim every pump thread and pipe.  The slow
  # CGI ignores TERM, exercising the forced-kill escalation as well.
  def test_cgi_cancellation_reaps_process_threads_and_pipes
    script = File.expand_path("webrick_slow.cgi", __dir__)
    pid_file = File.expand_path("webrick_slow_cgi.pid", __dir__)
    File.unlink(pid_file) if File.exist?(pid_file)
    TestWEBrick.start_cgi_server do |server, addr, port, log|
      servlet = WEBrick::HTTPServlet::CGIHandler.new(server, script)
      # The request body fails while being pumped, modelling a client that
      # vanishes (or times out) mid request.
      req = FailingBodyRequest.new(
        {
          "SCRIPT_FILENAME" => script,
          "REQUEST_METHOD" => "POST",
          "CONTENT_LENGTH" => "64",
        },
        pid_file)
      res = WEBrick::HTTPResponse.new(server.config)

      before_threads = Thread.list.size
      before_ios = live_io_count

      assert_raise(IOError) do
        servlet.do_POST(req, res)
      end

      assert_equal(before_threads, Thread.list.size,
                   "pump threads leaked: #{log.call}")
      assert_equal(before_ios, live_io_count,
                   "pipe IOs leaked: #{log.call}")
      assert_process_reaped(pid_file)
    end
  ensure
    File.unlink(pid_file) if pid_file && File.exist?(pid_file)
  end

  # Normal completion leaves no residual pump threads or descriptors.  Two
  # sequential requests must converge on the same steady state, which is
  # robust against the server's own connection threads.
  def test_cgi_normal_exit_reaps_threads_and_pipes
    TestWEBrick.start_cgi_server do |server, addr, port, log|
      http = Net::HTTP.new(addr, port)
      res = http.request(Net::HTTP::Get.new("/webrick.cgi"))
      assert_equal("200", res.code, log.call)
      assert_equal("/webrick.cgi", res.body, log.call)

      # Let the request's connection and pump threads settle, then record
      # the steady-state number of threads and open IOs.
      sleep 0.5
      baseline_threads = Thread.list.size
      baseline_ios = live_io_count

      res = http.request(Net::HTTP::Get.new("/webrick.cgi"))
      assert_equal("200", res.code, log.call)

      # A second request must not leave any extra thread or descriptor.
      wait_until { Thread.list.size <= baseline_threads }
      assert_equal(baseline_ios, live_io_count,
                   "pipe IOs leaked: #{log.call}")
    end
  end

  private

  def live_io_count
    ObjectSpace.each_object(IO).count { |io| !io.closed? }
  end

  def wait_until(max_seconds = 10)
    deadline = Time.now + max_seconds
    until yield
      flunk("timed out waiting for cleanup") if Time.now >= deadline
      sleep 0.05
    end
  end

  # The slow CGI writes its PID to +pid_file+; after cancellation that
  # process must no longer exist.
  def assert_process_reaped(pid_file)
    pid = nil
    wait_until do
      if File.exist?(pid_file) && File.binread(pid_file) =~ /\A(\d+)\z/
        pid = $1.to_i
      end
      !pid.nil?
    end
    wait_until { !process_alive?(pid) }
  end

  def process_alive?(pid)
    if /mswin|bccwin|mingw/ =~ RUBY_PLATFORM
      output = `tasklist /fi "PID eq #{pid}" /fo csv /nh 2>#{File::NULL}`
      output.include?(pid.to_s)
    else
      Process.kill(0, pid)
      true
    end
  rescue Errno::ESRCH, Errno::EPERM
    false
  end

  # Minimal stand-in for WEBrick::HTTPRequest exposing only what CGIHandler
  # touches: meta_vars and a body enumerator that fails while being pumped.
  # It waits for the CGI process to start (signalled by its PID file) before
  # simulating the client disconnect, so the failure cannot race process
  # startup under a slow test harness.
  class FailingBodyRequest
    def initialize(meta, pid_file)
      @meta = meta
      @pid_file = pid_file
    end

    def meta_vars
      @meta.dup
    end

    def body
      deadline = Time.now + 10
      until File.exist?(@pid_file)
        raise IOError, "CGI process never started" if Time.now >= deadline
        sleep 0.05
      end
      raise IOError, "client disconnected while sending request body"
    end
  end
end
