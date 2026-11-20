# frozen_string_literal: false
require "test/unit"
require_relative "utils.rb"
require "webrick"
require "stringio"
require "tmpdir"

class WEBrick::TestFileHandler < Test::Unit::TestCase
  def teardown
    WEBrick::Utils::TimeoutHandler.terminate
    super
  end

  def default_file_handler(filename)
    klass = WEBrick::HTTPServlet::DefaultFileHandler
    klass.new(WEBrick::Config::HTTP, filename)
  end

  def coalescing_file_handler(filename)
    config = WEBrick::Config::HTTP.dup.update(:CoalesceRanges => true)
    WEBrick::HTTPServlet::DefaultFileHandler.new(config, filename)
  end

  def windows?
    File.directory?("\\")
  end

  def get_res_body(res)
    sio = StringIO.new
    sio.binmode
    res.send_body(sio)
    sio.string
  end

  def make_range_request(range_spec)
    msg = <<~HTTP.gsub("\n", "\r\n")
      GET / HTTP/1.0
      Range: #{range_spec}

    HTTP
    return StringIO.new(msg)
  end

  def make_range_response(file, range_spec)
    req = WEBrick::HTTPRequest.new(WEBrick::Config::HTTP)
    req.parse(make_range_request(range_spec))
    res = WEBrick::HTTPResponse.new(WEBrick::Config::HTTP)
    size = File.size(file)
    handler = default_file_handler(file)
    handler.make_partial_content(req, res, file, size)
    return res
  end

  def make_coalesced_range_response(file, range_spec)
    req = WEBrick::HTTPRequest.new(WEBrick::Config::HTTP)
    req.parse(make_range_request(range_spec))
    res = WEBrick::HTTPResponse.new(WEBrick::Config::HTTP)
    size = File.size(file)
    handler = coalescing_file_handler(file)
    handler.make_partial_content(req, res, file, size)
    return res
  end

  def test_make_partial_content
    filename = __FILE__
    filesize = File.size(filename)

    res = make_range_response(filename, "bytes=#{filesize-100}-")
    assert_match(%r{^text/plain}, res["content-type"])
    assert_equal(100, get_res_body(res).size)

    res = make_range_response(filename, "bytes=-100")
    assert_match(%r{^text/plain}, res["content-type"])
    assert_equal(100, get_res_body(res).size)

    res = make_range_response(filename, "bytes=0-99")
    assert_match(%r{^text/plain}, res["content-type"])
    assert_equal(100, get_res_body(res).size)

    res = make_range_response(filename, "bytes=100-199")
    assert_match(%r{^text/plain}, res["content-type"])
    assert_equal(100, get_res_body(res).size)

    res = make_range_response(filename, "bytes=0-0")
    assert_match(%r{^text/plain}, res["content-type"])
    assert_equal(1, get_res_body(res).size)

    res = make_range_response(filename, "bytes=-1")
    assert_match(%r{^text/plain}, res["content-type"])
    assert_equal(1, get_res_body(res).size)

    res = make_range_response(filename, "bytes=0-0, -2")
    assert_match(%r{^multipart/byteranges}, res["content-type"])
    body = get_res_body(res)
    boundary = /; boundary=(.+)/.match(res['content-type'])[1]
    off = filesize - 2
    last = filesize - 1

    exp = "--#{boundary}\r\n" \
          "Content-Type: text/plain\r\n" \
          "Content-Range: bytes 0-0/#{filesize}\r\n" \
          "\r\n" \
          "#{File.read(__FILE__, 1)}\r\n" \
          "--#{boundary}\r\n" \
          "Content-Type: text/plain\r\n" \
          "Content-Range: bytes #{off}-#{last}/#{filesize}\r\n" \
          "\r\n" \
          "#{File.read(__FILE__, 2, off)}\r\n" \
          "--#{boundary}--\r\n"
    assert_equal exp, body
  end

  def test_coalesce_ranges
    handler = coalescing_file_handler(__FILE__)

    # overlapping ranges are merged into one ordered range
    assert_equal([1..8], handler.coalesce_ranges([1..4, 3..8], 100))
    # ranges may arrive out of order
    assert_equal([1..8], handler.coalesce_ranges([3..8, 1..4], 100))
    # adjacent ranges are merged as well
    assert_equal([1..8], handler.coalesce_ranges([1..4, 5..8], 100))
    # disjoint ranges stay separate, sorted and non-overlapping
    assert_equal([1..3, 7..9], handler.coalesce_ranges([7..9, 1..3], 100))
    # ranges extending past the end of the file are clamped
    assert_equal([1..9], handler.coalesce_ranges([1..999], 10))
    # suffix ranges are resolved against the file size
    assert_equal([7..9], handler.coalesce_ranges([-3..-1], 10))
    # unsatisfiable ranges are dropped; an entirely unsatisfiable set is empty
    assert_equal([1..2], handler.coalesce_ranges([1..2, 100..200], 10))
    assert_equal([], handler.coalesce_ranges([100..200], 10))
    assert_equal([], handler.coalesce_ranges([-100..-1], 5))
    # many overlapping/adjacent ranges normalize into ordered disjoint ones
    assert_equal([1..8, 20..22],
                 handler.coalesce_ranges([20..22, 1..3, 3..5, 4..8], 100))
    # identical ranges collapse into one
    assert_equal([2..4], handler.coalesce_ranges([2..4, 2..4], 100))
  end

  # Creates a short file of +data+ inside a temporary directory and yields
  # its path.
  def with_byte_file(data)
    Dir.mktmpdir do |dir|
      file = File.join(dir, "bytes.bin")
      File.binwrite(file, data)
      yield(file)
    end
  end

  # ten bytes with known values: "ABCDEFGHIJ"
  BYTE_FILE_DATA = (0...10).collect { |i| "A".ord + i }.pack("C*").freeze

  def test_make_partial_content_with_coalesced_ranges
    with_byte_file(BYTE_FILE_DATA) do |file|
      data = BYTE_FILE_DATA

      # overlapping ranges collapse to a single range, served as a
      # single-part response rather than a multipart one
      res = make_coalesced_range_response(file, "bytes=1-3, 2-5")
      assert_equal("application/octet-stream", res["content-type"])
      assert_equal("bytes 1-5/10", res["content-range"])
      assert_equal("5", res["content-length"])
      assert_equal(data[1..5], get_res_body(res))

      # adjacent ranges are likewise merged into one range
      res = make_coalesced_range_response(file, "bytes=0-2, 3-5")
      assert_equal("bytes 0-5/10", res["content-range"])
      assert_equal("6", res["content-length"])
      assert_equal(data[0..5], get_res_body(res))

      # suffix ranges participate in the merge
      res = make_coalesced_range_response(file, "bytes=-4")
      assert_equal("bytes 6-9/10", res["content-range"])
      assert_equal("4", res["content-length"])
      assert_equal(data[6..9], get_res_body(res))

      res = make_coalesced_range_response(file, "bytes=5-7, -3")
      assert_equal("bytes 5-9/10", res["content-range"])
      assert_equal("5", res["content-length"])
      assert_equal(data[5..9], get_res_body(res))

      # ranges that extend beyond the file are clamped before merging
      res = make_coalesced_range_response(file, "bytes=7-100")
      assert_equal("bytes 7-9/10", res["content-range"])
      assert_equal("3", res["content-length"])
      assert_equal(data[7..9], get_res_body(res))

      # a satisfiable range combined with an unsatisfiable one is still served
      res = make_coalesced_range_response(file, "bytes=0-1, 100-200")
      assert_equal("bytes 0-1/10", res["content-range"])
      assert_equal("2", res["content-length"])
      assert_equal(data[0..1], get_res_body(res))

      # genuinely disjoint ranges remain multipart, with coalesced part list
      res = make_coalesced_range_response(file, "bytes=1-2, 7-8")
      assert_match(%r{^multipart/byteranges}, res["content-type"])
      boundary = /; boundary=(.+)/.match(res['content-type'])[1]
      exp = "--#{boundary}\r\n" \
            "Content-Type: application/octet-stream\r\n" \
            "Content-Range: bytes 1-2/10\r\n" \
            "\r\n" \
            "#{data[1..2]}\r\n" \
            "--#{boundary}\r\n" \
            "Content-Type: application/octet-stream\r\n" \
            "Content-Range: bytes 7-8/10\r\n" \
            "\r\n" \
            "#{data[7..8]}\r\n" \
            "--#{boundary}--\r\n"
      assert_equal(exp, get_res_body(res))

      # when nothing is satisfiable the request is rejected with 416
      assert_raise(WEBrick::HTTPStatus::RequestRangeNotSatisfiable) do
        make_coalesced_range_response(file, "bytes=100-200")
      end
    end
  end

  def test_make_partial_content_without_coalescing_keeps_multipart
    with_byte_file(BYTE_FILE_DATA) do |file|
      # with the option disabled, overlapping ranges are still emitted as
      # separate multipart parts (the historical behavior)
      res = make_range_response(file, "bytes=1-3, 2-5")
      assert_match(%r{^multipart/byteranges}, res["content-type"])
      # sending the body also closes the file handle kept by the lambda
      get_res_body(res)
    end
  end

  def test_filehandler
    config = { :DocumentRoot => File.dirname(__FILE__), }
    this_file = File.basename(__FILE__)
    filesize = File.size(__FILE__)
    this_data = File.binread(__FILE__)
    range = nil
    bug2593 = '[ruby-dev:40030]'

    TestWEBrick.start_httpserver(config) do |server, addr, port, log|
      begin
        server[:DocumentRootOptions][:NondisclosureName] = []
        http = Net::HTTP.new(addr, port)
        req = Net::HTTP::Get.new("/")
        http.request(req){|res|
          assert_equal("200", res.code, log.call)
          assert_equal("text/html", res.content_type, log.call)
          assert_match(/HREF="#{this_file}"/, res.body, log.call)
        }
        req = Net::HTTP::Get.new("/#{this_file}")
        http.request(req){|res|
          assert_equal("200", res.code, log.call)
          assert_equal("text/plain", res.content_type, log.call)
          assert_equal(this_data, res.body, log.call)
        }

        req = Net::HTTP::Get.new("/#{this_file}", "range"=>"bytes=#{filesize-100}-")
        http.request(req){|res|
          assert_equal("206", res.code, log.call)
          assert_equal("text/plain", res.content_type, log.call)
          assert_nothing_raised(bug2593) {range = res.content_range}
          assert_equal((filesize-100)..(filesize-1), range, log.call)
          assert_equal(this_data[-100..-1], res.body, log.call)
        }

        req = Net::HTTP::Get.new("/#{this_file}", "range"=>"bytes=-100")
        http.request(req){|res|
          assert_equal("206", res.code, log.call)
          assert_equal("text/plain", res.content_type, log.call)
          assert_nothing_raised(bug2593) {range = res.content_range}
          assert_equal((filesize-100)..(filesize-1), range, log.call)
          assert_equal(this_data[-100..-1], res.body, log.call)
        }

        req = Net::HTTP::Get.new("/#{this_file}", "range"=>"bytes=0-99")
        http.request(req){|res|
          assert_equal("206", res.code, log.call)
          assert_equal("text/plain", res.content_type, log.call)
          assert_nothing_raised(bug2593) {range = res.content_range}
          assert_equal(0..99, range, log.call)
          assert_equal(this_data[0..99], res.body, log.call)
        }

        req = Net::HTTP::Get.new("/#{this_file}", "range"=>"bytes=100-199")
        http.request(req){|res|
          assert_equal("206", res.code, log.call)
          assert_equal("text/plain", res.content_type, log.call)
          assert_nothing_raised(bug2593) {range = res.content_range}
          assert_equal(100..199, range, log.call)
          assert_equal(this_data[100..199], res.body, log.call)
        }

        req = Net::HTTP::Get.new("/#{this_file}", "range"=>"bytes=0-0")
        http.request(req){|res|
          assert_equal("206", res.code, log.call)
          assert_equal("text/plain", res.content_type, log.call)
          assert_nothing_raised(bug2593) {range = res.content_range}
          assert_equal(0..0, range, log.call)
          assert_equal(this_data[0..0], res.body, log.call)
        }

        req = Net::HTTP::Get.new("/#{this_file}", "range"=>"bytes=-1")
        http.request(req){|res|
          assert_equal("206", res.code, log.call)
          assert_equal("text/plain", res.content_type, log.call)
          assert_nothing_raised(bug2593) {range = res.content_range}
          assert_equal((filesize-1)..(filesize-1), range, log.call)
          assert_equal(this_data[-1, 1], res.body, log.call)
        }

        req = Net::HTTP::Get.new("/#{this_file}", "range"=>"bytes=0-0, -2")
        http.request(req){|res|
          assert_equal("206", res.code, log.call)
          assert_equal("multipart/byteranges", res.content_type, log.call)
        }
      ensure
        server[:DocumentRootOptions].delete :NondisclosureName
      end
    end
  end

  def test_non_disclosure_name
    config = { :DocumentRoot => File.dirname(__FILE__), }
    log_tester = lambda {|log, access_log|
      log = log.reject {|s| /ERROR '.*' not found\./ =~ s }
      log = log.reject {|s| /WARN  the request refers nondisclosure name/ =~ s }
      assert_equal([], log)
    }
    this_file = File.basename(__FILE__)
    TestWEBrick.start_httpserver(config, log_tester) do |server, addr, port, log|
      http = Net::HTTP.new(addr, port)
      doc_root_opts = server[:DocumentRootOptions]
      doc_root_opts[:NondisclosureName] = %w(.ht* *~ test_*)
      req = Net::HTTP::Get.new("/")
      http.request(req){|res|
        assert_equal("200", res.code, log.call)
        assert_equal("text/html", res.content_type, log.call)
        assert_no_match(/HREF="#{File.basename(__FILE__)}"/, res.body)
      }
      req = Net::HTTP::Get.new("/#{this_file}")
      http.request(req){|res|
        assert_equal("404", res.code, log.call)
      }
      doc_root_opts[:NondisclosureName] = %w(.ht* *~ TEST_*)
      http.request(req){|res|
        assert_equal("404", res.code, log.call)
      }
    end
  end

  def test_directory_traversal
    return if File.executable?(__FILE__) # skip on strange file system

    config = { :DocumentRoot => File.dirname(__FILE__), }
    log_tester = lambda {|log, access_log|
      log = log.reject {|s| /ERROR bad URI/ =~ s }
      log = log.reject {|s| /ERROR '.*' not found\./ =~ s }
      assert_equal([], log)
    }
    TestWEBrick.start_httpserver(config, log_tester) do |server, addr, port, log|
      http = Net::HTTP.new(addr, port)
      req = Net::HTTP::Get.new("/../../")
      http.request(req){|res| assert_equal("400", res.code, log.call) }
      req = Net::HTTP::Get.new("/..%5c../#{File.basename(__FILE__)}")
      http.request(req){|res| assert_equal(windows? ? "200" : "404", res.code, log.call) }
      req = Net::HTTP::Get.new("/..%5c..%5cruby.c")
      http.request(req){|res| assert_equal("404", res.code, log.call) }
    end
  end

  def test_unwise_in_path
    if windows?
      config = { :DocumentRoot => File.dirname(__FILE__), }
      TestWEBrick.start_httpserver(config) do |server, addr, port, log|
        http = Net::HTTP.new(addr, port)
        req = Net::HTTP::Get.new("/..%5c..")
        http.request(req){|res| assert_equal("301", res.code, log.call) }
      end
    end
  end

  def test_short_filename
    return if File.executable?(__FILE__) # skip on strange file system

    log_tester = lambda {|log, access_log|
      log = log.reject {|s| /ERROR '.*' not found\./ =~ s }
      log = log.reject {|s| /WARN  the request refers nondisclosure name/ =~ s }
      assert_equal([], log)
    }
    TestWEBrick.start_cgi_server({}, log_tester) do |server, addr, port, log|
      http = Net::HTTP.new(addr, port)
      if windows?
        root = File.dirname(__FILE__).tr("/", "\\")
        fname = IO.popen(%W[dir /x #{root}\\webrick_long_filename.cgi], encoding: "binary", &:read)
        fname.sub!(/\A.*$^$.*$^$/m, '')
        if fname
          fname = fname[/\s(w.+?cgi)\s/i, 1]
          fname.downcase!
        end
      else
        fname = "webric~1.cgi"
      end
      req = Net::HTTP::Get.new("/#{fname}/test")
      http.request(req) do |res|
        if windows?
          assert_equal("200", res.code, log.call)
          assert_equal("/test", res.body, log.call)
        else
          assert_equal("404", res.code, log.call)
        end
      end

      req = Net::HTTP::Get.new("/.htaccess")
      http.request(req) {|res| assert_equal("404", res.code, log.call) }
      req = Net::HTTP::Get.new("/htacce~1")
      http.request(req) {|res| assert_equal("404", res.code, log.call) }
      req = Net::HTTP::Get.new("/HTACCE~1")
      http.request(req) {|res| assert_equal("404", res.code, log.call) }
    end
  end

  def test_multibyte_char_in_path
    if Encoding.default_external == Encoding.find('US-ASCII')
      reset_encoding = true
      verb = $VERBOSE
      $VERBOSE = false
      Encoding.default_external = Encoding.find('UTF-8')
    end

    c = "\u00a7"
    begin
      c = c.encode('filesystem')
    rescue EncodingError
      c = c.b
    end
    Dir.mktmpdir(c) do |dir|
      basename = "#{c}.txt"
      File.write("#{dir}/#{basename}", "test_multibyte_char_in_path")
      Dir.mkdir("#{dir}/#{c}")
      File.write("#{dir}/#{c}/#{basename}", "nested")
      config = {
        :DocumentRoot => dir,
        :DirectoryIndex => [basename],
      }
      TestWEBrick.start_httpserver(config) do |server, addr, port, log|
        http = Net::HTTP.new(addr, port)
        path = "/#{basename}"
        req = Net::HTTP::Get.new(WEBrick::HTTPUtils::escape(path))
        http.request(req){|res| assert_equal("200", res.code, log.call + "\nFilesystem encoding is #{Encoding.find('filesystem')}") }
        path = "/#{c}/#{basename}"
        req = Net::HTTP::Get.new(WEBrick::HTTPUtils::escape(path))
        http.request(req){|res| assert_equal("200", res.code, log.call) }
        req = Net::HTTP::Get.new('/')
        http.request(req){|res|
          assert_equal("test_multibyte_char_in_path", res.body, log.call)
        }
      end
    end
  ensure
    if reset_encoding
      Encoding.default_external = Encoding.find('US-ASCII')
      $VERBOSE = verb
    end
  end

  def test_script_disclosure
    return if File.executable?(__FILE__) # skip on strange file system

    config = {
      :CGIInterpreter => TestWEBrick::RubyBinArray,
      :DocumentRoot => File.dirname(__FILE__),
      :CGIPathEnv => ENV['PATH'],
      :RequestCallback => Proc.new{|req, res|
        def req.meta_vars
          meta = super
          meta["RUBYLIB"] = $:.join(File::PATH_SEPARATOR)
          meta[RbConfig::CONFIG['LIBPATHENV']] = ENV[RbConfig::CONFIG['LIBPATHENV']] if RbConfig::CONFIG['LIBPATHENV']
          return meta
        end
      },
    }
    log_tester = lambda {|log, access_log|
      log = log.reject {|s| /ERROR '.*' not found\./ =~ s }
      assert_equal([], log)
    }
    TestWEBrick.start_httpserver(config, log_tester) do |server, addr, port, log|
      http = Net::HTTP.new(addr, port)
      http.read_timeout = EnvUtil.apply_timeout_scale(60)
      http.write_timeout = EnvUtil.apply_timeout_scale(60) if http.respond_to?(:write_timeout=)

      req = Net::HTTP::Get.new("/webrick.cgi/test")
      http.request(req) do |res|
        assert_equal("200", res.code, log.call)
        assert_equal("/test", res.body, log.call)
      end

      resok = windows?
      response_assertion = Proc.new do |res|
        if resok
          assert_equal("200", res.code, log.call)
          assert_equal("/test", res.body, log.call)
        else
          assert_equal("404", res.code, log.call)
        end
      end
      req = Net::HTTP::Get.new("/webrick.cgi%20/test")
      http.request(req, &response_assertion)
      req = Net::HTTP::Get.new("/webrick.cgi./test")
      http.request(req, &response_assertion)
      resok &&= File.exist?(__FILE__+"::$DATA")
      req = Net::HTTP::Get.new("/webrick.cgi::$DATA/test")
      http.request(req, &response_assertion)
    end
  end

  def test_erbhandler
    config = { :DocumentRoot => File.dirname(__FILE__) }
    log_tester = lambda {|log, access_log|
      log = log.reject {|s| /ERROR '.*' not found\./ =~ s }
      assert_equal([], log)
    }
    TestWEBrick.start_httpserver(config, log_tester) do |server, addr, port, log|
      http = Net::HTTP.new(addr, port)
      req = Net::HTTP::Get.new("/webrick.rhtml")
      http.request(req) do |res|
        assert_equal("200", res.code, log.call)
        assert_match %r!\Areq to http://[^/]+/webrick\.rhtml {}\n!, res.body
      end
    end
  end

  # Issues raw HTTP/1.1 requests over a plain socket and returns the whole
  # response so the exact bytes sent for a range can be inspected.
  def raw_http_request(addr, port, method, path, range)
    socket = TCPSocket.new(addr, port)
    socket.binmode
    request = "#{method} #{path} HTTP/1.1\r\n" \
              "Host: #{addr}:#{port}\r\n" \
              "Range: #{range}\r\n" \
              "Connection: close\r\n" \
              "\r\n"
    socket.write(request)
    response = +""
    while chunk = socket.read(1024)
      response << chunk
    end
    response
  ensure
    socket&.close
  end

  def raw_header(response)
    response.split("\r\n\r\n", 2).first
  end

  def raw_body(response)
    response.split("\r\n\r\n", 2).last.to_s.b
  end

  def test_coalesced_ranges_over_http
    data = BYTE_FILE_DATA
    # the 416 case logs an error, so tolerate that single log line
    log_tester = lambda {|log, access_log|
      log = log.reject {|s| /ERROR WEBrick::HTTPStatus::RequestRangeNotSatisfiable/ =~ s }
      assert_equal([], log)
    }
    Dir.mktmpdir do |dir|
      file = File.join(dir, "range.bin")
      File.binwrite(file, data)
      config = {
        :DocumentRoot => dir,
        :CoalesceRanges => true,
        :NondisclosureName => [],
      }
      TestWEBrick.start_httpserver(config, log_tester) do |server, addr, port, log|
        # overlapping ranges merge into a single 206 range; verify against
        # the raw response which bytes were actually sent
        raw = raw_http_request(addr, port, "GET", "/range.bin", "bytes=1-3, 2-5")
        head = raw_header(raw)
        assert_match(/\AHTTP\/1\.1 206 /, head, log.call)
        assert_match(/^Content-Range: bytes 1-5\/10\r$/i, head, log.call)
        assert_match(/^Content-Length: 5\r$/i, head, log.call)
        assert_not_match(/^Content-Type: multipart\//i, head, log.call)
        assert_equal(data[1..5], raw_body(raw), log.call)

        # suffix range
        raw = raw_http_request(addr, port, "GET", "/range.bin", "bytes=-4")
        head = raw_header(raw)
        assert_match(/\AHTTP\/1\.1 206 /, head, log.call)
        assert_match(/^Content-Range: bytes 6-9\/10\r$/i, head, log.call)
        assert_equal(data[6..9], raw_body(raw), log.call)

        # ranges extending past the end are clamped
        raw = raw_http_request(addr, port, "GET", "/range.bin", "bytes=7-100")
        head = raw_header(raw)
        assert_match(/\AHTTP\/1\.1 206 /, head, log.call)
        assert_match(/^Content-Range: bytes 7-9\/10\r$/i, head, log.call)
        assert_equal(data[7..9], raw_body(raw), log.call)

        # entirely unsatisfiable ranges keep producing 416
        raw = raw_http_request(addr, port, "GET", "/range.bin", "bytes=100-200")
        head = raw_header(raw)
        body = raw_body(raw)
        assert_match(/\AHTTP\/1\.1 416 /, head, log.call)
        assert_match(/Request Range Not Satisfiable/, body, log.call)
        assert_not_equal(data, body, log.call)

        # HEAD answers with the same single-range headers but no body
        raw = raw_http_request(addr, port, "HEAD", "/range.bin", "bytes=1-3, 2-5")
        head = raw_header(raw)
        assert_match(/\AHTTP\/1\.1 206 /, head, log.call)
        assert_match(/^Content-Range: bytes 1-5\/10\r$/i, head, log.call)
        assert_match(/^Content-Length: 5\r$/i, head, log.call)
        assert_equal(+"", raw_body(raw), log.call)
      end
    end
  end
end
