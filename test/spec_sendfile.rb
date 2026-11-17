# frozen_string_literal: true

require_relative 'helper'
require 'fileutils'
require 'tmpdir'

separate_testing do
  require_relative '../lib/rack/sendfile'
  require_relative '../lib/rack/lint'
  require_relative '../lib/rack/mock_request'
end

describe Rack::Sendfile do
  def sendfile_body(filename = "rack_sendfile")
    FileUtils.touch File.join(Dir.tmpdir,  filename)
    res = ['Hello World']
    res.define_singleton_method(:to_path) { File.join(Dir.tmpdir,  filename) }
    res
  end

  def simple_app(body = sendfile_body)
    lambda { |env| [200, { 'content-type' => 'text/plain' }, body] }
  end

  def sendfile_app(body, mappings = [], variation = nil)
    Rack::Lint.new Rack::Sendfile.new(simple_app(body), variation, mappings)
  end

  def request(headers = {}, body = sendfile_body, mappings = [], variation = nil)
    yield Rack::MockRequest.new(sendfile_app(body, mappings, variation)).get('/', headers)
  end

  def open_file(path)
    Class.new(File) do
      unless method_defined?(:to_path)
        alias :to_path :path
      end
    end.open(path, 'wb+')
  end

  it "does nothing when no x-sendfile-type header present" do
    request do |response|
      response.must_be :ok?
      response.body.must_equal 'Hello World'
      response.headers.wont_include 'x-sendfile'
    end
  end

  it "does nothing and logs to rack.errors when incorrect x-sendfile-type header present" do
    io = StringIO.new
    # Configure with wrong variation type
    request({ 'rack.errors' => io }, sendfile_body, [], 'X-Banana') do |response|
      response.must_be :ok?
      response.body.must_equal 'Hello World'
      response.headers.wont_include 'x-sendfile'

      io.rewind
      io.read.must_equal "Unknown x-sendfile variation: \"X-Banana\"\n"
    end
  end

  it "sets x-sendfile response header and discards body" do
    request({}, sendfile_body, [], 'X-Sendfile') do |response|
      response.must_be :ok?
      response.body.must_be :empty?
      response.headers['content-length'].must_equal '0'
      response.headers['x-sendfile'].must_equal File.join(Dir.tmpdir,  "rack_sendfile")
    end
  end

  it "closes body when x-sendfile used" do
    body = sendfile_body
    closed = false
    body.define_singleton_method(:close){closed = true}
    request({}, body, [], 'X-Sendfile') do |response|
      response.must_be :ok?
      response.body.must_be :empty?
      response.headers['content-length'].must_equal '0'
      response.headers['x-sendfile'].must_equal File.join(Dir.tmpdir,  "rack_sendfile")
    end
    closed.must_equal true
  end

  it "sets x-lighttpd-send-file response header and discards body" do
    request({}, sendfile_body, [], 'X-Lighttpd-Send-File') do |response|
      response.must_be :ok?
      response.body.must_be :empty?
      response.headers['content-length'].must_equal '0'
      response.headers['x-lighttpd-send-file'].must_equal File.join(Dir.tmpdir,  "rack_sendfile")
    end
  end

  it "sets x-accel-redirect response header and discards body" do
    headers = {
      'sendfile.mapping' => "#{Dir.tmpdir}/=/foo/bar/"
    }
    request(headers, sendfile_body, [], 'X-Accel-Redirect') do |response|
      response.must_be :ok?
      response.body.must_be :empty?
      response.headers['content-length'].must_equal '0'
      response.headers['x-accel-redirect'].must_equal '/foo/bar/rack_sendfile'
    end
  end

  it "does not do a regexp substitution on the internal path" do
    tmpdir = Dir.tmpdir.dup
    tmpdir[1..2] = ".*"
    headers = {
      'sendfile.mapping' => "#{tmpdir}/=/foo/bar/"
    }
    body = sendfile_body
    # The mapping root is treated as a literal path, so the injected ".*"
    # does not match as a regexp and the body cannot be offloaded.
    request(headers, body, [], 'X-Accel-Redirect') do |response|
      response.must_be :ok?
      response.body.must_equal 'Hello World'
      response.headers.wont_include 'x-accel-redirect'
      response.errors.must_include 'x-accel-mapping'
    end
  end

  it "sets x-accel-redirect response header to percent-encoded path" do
    skip "file names cannot contain '?' on this platform" if RUBY_PLATFORM =~ /mingw|mswin/
    headers = {
      'sendfile.mapping' => "#{Dir.tmpdir}/=/foo/bar%/"
    }
    request(headers, sendfile_body('file_with_%_?_symbol'), [], 'X-Accel-Redirect') do |response|
      response.must_be :ok?
      response.body.must_be :empty?
      response.headers['content-length'].must_equal '0'
      response.headers['x-accel-redirect'].must_equal '/foo/bar%25/file_with_%25_%3F_symbol'
    end
  end

  it 'writes to rack.error when no x-accel-mapping is specified' do
    request({}, sendfile_body, [], 'X-Accel-Redirect') do |response|
      response.must_be :ok?
      response.body.must_equal 'Hello World'
      response.headers.wont_include 'x-accel-redirect'
      response.errors.must_include 'x-accel-mapping'
    end
  end

  it 'does nothing when body does not respond to #to_path' do
    request({}, ['Not a file...'], [], 'X-Sendfile') do |response|
      response.body.must_equal 'Not a file...'
      response.headers.wont_include 'x-sendfile'
    end
  end

  it "sets x-accel-redirect response header and discards body when initialized with multiple mappings" do
    begin
      dir1 = Dir.mktmpdir
      dir2 = Dir.mktmpdir

      first_body = open_file(File.join(dir1, 'rack_sendfile'))
      first_body.puts 'hello world'

      second_body = open_file(File.join(dir2, 'rack_sendfile'))
      second_body.puts 'goodbye world'

      mappings = [
        ["#{dir1}/", '/foo/bar/'],
        ["#{dir2}/", '/wibble/']
      ]

      request({}, first_body, mappings, 'X-Accel-Redirect') do |response|
        response.must_be :ok?
        response.body.must_be :empty?
        response.headers['content-length'].must_equal '0'
        response.headers['x-accel-redirect'].must_equal '/foo/bar/rack_sendfile'
      end

      request({}, second_body, mappings, 'X-Accel-Redirect') do |response|
        response.must_be :ok?
        response.body.must_be :empty?
        response.headers['content-length'].must_equal '0'
        response.headers['x-accel-redirect'].must_equal '/wibble/rack_sendfile'
      end
    ensure
      FileUtils.remove_entry_secure dir1
      FileUtils.remove_entry_secure dir2
    end
  end

  it "sets x-accel-redirect response header and discards body when initialized with multiple mappings via env" do
    begin
      dir1 = Dir.mktmpdir
      dir2 = Dir.mktmpdir
      dir3 = Dir.mktmpdir

      first_body = open_file(File.join(dir1, 'rack_sendfile'))
      first_body.puts 'hello world'

      second_body = open_file(File.join(dir2, 'rack_sendfile'))
      second_body.puts 'goodbye world'

      third_body = open_file(File.join(dir3, 'rack_sendfile'))
      third_body.puts 'hello again world'
      third_body.rewind

      # Now we need to explicitly enable x-accel-redirect in the constructor
      app = Rack::Lint.new Rack::Sendfile.new(simple_app(first_body), "X-Accel-Redirect", [])

      headers = {
        'sendfile.mapping' => "#{dir1}/=/foo/bar/, #{dir2}/=/wibble/"
      }

      response = Rack::MockRequest.new(app).get('/', headers)
      response.must_be :ok?
      response.body.must_be :empty?
      response.headers['content-length'].must_equal '0'
      response.headers['x-accel-redirect'].must_equal '/foo/bar/rack_sendfile'

      app = Rack::Lint.new Rack::Sendfile.new(simple_app(second_body), "X-Accel-Redirect", [])
      response = Rack::MockRequest.new(app).get('/', headers)
      response.must_be :ok?
      response.body.must_be :empty?
      response.headers['content-length'].must_equal '0'
      response.headers['x-accel-redirect'].must_equal '/wibble/rack_sendfile'

      # A path outside every mapped root cannot be offloaded; the declared
      # default policy keeps the original body instead of emitting the raw
      # local path as a redirect URI.
      app = Rack::Lint.new Rack::Sendfile.new(simple_app(third_body), "X-Accel-Redirect", [])
      response = Rack::MockRequest.new(app).get('/', headers)
      response.must_be :ok?
      response.body.must_equal "hello again world\n"
      response.headers.wont_include 'x-accel-redirect'
    ensure
      FileUtils.remove_entry_secure dir1
      FileUtils.remove_entry_secure dir2
      FileUtils.remove_entry_secure dir3
    end
  end

  # Security tests for CVE mitigation
  describe "security: information disclosure prevention" do
    it "ignores HTTP_X_SENDFILE_TYPE header to prevent attacker-controlled sendfile activation" do
      # Attacker tries to enable x-sendfile via header
      request 'HTTP_X_SENDFILE_TYPE' => 'x-sendfile' do |response|
        response.must_be :ok?
        response.body.must_equal 'Hello World'
        response.headers.wont_include 'x-sendfile'
      end
    end

    it "ignores HTTP_X_SENDFILE_TYPE header attempting to enable x-accel-redirect" do
      # Attacker tries to enable x-accel-redirect via header with mapping
      headers = {
        'HTTP_X_SENDFILE_TYPE' => 'x-accel-redirect',
        'HTTP_X_ACCEL_MAPPING' => "#{Dir.tmpdir}/=/attacker/path/"
      }
      request headers do |response|
        response.must_be :ok?
        response.body.must_equal 'Hello World'
        response.headers.wont_include 'x-accel-redirect'
      end
    end

    it "ignores HTTP_X_ACCEL_MAPPING when x-accel-redirect is not explicitly enabled" do
      # Even if attacker sends mapping header, it should be ignored without explicit config
      headers = {
        'HTTP_X_ACCEL_MAPPING' => "#{Dir.tmpdir}/=/attacker/path/"
      }
      request headers do |response|
        response.must_be :ok?
        response.body.must_equal 'Hello World'
        response.headers.wont_include 'x-accel-redirect'
      end
    end

    it "ignores HTTP_X_ACCEL_MAPPING when application-level mappings are configured" do
      # When app provides mappings, header should be ignored for security
      begin
        dir = Dir.mktmpdir
        body = open_file(File.join(dir, 'rack_sendfile'))
        body.puts 'test'
        
        app_mappings = [["#{dir}/", '/app/mapping/']]
        app = Rack::Lint.new Rack::Sendfile.new(simple_app(body), "X-Accel-Redirect", app_mappings)
        
        headers = {
          'HTTP_X_ACCEL_MAPPING' => "#{dir}/=/attacker/path/"
        }
        
        response = Rack::MockRequest.new(app).get('/', headers)
        response.must_be :ok?
        response.body.must_be :empty?
        response.headers['x-accel-redirect'].must_equal '/app/mapping/rack_sendfile'
        response.headers['x-accel-redirect'].wont_equal '/attacker/path/rack_sendfile'
      ensure
        FileUtils.remove_entry_secure dir
      end
    end

    it "never trusts HTTP_X_ACCEL_MAPPING, even when x-accel-redirect is explicitly enabled with no app mappings" do
      # Request headers are attacker-controlled and must never be used as
      # mapping configuration, regardless of middleware configuration.
      begin
        dir = Dir.mktmpdir
        body = open_file(File.join(dir, 'rack_sendfile'))
        body.puts 'test'
        body.rewind

        app = Rack::Lint.new Rack::Sendfile.new(simple_app(body), "X-Accel-Redirect", [])

        headers = {
          'HTTP_X_ACCEL_MAPPING' => "#{dir}/=/attacker/path/"
        }

        response = Rack::MockRequest.new(app).get('/', headers)
        response.must_be :ok?
        response.body.must_equal "test\n"
        response.headers.wont_include 'x-accel-redirect'
      ensure
        FileUtils.remove_entry_secure dir
      end
    end

    it "honors sendfile.mapping env key when x-accel-redirect explicitly enabled with no app mappings" do
      # This is the safe use case: explicit config + no app mappings, and the
      # mapping arrives via a non-header env key only the server can set.
      begin
        dir = Dir.mktmpdir
        body = open_file(File.join(dir, 'rack_sendfile'))
        body.puts 'test'

        app = Rack::Lint.new Rack::Sendfile.new(simple_app(body), "X-Accel-Redirect", [])

        headers = {
          'sendfile.mapping' => "#{dir}/=/safe/nginx/mapping/"
        }

        response = Rack::MockRequest.new(app).get('/', headers)
        response.must_be :ok?
        response.body.must_be :empty?
        response.headers['x-accel-redirect'].must_equal '/safe/nginx/mapping/rack_sendfile'
      ensure
        FileUtils.remove_entry_secure dir
      end
    end

    it "does not allow x-lighttpd-send-file activation via header" do
      # Verify other sendfile types also can't be enabled via headers
      request 'HTTP_X_SENDFILE_TYPE' => 'x-lighttpd-send-file' do |response|
        response.must_be :ok?
        response.body.must_equal 'Hello World'
        response.headers.wont_include 'x-lighttpd-send-file'
      end
    end

    it "requires explicit middleware configuration for any sendfile variation" do
      # Test that sendfile.type env var still works (internal, not from HTTP headers)
      body = sendfile_body
      app = lambda { |env| [200, { 'content-type' => 'text/plain' }, body] }
      middleware = Rack::Lint.new Rack::Sendfile.new(app)
      
      env = Rack::MockRequest.env_for('/', { 'sendfile.type' => 'x-sendfile' })
      status, headers = middleware.call(env)
      
      status.must_equal 200
      headers['x-sendfile'].must_equal File.join(Dir.tmpdir, "rack_sendfile")
      headers['content-length'].must_equal '0'
    end
  end

  it "ignores x-accel-mapping header when x-accel-redirect is not configured via the constructor" do
    headers = {
      'sendfile.type' => 'X-Accel-Redirect',
      'HTTP_X_ACCEL_MAPPING' => "#{Dir.tmpdir}/=/foo/bar/"
    }
    request(headers) do |response|
      response.must_be :ok?
      response.body.must_equal 'Hello World'
      response.headers.wont_include 'x-accel-redirect'
      response.errors.must_include 'x-accel-mapping'
    end
  end

  it "ignores x-accel-mapping header when mappings are configured but none match" do
    headers = {
      'HTTP_X_ACCEL_MAPPING' => "#{Dir.tmpdir}/=/foo/bar/"
    }
    request(headers, sendfile_body, [["/nonexistent/", "/foo/"]], 'X-Accel-Redirect') do |response|
      response.must_be :ok?
      response.body.must_equal 'Hello World'
      response.headers.wont_include 'x-accel-redirect'
      response.errors.must_include 'x-accel-mapping'
    end
  end

  describe "mapping plan path safety" do
    it "does not map paths under adjacent prefixes that share a leading substring" do
      begin
        dir = Dir.mktmpdir('sendfile')
        sibling = "#{dir}2"
        FileUtils.mkdir_p(sibling)

        body = open_file(File.join(sibling, 'rack_sendfile'))
        body.puts 'sibling'
        body.rewind

        # The mapping root is a literal path boundary: a file living under
        # "#{dir}2" must never be mapped as if it lived under "#{dir}".
        app = Rack::Lint.new Rack::Sendfile.new(simple_app(body), "X-Accel-Redirect", [[dir, '/files/']])
        response = Rack::MockRequest.new(app).get('/')
        response.must_be :ok?
        response.body.must_equal "sibling\n"
        response.headers.wont_include 'x-accel-redirect'
        response.errors.must_include 'x-accel-mapping'
      ensure
        FileUtils.remove_entry_secure dir
        FileUtils.remove_entry_secure sibling
      end
    end

    it "maps paths directly under a root given without a trailing slash" do
      begin
        dir = Dir.mktmpdir
        body = open_file(File.join(dir, 'rack_sendfile'))

        app = Rack::Lint.new Rack::Sendfile.new(simple_app(body), "X-Accel-Redirect", [[dir, '/files']])
        response = Rack::MockRequest.new(app).get('/')
        response.must_be :ok?
        response.body.must_be :empty?
        response.headers['x-accel-redirect'].must_equal '/files/rack_sendfile'
      ensure
        FileUtils.remove_entry_secure dir
      end
    end

    it "percent-encodes spaces and non-ASCII characters in the mapped URI" do
      begin
        dir = Dir.mktmpdir
        body = open_file(File.join(dir, 'rack sendfile ü.txt'))
        body.puts 'unicode'

        app = Rack::Lint.new Rack::Sendfile.new(simple_app(body), "X-Accel-Redirect", [["#{dir}/", '/files/']])
        response = Rack::MockRequest.new(app).get('/')
        response.must_be :ok?
        response.body.must_be :empty?
        response.headers['x-accel-redirect'].must_equal '/files/rack%20sendfile%20%C3%BC.txt'
      ensure
        FileUtils.remove_entry_secure dir
      end
    end

    it "hands the raw local path to x-sendfile even with spaces and non-ASCII characters" do
      begin
        dir = Dir.mktmpdir
        path = File.join(dir, 'rack sendfile ü.txt')
        body = open_file(path)

        app = Rack::Lint.new Rack::Sendfile.new(simple_app(body), "X-Sendfile")
        response = Rack::MockRequest.new(app).get('/')
        response.must_be :ok?
        response.headers['x-sendfile'].must_equal File.expand_path(path)
      ensure
        FileUtils.remove_entry_secure dir
      end
    end
  end

  describe "scheme and mapping compatibility" do
    it "raises ArgumentError when mappings are combined with the x-sendfile variation" do
      lambda {
        Rack::Sendfile.new(simple_app, "X-Sendfile", [["/var/www/", "/files/"]])
      }.must_raise ArgumentError
    end

    it "raises ArgumentError when mappings are combined with the x-lighttpd-send-file variation" do
      lambda {
        Rack::Sendfile.new(simple_app, "X-Lighttpd-Send-File", [["/var/www/", "/files/"]])
      }.must_raise ArgumentError
    end

    it "applies the failure policy when the per-request variation cannot honor mappings" do
      body = sendfile_body
      app = Rack::Lint.new Rack::Sendfile.new(simple_app(body), nil, [["#{Dir.tmpdir}/", "/files/"]])
      response = Rack::MockRequest.new(app).get('/', 'sendfile.type' => 'x-sendfile')
      response.must_be :ok?
      response.body.must_equal 'Hello World'
      response.headers.wont_include 'x-sendfile'
      response.errors.must_include 'x-accel-redirect'
    end
  end

  describe "failure policy" do
    it "keeps the original body and its content-length by default when offload fails" do
      body = sendfile_body
      app = lambda { |env| [200, { 'content-type' => 'text/plain', 'content-length' => '11' }, body] }
      middleware = Rack::Lint.new Rack::Sendfile.new(app, "X-Accel-Redirect", [])
      response = Rack::MockRequest.new(middleware).get('/')
      response.must_be :ok?
      response.body.must_equal 'Hello World'
      response.headers['content-length'].must_equal '11'
      response.headers.wont_include 'x-accel-redirect'
    end

    it "refuses the request and closes the body when on_failure is :refuse and no mapping matches" do
      body = sendfile_body
      closed = false
      body.define_singleton_method(:close) { closed = true }

      app = Rack::Lint.new Rack::Sendfile.new(simple_app(body), "X-Accel-Redirect", [], on_failure: :refuse)
      response = Rack::MockRequest.new(app).get('/')
      response.status.must_equal 500
      response.body.must_be :empty?
      response.headers['content-length'].must_equal '0'
      response.headers.wont_include 'x-accel-redirect'
      closed.must_equal true
    end

    it "refuses the request when the variation is unknown and on_failure is :refuse" do
      body = sendfile_body
      closed = false
      body.define_singleton_method(:close) { closed = true }

      app = Rack::Lint.new Rack::Sendfile.new(simple_app(body), "X-Banana", [], on_failure: :refuse)
      response = Rack::MockRequest.new(app).get('/')
      response.status.must_equal 500
      response.body.must_be :empty?
      response.errors.must_include 'Unknown x-sendfile variation'
      closed.must_equal true
    end

    it "raises ArgumentError for an unknown on_failure policy" do
      lambda {
        Rack::Sendfile.new(simple_app, "X-Accel-Redirect", [], on_failure: :explode)
      }.must_raise ArgumentError
    end
  end
end
