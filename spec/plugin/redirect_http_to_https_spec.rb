require_relative "../spec_helper"

describe "redirect_http_to_https plugin" do 
  before do
    app(:redirect_http_to_https) do |r|
      r.get 'a' do
        "a-#{r.ssl?}"
      end

      r.redirect_http_to_https

      "x-#{r.ssl?}"
    end
  end

  it "should not redirect before call to r.redirect_http_to_https" do
    body('/a').must_equal 'a-false'
    body('/a', 'HTTPS'=>'on').must_equal 'a-true'
  end

  it "r.redirect_http_to_https redirects HTTP requests to HTTP" do
    s, h, b = req('/b', 'HTTP_HOST'=>'foo.com')
    s.must_equal 301
    h[RodaResponseHeaders::LOCATION].must_equal 'https://foo.com/b'
    b.must_be_empty
    body('/b', 'HTTPS'=>'on').must_equal 'x-true'
  end

  it "uses 301 for HEAD redirects by default" do
    status('/b', 'HTTP_HOST'=>'foo.com', 'REQUEST_METHOD'=>'HEAD').must_equal 301
  end

  it "uses 307 for POST redirects by default" do
    status('/b', 'HTTP_HOST'=>'foo.com', 'REQUEST_METHOD'=>'POST').must_equal 307
  end

  it "includes query string when redirecting" do
    header(RodaResponseHeaders::LOCATION, '/b', 'HTTP_HOST'=>'foo.com', 'QUERY_STRING'=>'foo=bar').must_equal 'https://foo.com/b?foo=bar'
  end

  it "supports :body option" do
    @app.plugin :redirect_http_to_https, :body=>'RTHS'
    s, h, b = req('/b', 'HTTP_HOST'=>'foo.com')
    s.must_equal 301
    h[RodaResponseHeaders::LOCATION].must_equal 'https://foo.com/b'
    b.must_equal ['RTHS']
  end

  it "supports :headers option" do
    @app.plugin :redirect_http_to_https, :headers=>{'foo'=>'bar'}
    s, h, b = req('/b', 'HTTP_HOST'=>'foo.com')
    s.must_equal 301
    h[RodaResponseHeaders::LOCATION].must_equal 'https://foo.com/b'
    h['foo'].must_equal 'bar'
    b.must_be_empty
  end

  it "supports :host option" do
    @app.plugin :redirect_http_to_https, :host=>'bar.foo.com'
    s, h, b = req('/b', 'HTTP_HOST'=>'foo.com')
    s.must_equal 301
    h[RodaResponseHeaders::LOCATION].must_equal 'https://bar.foo.com/b'
    b.must_be_empty
  end

  it "supports :port option" do
    @app.plugin :redirect_http_to_https, :port=>444
    s, h, b = req('/b', 'HTTP_HOST'=>'foo.com')
    s.must_equal 301
    h[RodaResponseHeaders::LOCATION].must_equal 'https://foo.com:444/b'
    b.must_be_empty
  end

  it "supports :host and :port options together" do
    @app.plugin :redirect_http_to_https, :host=>'bar.foo.com', :port=>444
    s, h, b = req('/b', 'HTTP_HOST'=>'foo.com')
    s.must_equal 301
    h[RodaResponseHeaders::LOCATION].must_equal 'https://bar.foo.com:444/b'
    b.must_be_empty
  end

  it "supports :status_map option" do
    map = Hash.new(302)
    map['GET'] = 301
    @app.plugin :redirect_http_to_https, :status_map=>map
    status('/b', 'HTTP_HOST'=>'foo.com', 'REQUEST_METHOD'=>'GET').must_equal 301
    status('/b', 'HTTP_HOST'=>'foo.com', 'REQUEST_METHOD'=>'HEAD').must_equal 302
  end

  it "raise for :status_map that does not handle request mthod" do
    @app.plugin :redirect_http_to_https, :status_map=>{'GET'=>302}
    status('/b', 'HTTP_HOST'=>'foo.com', 'REQUEST_METHOD'=>'GET').must_equal 302
    proc{status('/b', 'HTTP_HOST'=>'foo.com', 'REQUEST_METHOD'=>'HEAD')}.must_raise Roda::RodaError
  end
end
