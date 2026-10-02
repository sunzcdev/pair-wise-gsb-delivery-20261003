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

  specify "should not modify the location url if it is an absolute url with another scheme" do
    request(:location=>'ftp://example.org/files') do |r|
      _(r.status).must_equal(301)
      _(r.headers['Location']).must_equal('ftp://example.org/files')
    end
  end

  specify "should resolve query-only and fragment-only locations against the request path" do
    request(:path=>'/reports/monthly', :location=>'?page=2') do |r|
      _(r.status).must_equal(301)
      _(r.headers['Location']).must_equal('http://example.org/reports/monthly?page=2')
    end
    request(:path=>'/reports/monthly', :location=>'#summary') do |r|
      _(r.status).must_equal(301)
      _(r.headers['Location']).must_equal('http://example.org/reports/monthly#summary')
    end
    request(:path=>'/reports/monthly?year=2026', :location=>'#summary') do |r|
      _(r.status).must_equal(301)
      _(r.headers['Location']).must_equal('http://example.org/reports/monthly?year=2026#summary')
    end
  end

  specify "should resolve network-path references against the request scheme" do
    request(:location=>'//cdn.example.org/assets/app.js') do |r|
      _(r.status).must_equal(301)
      _(r.headers['Location']).must_equal('http://cdn.example.org/assets/app.js')
    end
  end

  specify "should resolve relative references against SCRIPT_NAME and PATH_INFO" do
    mounted = {'SCRIPT_NAME' => '/console'}
    request(:path=>'/reports/monthly', :headers=>mounted, :location=>'?page=2') do |r|
      _(r.status).must_equal(301)
      _(r.headers['Location']).must_equal('http://example.org/console/reports/monthly?page=2')
    end
    request(:path=>'/reports/monthly', :headers=>mounted, :location=>'weekly') do |r|
      _(r.status).must_equal(301)
      _(r.headers['Location']).must_equal('http://example.org/console/reports/weekly')
    end
    request(:path=>'/reports/monthly', :headers=>mounted, :location=>'/reports') do |r|
      _(r.status).must_equal(301)
      _(r.headers['Location']).must_equal('http://example.org/reports')
    end
  end

  specify "should keep encoded characters such as %2F inside their path segment" do
    request(:path=>'/files/a%2Fb/monthly', :location=>'download') do |r|
      _(r.status).must_equal(301)
      _(r.headers['Location']).must_equal('http://example.org/files/a%2Fb/download')
    end
    request(:path=>'/files/a%2Fb/monthly', :location=>'?page=2') do |r|
      _(r.status).must_equal(301)
      _(r.headers['Location']).must_equal('http://example.org/files/a%2Fb/monthly?page=2')
    end
  end

  specify "should not modify the headers object returned by the app" do
    app_headers = {'location' => '/redirect/to/blah'}
    @def_app = lambda { |env| [301, app_headers, [""]] }
    request do |r|
      _(r.status).must_equal(301)
      _(r.headers['Location']).must_equal('http://example.org/redirect/to/blah')
    end
    _(app_headers).must_equal({'location' => '/redirect/to/blah'})
  end

  specify "should pass non-redirect responses through untouched" do
    headers = {'content-type' => 'text/plain'}
    body = ['response body']
    app = lambda { |env| [200, headers, body] }
    status, res_headers, res_body = Rack::RelativeRedirect.new(app).call(Rack::MockRequest.env_for('/path/to/blah'))
    _(status).must_equal(200)
    _(res_headers).must_be_same_as(headers)
    _(res_body).must_be_same_as(body)
  end
end
