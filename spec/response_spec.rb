require_relative "spec_helper"

describe "response #[] and #[]=" do
  it "should get/set headers" do
    app do |r|
      response['foo'] = 'bar'
      response['foo'] + response.headers['foo']
    end

    header('foo').must_equal "bar"
    body.must_equal 'barbar'
  end
end

describe "response #headers and #body" do
  it "should return headers and body" do
    app do |r|
      response.headers['foo'] = 'bar'
      response.write response.body.is_a?(Array)
    end

    header('foo').must_equal "bar"
    body.must_equal 'true'
  end

  it "uses plain hash for response headers" do
    app do |r|
      response.headers['UP'] = 'U'
      response.headers['down'] = 'd'
    end

    req[1].must_be_instance_of Hash
    header('up').must_be_nil
    header('UP').must_equal 'U'
    header('down').must_equal 'd'
    header('DOWN').must_be_nil
  end if Rack.release < '3'

  it "uses Rack::Headers for response headers" do
    app do |r|
      response.headers['UP'] = 'U'
      response.headers['down'] = 'd'
    end

    req[1].must_be_instance_of Rack::Headers
    header('up').must_equal 'U'
    header('UP').must_equal 'U'
    header('down').must_equal 'd'
    header('DOWN').must_equal 'd'
  end if Rack.release >= '3' && !ENV['PLAIN_HASH_RESPONSE_HEADERS']
end

describe "response #write" do
  it "should add to body" do
    app do |r|
      response.write 'a'
      response.write 'b'
    end

    body.must_equal 'ab'
  end
end

describe "response #finish" do
  it "should set status to 404 if body has not been written to" do
    s, h, b = nil
    app do |r|
      s, h, b = response.finish
      ''
    end

    body.must_equal ''
    s.must_equal 404
    h[RodaResponseHeaders::CONTENT_TYPE].must_equal 'text/html'
    b.join.length.must_equal 0
  end

  it "should set status to 200 if body has been written to" do
    s, h, b = nil
    app do |r|
      response.write 'a'
      s, h, b = response.finish
      ''
    end

    body.must_equal 'a'
    s.must_equal 200
    h[RodaResponseHeaders::CONTENT_TYPE].must_equal 'text/html'
    b.join.length.must_equal 1
  end

  it "should set Content-Length header" do
    app do |r|
      response.write 'a'
      response[RodaResponseHeaders::CONTENT_LENGTH].must_be_nil
      throw :halt, response.finish
    end

    header(RodaResponseHeaders::CONTENT_LENGTH).must_equal '1'
  end

  [204, 304, 100].each do |status|
    it "should not set Content-Type or Content-Length header on a #{status} response" do
      app do |r|
        response.status = status
        throw :halt, response.finish
      end

      header(RodaResponseHeaders::CONTENT_TYPE).must_be_nil
      header(RodaResponseHeaders::CONTENT_LENGTH).must_be_nil
    end
  end

  it "should not set Content-Type header on a 205 response, but should set a Content-Length header" do
    app do |r|
      response.status = 205
      throw :halt, response.finish
    end

    header(RodaResponseHeaders::CONTENT_TYPE).must_be_nil
    if Rack.release < '2.0.2'
      header(RodaResponseHeaders::CONTENT_LENGTH).must_be_nil
    else
      header(RodaResponseHeaders::CONTENT_LENGTH).must_equal '0'
    end
  end

  it "should not overwrite existing status" do
    s, h, b = nil
    app do |r|
      response.status = 500
      s, h, b = response.finish
      ''
    end

    body.must_equal ''
    s.must_equal 500
    h[RodaResponseHeaders::CONTENT_TYPE].must_equal 'text/html'
    b.join.length.must_equal 0
  end
end

describe "response #finish_with_body" do
  it "should use given body" do
    app do |r|
      throw :halt, response.finish_with_body(['123'])
    end

    body.must_equal '123'
  end

  it "should set status to 200 if status has not been set" do
    app do |r|
      throw :halt, response.finish_with_body([])
    end

    status.must_equal 200
  end

  it "should not set Content-Length header" do
    app do |r|
      response.write 'a'
      response[RodaResponseHeaders::CONTENT_LENGTH].must_be_nil
      throw :halt, response.finish_with_body(['123'])
    end

    header(RodaResponseHeaders::CONTENT_LENGTH).must_be_nil
  end

  it "should not overwrite existing status" do
    app do |r|
      response.status = 500
      throw :halt, response.finish_with_body(['123'])
    end

    status.must_equal 500
  end
end

describe "response #redirect" do
  it "should set location and status" do
    app do |r|
      r.on 'a' do
        response.redirect '/foo', 303
      end
      r.on do
        response.redirect '/bar'
      end
    end

    status('/a').must_equal 303
    status.must_equal 302
    header(RodaResponseHeaders::LOCATION, '/a').must_equal '/foo'
    header(RodaResponseHeaders::LOCATION).must_equal '/bar'
  end
end

describe "response #empty?" do
  it "should return whether the body is empty" do
    app do |r|
      r.on 'a' do
        response['foo'] = response.empty?.to_s
      end
      r.on do
        response.write 'a'
        response['foo'] = response.empty?.to_s
      end
    end

    header('foo', '/a').must_equal 'true'
    header('foo').must_equal 'false'
  end
end

describe "response #inspect" do
  it "should return information about response" do
    app(:bare) do
      def self.inspect
        'Foo'
      end

      route do |r|
        response.status = 200
        response.inspect
      end
    end

    body.must_equal '#<Foo::RodaResponse 200 {} []>'
  end
end

describe "roda_class" do
  it "should return the related roda subclass" do
    app do |r|
      self.class.opts[:a] = 'a'
      response.class.roda_class.opts[:a] + response.roda_class.opts[:a]
    end

    body.must_equal  "aa"
  end
end
