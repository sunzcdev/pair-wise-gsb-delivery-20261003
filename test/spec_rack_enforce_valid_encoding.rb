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

    it "should not let the query string complete a percent escape started in the path" do
      seen = nil
      app = Rack::EnforceValidEncoding.new(
        lambda do |env|
          seen = env
          [200, {'content-type'=>'text/plain'}, ['Hello World']]
        end
      )
      response = Rack::MockRequest.new(app).get('/', 'PATH_INFO' => '/foo%', 'QUERY_STRING' => '41')
      _(response.status).must_equal(200)
      _(seen['PATH_INFO']).must_equal('/foo%')
      _(seen['QUERY_STRING']).must_equal('41')
    end

    it "should tolerate malformed percent-encoding when not strict" do
      response = Rack::MockRequest.new(@app).get('/', 'PATH_INFO' => '/foo%', 'QUERY_STRING' => 'a=%4&b=%ZZ')
      _(response.status).must_equal(200)
    end

    it "should pass the path and query string through with their encodings untouched" do
      seen = nil
      app = Rack::EnforceValidEncoding.new(
        lambda do |env|
          seen = env
          [200, {'content-type'=>'text/plain'}, ['Hello World']]
        end
      )
      path = '/a%2Fb+c'.dup.force_encoding(Encoding::BINARY)
      query = 'x=%C3%A4+y'.dup.force_encoding(Encoding::UTF_8)
      response = Rack::MockRequest.new(app).get('/', 'PATH_INFO' => path, 'QUERY_STRING' => query)
      _(response.status).must_equal(200)
      _(seen['PATH_INFO']).must_equal('/a%2Fb+c')
      _(seen['PATH_INFO'].encoding).must_equal(Encoding::BINARY)
      _(seen['QUERY_STRING']).must_equal('x=%C3%A4+y')
      _(seen['QUERY_STRING'].encoding).must_equal(Encoding::UTF_8)
    end

    it "should accept frozen path and query strings without modifying them" do
      seen = nil
      app = Rack::EnforceValidEncoding.new(
        lambda do |env|
          seen = env
          [200, {'content-type'=>'text/plain'}, ['Hello World']]
        end
      )
      path = '/frozen%20path'.freeze
      query = 'frozen=%C3%A4'.freeze
      response = Rack::MockRequest.new(app).get('/', 'PATH_INFO' => path, 'QUERY_STRING' => query)
      _(response.status).must_equal(200)
      _(seen['PATH_INFO']).must_equal(path)
      _(seen['PATH_INFO'].frozen?).must_equal(true)
      _(seen['QUERY_STRING']).must_equal(query)
      _(seen['QUERY_STRING'].frozen?).must_equal(true)
    end

    describe "strict mode" do
      before do
        @called = false
        @seen = nil
        downstream = lambda do |env|
          @called = true
          @seen = env
          [200, {'content-type'=>'text/plain'}, ['Hello World']]
        end
        @app = Rack::EnforceValidEncoding.new(downstream, strict: true)
      end

      def get(path, query = '')
        Rack::MockRequest.new(@app).get('/', 'PATH_INFO' => path, 'QUERY_STRING' => query)
      end

      it "should reject a percent escape split across the path/query boundary" do
        response = get('/foo%', '41')
        _(response.status).must_equal(400)
        _(@called).must_equal(false)
      end

      it "should reject a lone percent sign" do
        _(get('/foo%').status).must_equal(400)
        _(get('/', 'a=%').status).must_equal(400)
        _(@called).must_equal(false)
      end

      it "should reject an incomplete percent triplet" do
        _(get('/foo%4').status).must_equal(400)
        _(get('/', 'a=%4').status).must_equal(400)
        _(@called).must_equal(false)
      end

      it "should reject percent escapes with non-hexadecimal digits" do
        _(get('/foo%ZZ').status).must_equal(400)
        _(get('/', 'a=%2G').status).must_equal(400)
        _(@called).must_equal(false)
      end

      it "should reject truncated multibyte characters" do
        _(get('/%E2%82').status).must_equal(400)
        _(get('/', 'a=%C3').status).must_equal(400)
        _(@called).must_equal(false)
      end

      it "should accept a valid %2F escape" do
        response = get('/a%2Fb')
        _(response.status).must_equal(200)
        _(@seen['PATH_INFO']).must_equal('/a%2Fb')
      end

      it "should only decode a single layer, leaving %252F as literal text" do
        response = get('/a%252Fb')
        _(response.status).must_equal(200)
        _(@seen['PATH_INFO']).must_equal('/a%252Fb')
      end

      it "should treat + as an ordinary character in the path" do
        response = get('/a+b')
        _(response.status).must_equal(200)
        _(@seen['PATH_INFO']).must_equal('/a+b')
      end

      it "should treat + as a space in the query string without rewriting the env" do
        response = get('/', 'a=b+c')
        _(response.status).must_equal(200)
        _(@seen['QUERY_STRING']).must_equal('a=b+c')
      end

      it "should accept frozen path and query strings without modifying them" do
        path = '/frozen%20path'.freeze
        query = 'frozen=%C3%A4'.freeze
        response = get(path, query)
        _(response.status).must_equal(200)
        _(@seen['PATH_INFO']).must_equal(path)
        _(@seen['PATH_INFO'].frozen?).must_equal(true)
        _(@seen['QUERY_STRING']).must_equal(query)
        _(@seen['QUERY_STRING'].frozen?).must_equal(true)
      end
    end
  end
else
  STDERR.puts "WARN: Skipping Rack::EnforceValidEncoding tests (String#valid_encoding? not available)"
end
