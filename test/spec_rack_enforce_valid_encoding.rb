# -*- encoding : us-ascii -*-
# frozen_string_literal: true

require 'minitest/autorun'
require 'rack/contrib/enforce_valid_encoding'

if "a string".respond_to?(:valid_encoding?)
  require 'rack/mock'
  require 'rack/lint'
  require 'rack/contrib/enforce_valid_encoding'

  VALID_PATH = "h%C3%A4ll%C3%B2"
  INVALID_PATH = "/%D1%A1%D4%F1%D7%A2%B2%E1%D3%C3%BB%A7%C3%FB"

  describe "Rack::EnforceValidEncoding" do
    before do
      @app = Rack::Lint.new(
        Rack::EnforceValidEncoding.new(
          lambda do |env|
            [200, {'content-type'=>'text/plain'}, ['Hello World']]
          end
        )
      )
    end

    describe "contstant assertions" do
      it "INVALID_PATH should not be a valid UTF-8 string when decoded" do
        _(Rack::Utils.unescape(INVALID_PATH).valid_encoding?).must_equal false
      end

      it "VALID_PATH should be valid when decoded" do
        _(Rack::Utils.unescape(VALID_PATH).valid_encoding?).must_equal true
      end
    end

    it "should accept a request with a correctly encoded path" do
      response = Rack::MockRequest.new(@app).get(VALID_PATH)
      _(response.body).must_equal("Hello World")
      _(response.status).must_equal(200)
    end

    it "should reject a request with a poorly encoded path" do
      response = Rack::MockRequest.new(@app).get(INVALID_PATH)
      _(response.status).must_equal(400)
    end

    it "should accept a request with a correctly encoded query string" do
      response = Rack::MockRequest.new(@app).get('/', 'QUERY_STRING' => VALID_PATH)
      _(response.body).must_equal("Hello World")
      _(response.status).must_equal(200)
    end

    it "should reject a request with a poorly encoded query string" do
      response = Rack::MockRequest.new(@app).get('/', 'QUERY_STRING' => INVALID_PATH)
      _(response.status).must_equal(400)
    end

    it "should reject a request containing malformed multibyte characters" do
      response = Rack::MockRequest.new(@app).get('/', 'QUERY_STRING' => Rack::Utils.unescape(INVALID_PATH, Encoding::ASCII_8BIT))
      _(response.status).must_equal(400)
    end

    def capture_app(strict: false)
      captured = nil
      downstream = lambda do |env|
        captured = env
        [200, {'content-type'=>'text/plain'}, ['Hello World']]
      end
      [Rack::EnforceValidEncoding.new(downstream, strict: strict), -> { captured }]
    end

    describe "component boundaries" do
      it "should reject a percent sign completed by the query string" do
        app, captured = capture_app
        status, = app.call('PATH_INFO' => '/%', 'QUERY_STRING' => '41')
        _(status).must_equal(400)
        _(captured.call).must_be_nil
      end

      it "should reject a percent sign completed by the path" do
        app, captured = capture_app
        status, = app.call('PATH_INFO' => '/foo%4', 'QUERY_STRING' => '1')
        _(status).must_equal(400)
        _(captured.call).must_be_nil
      end

      it "should reject a truncated multibyte character in the path" do
        app, captured = capture_app
        status, = app.call('PATH_INFO' => "/\xC3".b, 'QUERY_STRING' => '')
        _(status).must_equal(400)
        _(captured.call).must_be_nil
      end

      it "should accept a valid %2F escape in the path" do
        app, captured = capture_app
        status, = app.call('PATH_INFO' => '/a%2Fb', 'QUERY_STRING' => '')
        _(status).must_equal(200)
        _(captured.call['PATH_INFO']).must_equal('/a%2Fb')
      end

      it "should not modify frozen request strings" do
        app, captured = capture_app
        path = '/h%C3%A4llo'.freeze
        query = 'x=%C3%A4'.freeze
        status, = app.call('PATH_INFO' => path, 'QUERY_STRING' => query)
        _(status).must_equal(200)
        _(captured.call['PATH_INFO']).must_equal('/h%C3%A4llo')
        _(captured.call['QUERY_STRING']).must_equal('x=%C3%A4')
        _(captured.call['PATH_INFO'].encoding).must_equal(path.encoding)
        _(captured.call['QUERY_STRING'].encoding).must_equal(query.encoding)
      end
    end

    describe "strict percent-encoding validation" do
      it "should reject a lone percent sign" do
        app, captured = capture_app(strict: true)
        status, = app.call('PATH_INFO' => '/%', 'QUERY_STRING' => '')
        _(status).must_equal(400)
        _(captured.call).must_be_nil
      end

      it "should reject an incomplete escape triplet" do
        app, captured = capture_app(strict: true)
        status, = app.call('PATH_INFO' => '/', 'QUERY_STRING' => 'x=%4')
        _(status).must_equal(400)
        _(captured.call).must_be_nil
      end

      it "should reject a non-hex escape" do
        app, captured = capture_app(strict: true)
        status, = app.call('PATH_INFO' => '/%GG', 'QUERY_STRING' => '')
        _(status).must_equal(400)
        _(captured.call).must_be_nil
      end

      it "should reject escapes decoding to invalid UTF-8" do
        app, captured = capture_app(strict: true)
        status, = app.call('PATH_INFO' => '/%C3', 'QUERY_STRING' => '')
        _(status).must_equal(400)
        _(captured.call).must_be_nil
      end

      it "should decode only a single layer of percent encoding" do
        app, captured = capture_app(strict: true)
        status, = app.call('PATH_INFO' => '/%252F', 'QUERY_STRING' => '')
        _(status).must_equal(200)
        _(captured.call['PATH_INFO']).must_equal('/%252F')
      end

      it "should treat a plus sign in the path as a literal character" do
        app, captured = capture_app(strict: true)
        status, = app.call('PATH_INFO' => '/a+b', 'QUERY_STRING' => '')
        _(status).must_equal(200)
        _(captured.call['PATH_INFO']).must_equal('/a+b')
      end

      it "should treat a plus sign in the query string as a form space" do
        app, captured = capture_app(strict: true)
        status, = app.call('PATH_INFO' => '/', 'QUERY_STRING' => 'q=a+b')
        _(status).must_equal(200)
        _(captured.call['QUERY_STRING']).must_equal('q=a+b')
      end

      it "should accept valid escapes and leave the environment untouched" do
        app, captured = capture_app(strict: true)
        path = '/h%C3%A4llo'.freeze
        query = 'x=%2F'.freeze
        status, = app.call('PATH_INFO' => path, 'QUERY_STRING' => query)
        _(status).must_equal(200)
        _(captured.call['PATH_INFO']).must_equal('/h%C3%A4llo')
        _(captured.call['QUERY_STRING']).must_equal('x=%2F')
        _(captured.call['PATH_INFO'].encoding).must_equal(path.encoding)
        _(captured.call['QUERY_STRING'].encoding).must_equal(query.encoding)
      end
    end
  end
else
  STDERR.puts "WARN: Skipping Rack::EnforceValidEncoding tests (String#valid_encoding? not available)"
end
