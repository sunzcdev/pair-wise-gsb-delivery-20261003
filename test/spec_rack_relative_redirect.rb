# frozen_string_literal: true

require 'minitest/autorun'
require 'rack/mock'
require 'rack/contrib/relative_redirect'
require 'fileutils'

describe Rack::RelativeRedirect do
  def request(opts={}, &block)
    @def_status = opts[:status] if opts[:status]
    @def_location = opts[:location] if opts[:location]
    yield Rack::MockRequest.new(Rack::Lint.new(Rack::RelativeRedirect.new(@def_app, &opts[:block]))).get(opts[:path]||@def_path, opts[:headers]||{})
  end

  before do
    @def_path = '/path/to/blah'
    @def_status = 301
    @def_location = '/redirect/to/blah'
    @def_app = lambda { |env| [@def_status, {'location' => @def_location}, [""]]}
  end

  specify "should rewrite Location on all the redirect codes" do
    [301, 302, 303, 307, 308].each do |status|
      request(:status => status) do |r|
        _(r.status).must_equal(status)
        _(r.headers['Location']).must_equal('http://example.org/redirect/to/blah')
      end
    end
  end

  specify "should not rewrite Location on other status codes" do
    [200, 201, 300, 304, 305, 306, 404, 500].each do |status|
      request(:status => status) do |r|
        _(r.status).must_equal(status)
        _(r.headers['Location']).must_equal('/redirect/to/blah')
      end
    end
  end

  specify "should make the location url an absolute url if currently a relative url" do
    request do |r|
      _(r.status).must_equal(301)
      _(r.headers['Location']).must_equal('http://example.org/redirect/to/blah')
    end
    request(:status=>302, :location=>'/redirect') do |r|
      _(r.status).must_equal(302)
      _(r.headers['Location']).must_equal('http://example.org/redirect')
    end
  end

  specify "should resolve a relative reference against the full current path, not its directory" do
    request(:status=>302, :location=>'?page=2', :path=>'/reports/monthly') do |r|
      _(r.status).must_equal(302)
      _(r.headers['Location']).must_equal('http://example.org/reports/monthly?page=2')
    end
    request(:status=>302, :location=>'#section', :path=>'/reports/monthly') do |r|
      _(r.status).must_equal(302)
      _(r.headers['Location']).must_equal('http://example.org/reports/monthly#section')
    end
  end

  specify "should keep the current query when resolving a fragment-only reference" do
    request(:status=>302, :location=>'#section', :path=>'/reports/monthly?sort=date') do |r|
      _(r.headers['Location']).must_equal('http://example.org/reports/monthly?sort=date#section')
    end
  end

  specify "should resolve a network-path reference using the current scheme" do
    request(:status=>302, :location=>'//cdn.example.com/assets/app.js') do |r|
      _(r.status).must_equal(302)
      _(r.headers['Location']).must_equal('http://cdn.example.com/assets/app.js')
    end
    request(:status=>302, :location=>'//cdn.example.com/assets/app.js',
            :block=>proc{|env, res| "https://example.org"}) do |r|
      _(r.headers['Location']).must_equal('https://cdn.example.com/assets/app.js')
    end
  end

  specify "should include SCRIPT_NAME when resolving references under a mounted prefix" do
    request(:status=>302, :location=>'?page=2', :path=>'/reports/monthly',
            :headers=>{'SCRIPT_NAME'=>'/console'}) do |r|
      _(r.status).must_equal(302)
      _(r.headers['Location']).must_equal('http://example.org/console/reports/monthly?page=2')
    end
    request(:status=>302, :location=>'next', :path=>'/reports/monthly',
            :headers=>{'SCRIPT_NAME'=>'/console'}) do |r|
      _(r.headers['Location']).must_equal('http://example.org/console/reports/next')
    end
    request(:status=>302, :location=>'/login',
            :headers=>{'SCRIPT_NAME'=>'/console'}) do |r|
      _(r.headers['Location']).must_equal('http://example.org/login')
    end
  end

  specify "should not url-decode the current path, so %2F stays inside its segment" do
    request(:status=>302, :location=>'next', :path=>'/reports/a%2Fb') do |r|
      _(r.headers['Location']).must_equal('http://example.org/reports/next')
    end
    request(:status=>302, :location=>'?page=2', :path=>'/reports/a%2Fb') do |r|
      _(r.headers['Location']).must_equal('http://example.org/reports/a%2Fb?page=2')
    end
    request(:status=>302, :location=>'a%2Fb', :path=>'/reports/monthly') do |r|
      _(r.headers['Location']).must_equal('http://example.org/reports/a%2Fb')
    end
  end

  specify "should use the request path if the relative url is given and doesn't start with a slash" do
    request(:status=>303, :location=>'redirect/to/blah') do |r|
      _(r.status).must_equal(303)
      _(r.headers['Location']).must_equal('http://example.org/path/to/redirect/to/blah')
    end
    request(:status=>303, :location=>'redirect') do |r|
      _(r.status).must_equal(303)
      _(r.headers['Location']).must_equal('http://example.org/path/to/redirect')
    end
  end

  specify "should use a given block to make the url absolute" do
    request(:block=>proc{|env, res| "https://example.org"}) do |r|
      _(r.status).must_equal(301)
      _(r.headers['Location']).must_equal('https://example.org/redirect/to/blah')
    end
    request(:status=>303, :location=>'/redirect', :block=>proc{|env, res| "https://e.org:9999/blah"}) do |r|
      _(r.status).must_equal(303)
      _(r.headers['Location']).must_equal('https://e.org:9999/blah/redirect')
    end
  end

  specify "should not modify the location url unless the response is a redirect" do
    status = 200
    @def_app = lambda { |env| [status, {'content-type' => "text/html"}, [""]]}
    request do |r|
      _(r.status).must_equal(200)
      _(r.headers).wont_include('Location')
    end
    status = 404
    @def_app = lambda { |env| [status, {'content-type' => "text/html", 'location' => 'redirect'}, [""]]}
    request do |r|
      _(r.status).must_equal(404)
      _(r.headers['Location']).must_equal('redirect')
    end
  end

  specify "should not modify the location url if it is already an absolute url" do
    request(:location=>'https://example.org/') do |r|
      _(r.status).must_equal(301)
      _(r.headers['Location']).must_equal('https://example.org/')
    end
    request(:status=>302, :location=>'https://e.org:9999/redirect') do |r|
      _(r.status).must_equal(302)
      _(r.headers['Location']).must_equal('https://e.org:9999/redirect')
    end
  end

  specify "should rewrite a copy of the headers without touching the app's header object" do
    original_headers = {'Location' => '?page=2'}
    app = lambda { |env| [302, original_headers, []] }
    middleware = Rack::RelativeRedirect.new(app)
    status, headers, body = middleware.call(Rack::MockRequest.env_for('/reports/monthly'))
    _(status).must_equal(302)
    _(headers['Location']).must_equal('http://example.org/reports/monthly?page=2')
    _(headers).wont_be_same_as(original_headers)
    _(original_headers['Location']).must_equal('?page=2')
  end

  specify "should pass a response without a redirect Location straight through without reading the body" do
    original_headers = {'Content-Type' => 'text/plain'}
    body = Object.new
    def body.each; raise "body must not be read"; end
    app = lambda { |env| [200, original_headers, body] }
    middleware = Rack::RelativeRedirect.new(app)
    status, headers, returned_body = middleware.call(Rack::MockRequest.env_for('/reports/monthly'))
    _(status).must_equal(200)
    _(headers).must_be_same_as(original_headers)
    _(returned_body).must_be_same_as(body)
  end
end
