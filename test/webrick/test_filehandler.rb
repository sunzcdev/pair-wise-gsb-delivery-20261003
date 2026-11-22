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

  def with_mime_files
    Dir.mktmpdir do |dir|
      %w[a b].each do |sub|
        Dir.mkdir(File.join(dir, sub))
        # the same names under both mounts: a compound suffix, a simple
        # suffix, and a suffix that no mapping knows about
        File.binwrite(File.join(dir, sub, "same.tar.gz"), "0123456789")
        File.binwrite(File.join(dir, sub, "same.gz"), "0123456789")
        File.binwrite(File.join(dir, sub, "plain.unknown"), "0123456789")
      end
      yield dir
    end
  end

  def frozen_mime_types(tab)
    WEBrick::HTTPUtils.freeze_mime_types(tab)
  end

  def get_request(url = "/")
    req = WEBrick::HTTPRequest.new(WEBrick::Config::HTTP)
    req.parse(StringIO.new("GET #{url} HTTP/1.0\r\n\r\n"))
    req
  end

  def get_range_request(range_spec)
    req = WEBrick::HTTPRequest.new(WEBrick::Config::HTTP)
    req.parse(make_range_request(range_spec))
    req
  end

  def test_default_file_handler_uses_mount_mime_types
    with_mime_files do |dir|
      path = File.join(dir, "a", "same.tar.gz")
      mime_types = frozen_mime_types("gz" => "application/gzip",
                                     "tar.gz" => "application/x-tar")
      handler = WEBrick::HTTPServlet::DefaultFileHandler.new(
        WEBrick::Config::HTTP, path, mime_types)

      # a plain GET picks the most specific suffix of the mount mapping
      res = WEBrick::HTTPResponse.new(WEBrick::Config::HTTP)
      handler.do_GET(get_request, res)
      assert_equal("application/x-tar", res["content-type"])
      assert_equal("0123456789", get_res_body(res))

      # HEAD resolves the file the same way
      res = WEBrick::HTTPResponse.new(WEBrick::Config::HTTP)
      res.request_method = "HEAD"
      handler.do_GET(get_request, res)
      assert_equal("application/x-tar", res["content-type"])
      res.body.close # HEAD sends no body, so close it here

      # a single-part Range resolves the file the same way
      res = WEBrick::HTTPResponse.new(WEBrick::Config::HTTP)
      handler.make_partial_content(get_range_request("bytes=0-3"), res,
                                   path, File.size(path))
      assert_equal("application/x-tar", res["content-type"])
      assert_equal("0123", get_res_body(res))

      # so does every part of a multipart/byteranges response
      res = WEBrick::HTTPResponse.new(WEBrick::Config::HTTP)
      handler.make_partial_content(get_range_request("bytes=0-0, -2"), res,
                                   path, File.size(path))
      assert_match(%r{\Amultipart/byteranges}, res["content-type"])
      body = get_res_body(res)
      assert_equal(2, body.scan("Content-Type: application/x-tar\r\n").size)
    end
  end

  def test_default_file_handler_falls_back_without_matching_suffix
    with_mime_files do |dir|
      path = File.join(dir, "a", "plain.unknown")
      config = WEBrick::Config::HTTP.dup
      config[:MimeTypes] = WEBrick::HTTPUtils::DefaultMimeTypes.merge(
        "unknown" => "application/x-fallback")
      handler = WEBrick::HTTPServlet::DefaultFileHandler.new(
        config, path, frozen_mime_types("gz" => "application/gzip"))

      # the mount mapping has no match, so the server-wide table is used
      res = WEBrick::HTTPResponse.new(config)
      handler.do_GET(get_request, res)
      assert_equal("application/x-fallback", res["content-type"])
      res.body.close

      # when neither table knows the suffix, the old octet-stream fallback
      # is preserved
      config = WEBrick::Config::HTTP.dup
      handler = WEBrick::HTTPServlet::DefaultFileHandler.new(
        config, path, frozen_mime_types("gz" => "application/gzip"))
      res = WEBrick::HTTPResponse.new(config)
      handler.do_GET(get_request, res)
      assert_equal("application/octet-stream", res["content-type"])
      res.body.close
    end
  end

  def test_default_file_handler_falls_back_without_mount_mapping
    with_mime_files do |dir|
      # no mount mapping at all: exactly the previous behaviour, which only
      # knows the two last suffixes and the server-wide table
      path = File.join(dir, "a", "same.tar.gz")
      handler = WEBrick::HTTPServlet::DefaultFileHandler.new(
        WEBrick::Config::HTTP, path)
      assert_nil(handler.mime_types)

      res = WEBrick::HTTPResponse.new(WEBrick::Config::HTTP)
      handler.do_GET(get_request, res)
      assert_equal("application/octet-stream", res["content-type"])
      res.body.close

      path = File.join(dir, "a", "same.gz")
      handler = WEBrick::HTTPServlet::DefaultFileHandler.new(
        WEBrick::Config::HTTP, path)
      res = WEBrick::HTTPResponse.new(WEBrick::Config::HTTP)
      handler.do_GET(get_request, res)
      assert_equal("application/octet-stream", res["content-type"])
      res.body.close
    end
  end

  def test_mount_mime_types_do_not_leak_between_mounts
    with_mime_files do |dir|
      mapping_a = { "gz" => "application/gzip",
                    "tar.gz" => "application/x-tar" }
      mapping_b = { "gz" => "application/x-b-gzip",
                    "tar.gz" => "application/x-b-tar" }
      options_a = WEBrick::HTTPServlet::FileHandler.freeze_mount_options(
        File.join(dir, "a"), { :MimeTypes => mapping_a })[1]
      options_b = WEBrick::HTTPServlet::FileHandler.freeze_mount_options(
        File.join(dir, "b"), { :MimeTypes => mapping_b })[1]

      assert_predicate(options_a[:MimeTypes], :frozen?)
      assert_equal("application/x-tar", options_a[:MimeTypes]["tar.gz"])
      assert_equal("application/x-b-tar", options_b[:MimeTypes]["tar.gz"])

      # mutating the caller mappings after mounting changes neither snapshot
      mapping_a["tar.gz"] = "text/plain"
      mapping_b["gz"] = "text/plain"
      assert_equal("application/x-tar", options_a[:MimeTypes]["tar.gz"])
      assert_equal("application/x-b-tar", options_b[:MimeTypes]["tar.gz"])
      assert_equal("application/gzip", options_a[:MimeTypes]["gz"])
      assert_equal("application/x-b-gzip", options_b[:MimeTypes]["gz"])

      # and neither frozen snapshot is the caller's mapping
      assert_not_same(mapping_a, options_a[:MimeTypes])
      assert_not_same(mapping_b, options_b[:MimeTypes])
      assert_not_same(options_a[:MimeTypes], options_b[:MimeTypes])

      # options unrelated to the mapping are left untouched
      opts = { :FancyIndexing => true }
      mount_options = WEBrick::HTTPServlet::FileHandler.freeze_mount_options(
        File.join(dir, "a"), opts)
      assert_same(opts, mount_options[1])
    end
  end

  def test_mount_mime_types_are_frozen_at_mount_time
    with_mime_files do |dir|
      mapping = { "tar.gz" => "application/x-tar" }
      config = { :DocumentRoot => File.join(dir, "a"),
                 :DocumentRootOptions => { :MimeTypes => mapping } }
      TestWEBrick.start_httpserver(config) do |server, addr, port, log|
        # the mount table owns a frozen copy, not the caller's hash
        document_root_options = server.search_servlet("/same.tar.gz")[1][1]
        assert_predicate(document_root_options[:MimeTypes], :frozen?)
        assert_equal("application/x-tar",
                     document_root_options[:MimeTypes]["tar.gz"])

        # mutating the caller's hash after mounting has no effect either
        mapping["tar.gz"] = "text/plain"

        http = Net::HTTP.new(addr, port)
        req = Net::HTTP::Get.new("/same.tar.gz")
        http.request(req) do |res|
          assert_equal("200", res.code, log.call)
          assert_equal("application/x-tar", res.content_type, log.call)
        end
      end
    end
  end

  def test_default_file_handler_mount_mime_types_are_frozen
    with_mime_files do |dir|
      path = File.join(dir, "a", "same.tar.gz")
      mapping = { "tar.gz" => "application/x-tar" }
      TestWEBrick.start_httpserver({}) do |server, addr, port, log|
        # a directly mounted DefaultFileHandler gets its mapping as the
        # second servlet option
        server.mount("/x", WEBrick::HTTPServlet::DefaultFileHandler,
                     path, mapping)
        _, options, = server.search_servlet("/x/same.tar.gz")
        assert_predicate(options[1], :frozen?)

        mapping["tar.gz"] = "text/plain"

        http = Net::HTTP.new(addr, port)
        http.request(Net::HTTP::Get.new("/x")) do |res|
          assert_equal("200", res.code, log.call)
          assert_equal("application/x-tar", res.content_type, log.call)
        end
      end
    end
  end

  def test_two_mounts_same_file_name_have_independent_mime_types
    with_mime_files do |dir|
      # distinct bodies so a response from one mount cannot be mistaken for
      # the other
      File.binwrite(File.join(dir, "a", "same.tar.gz"), "aaaaaaaaaa")
      File.binwrite(File.join(dir, "b", "same.tar.gz"), "bbbbbbbbbb")
      File.binwrite(File.join(dir, "a", "same.gz"), "aaaaaaaaaa")
      File.binwrite(File.join(dir, "b", "same.gz"), "bbbbbbbbbb")

      mapping_a = { "gz" => "application/gzip",
                    "tar.gz" => "application/x-a-tar" }
      mapping_b = { "gz" => "application/x-b-gzip",
                    "tar.gz" => "application/x-b-tar" }
      TestWEBrick.start_httpserver({}) do |server, addr, port, log|
        server.mount("/a", WEBrick::HTTPServlet::FileHandler,
                     File.join(dir, "a"), :MimeTypes => mapping_a)
        server.mount("/b", WEBrick::HTTPServlet::FileHandler,
                     File.join(dir, "b"), :MimeTypes => mapping_b)
        http = Net::HTTP.new(addr, port)

        expected = {
          "/a/same.tar.gz" => ["application/x-a-tar", "aaaaaaaaaa"],
          "/b/same.tar.gz" => ["application/x-b-tar", "bbbbbbbbbb"],
          "/a/same.gz"     => ["application/gzip",   "aaaaaaaaaa"],
          "/b/same.gz"     => ["application/x-b-gzip", "bbbbbbbbbb"],
        }

        # alternate between the two mounts repeatedly: neither may drift
        # towards the mapping (or content) of the other
        3.times do
          expected.each do |path, (type, body)|
            http.request(Net::HTTP::Get.new(path)) do |res|
              assert_equal("200", res.code, log.call)
              assert_equal(type, res.content_type, "#{path}\n#{log.call}")
              assert_equal(body, res.body, "#{path}\n#{log.call}")
            end
          end
        end

        # HEAD agrees with GET for both mounts
        expected.each do |path, (type, _)|
          req = Net::HTTP::Head.new(path)
          http.request(req) do |res|
            assert_equal(type, res.content_type, "HEAD #{path}\n#{log.call}")
          end
        end

        # Range agrees with GET for both mounts
        expected.each do |path, (type, body)|
          req = Net::HTTP::Get.new(path, "range" => "bytes=0-3")
          http.request(req) do |res|
            assert_equal("206", res.code, log.call)
            assert_equal(type, res.content_type, "Range #{path}\n#{log.call}")
            assert_equal(body[0, 4], res.body, log.call)
          end
        end

        # changing the configuration of one mount after mounting changes
        # neither response
        mapping_a.clear
        mapping_b["tar.gz"] = "text/plain"
        expected.each do |path, (type, body)|
          http.request(Net::HTTP::Get.new(path)) do |res|
            assert_equal(type, res.content_type, "#{path}\n#{log.call}")
            assert_equal(body, res.body, "#{path}\n#{log.call}")
          end
        end
      end
    end
  end
end
