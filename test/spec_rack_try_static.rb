# frozen_string_literal: true

require 'minitest/autorun'

require 'rack'
require 'rack/contrib/try_static'
require 'rack/mock'

def build_options(opts)
  {
    :urls => %w[/],
    :root => ::File.expand_path(::File.dirname(__FILE__)),
  }.merge(opts)
end

def request(options = {})
  @request =
    Rack::MockRequest.new(
      Rack::Lint.new(
        Rack::TryStatic.new(
          lambda { |_| [200, {}, ["Hello World"]]},
          options)))
end

# A response body that records how many times it was closed.
class CloseCountingBody
  attr_reader :close_count

  def initialize(payload = "candidate")
    @payload = payload
    @close_count = 0
  end

  def each
    yield @payload
  end

  def close
    @close_count += 1
  end
end

# A response body whose close raises, simulating a failed cleanup.
class ExplodingCloseBody < CloseCountingBody
  def close
    super
    raise "closing loser body failed"
  end
end

# Stand-in for Rack::Static that replies from a path => response script.
# A +:raise+ scripted response makes the candidate application raise.
class FakeStatic
  attr_reader :calls, :seen_paths

  def initialize(responses)
    @responses = responses
    @calls = 0
    @seen_paths = []
  end

  def call(env)
    @calls += 1
    path = env['PATH_INFO']
    @seen_paths << path
    raise "candidate exploded" if @responses[path] == :raise
    @responses.fetch(path)
  end
end

def try_static_with_fake(app, try, fake_static)
  middleware = Rack::TryStatic.new(app, build_options(:try => try))
  middleware.instance_variable_set(:@static, fake_static)
  middleware
end

def recording_app(recorder)
  lambda do |env|
    recorder[:path] = env['PATH_INFO']
    [200, {}, []]
  end
end

def env_for(path, method = 'GET')
  Rack::MockRequest.env_for(path, :method => method)
end

describe "Rack::TryStatic" do
  describe 'when file cannot be found' do
    it 'should call call app' do
      res = request(build_options(:try => ['html'])).get('/statics')
      _(res.ok?).must_equal(true)
      _(res.body).must_equal "Hello World"
    end
  end

  describe 'when file can be found' do
    it 'should serve first found' do
      res = request(build_options(:try => ['.html', '/index.html', '/index.htm'])).get('/statics')
      _(res.ok?).must_equal(true)
      _(res.body.strip).must_equal "index.html"
    end
  end

  describe 'when path_info maps directly to file' do
    it 'should serve existing' do
      res = request(build_options(:try => ['/index.html'])).get('/statics/existing.html')
      _(res.ok?).must_equal(true)
      _(res.body.strip).must_equal "existing.html"
    end
  end

  describe 'when sharing options' do
    it 'should not mutate given options' do
      org_options = build_options  :try => ['/index.html']
      given_options = org_options.dup
      _(request(given_options).get('/statics').ok?).must_equal(true)
      _(request(given_options).get('/statics').ok?).must_equal(true)
      _(given_options).must_equal org_options
    end
  end

  describe 'suffix ordering' do
    it 'tries the empty suffix before the configured suffixes' do
      res = request(build_options(:try => ['.html'])).get('/statics/test')
      _(res.ok?).must_equal(true)
      _(res.body).must_equal "rubyrack"
    end

    it 'serves a match found by the first configured suffix' do
      res = request(build_options(:try => ['.html', '/index.html', '/index.htm'])).get('/statics')
      _(res.ok?).must_equal(true)
      _(res.body.strip).must_equal "index.html"
    end

    it 'serves a match found only by the last suffix' do
      res = request(build_options(:try => ['.bogus', '.html', '/index.html'])).get('/statics')
      _(res.ok?).must_equal(true)
      _(res.body.strip).must_equal "index.html"
    end

    it 'falls through to the app when every suffix misses' do
      res = request(build_options(:try => ['.html', '/nope.html'])).get('/statics/missing')
      _(res.ok?).must_equal(true)
      _(res.body).must_equal "Hello World"
    end

    it 'falls through to the app when every candidate answers 405' do
      res = request(build_options(:try => ['.html'])).post('/statics/index.html')
      _(res.ok?).must_equal(true)
      _(res.body).must_equal "Hello World"
    end
  end

  describe 'loser response bodies' do
    [403, 404, 405].each do |status|
      it "closes the #{status} body once and keeps trying later suffixes" do
        loser = CloseCountingBody.new
        winner = CloseCountingBody.new("winner")
        static = FakeStatic.new(
          '/foo' => [status, {}, loser],
          '/foo.html' => [200, {}, winner])
        recorder = {}
        middleware = try_static_with_fake(recording_app(recorder), ['.html'], static)
        env = env_for('/foo')

        response_status, _headers, response_body = middleware.call(env)

        _(response_status).must_equal 200
        _(static.seen_paths).must_equal ['/foo', '/foo.html']
        _(loser.close_count).must_equal 1
        _(winner.close_count).must_equal 0
        _(env['PATH_INFO']).must_equal '/foo'
        _(recorder).wont_include :path

        response_body.close
        _(winner.close_count).must_equal 1
      end
    end

    it 'closes every loser body before delegating to the app' do
      first = CloseCountingBody.new("first")
      second = CloseCountingBody.new("second")
      static = FakeStatic.new(
        '/foo' => [404, {}, first],
        '/foo.html' => [405, {}, second])
      recorder = {}
      middleware = try_static_with_fake(recording_app(recorder), ['.html'], static)
      env = env_for('/foo')

      status, _headers, _body = middleware.call(env)

      _(status).must_equal 200
      _(first.close_count).must_equal 1
      _(second.close_count).must_equal 1
      _(recorder[:path]).must_equal '/foo'
      _(env['PATH_INFO']).must_equal '/foo'
    end
  end

  describe 'cleanup failures' do
    it 'stops trying candidates and restores PATH_INFO when a candidate raises' do
      later = CloseCountingBody.new
      static = FakeStatic.new(
        '/foo' => :raise,
        '/foo.html' => [404, {}, later])
      middleware = try_static_with_fake(recording_app({}), ['.html'], static)
      env = env_for('/foo')

      error = assert_raises(RuntimeError) { middleware.call(env) }

      _(error.message).must_equal "candidate exploded"
      _(static.calls).must_equal 1
      _(later.close_count).must_equal 0
      _(env['PATH_INFO']).must_equal '/foo'
    end

    it 'stops trying candidates and restores PATH_INFO when closing a loser fails' do
      broken = ExplodingCloseBody.new
      later = CloseCountingBody.new
      static = FakeStatic.new(
        '/foo' => [403, {}, broken],
        '/foo.html' => [404, {}, later])
      middleware = try_static_with_fake(recording_app({}), ['.html'], static)
      env = env_for('/foo')

      error = assert_raises(RuntimeError) { middleware.call(env) }

      _(error.message).must_equal "closing loser body failed"
      _(broken.close_count).must_equal 1
      _(static.calls).must_equal 1
      _(later.close_count).must_equal 0
      _(env['PATH_INFO']).must_equal '/foo'
    end
  end

  describe 'PATH_INFO restoration' do
    it 'restores PATH_INFO after a suffix match is returned' do
      middleware = Rack::TryStatic.new(
        lambda { |_| [200, {}, []] },
        build_options(:try => ['/index.html']))
      env = env_for('/statics')

      status, _headers, body = middleware.call(env)

      _(status).must_equal 200
      _(env['PATH_INFO']).must_equal '/statics'
      body.close
    end

    it 'passes the original PATH_INFO to the downstream app on a total miss' do
      seen = nil
      app = lambda { |env| seen = env['PATH_INFO']; [200, {}, []] }
      middleware = Rack::TryStatic.new(app, build_options(:try => ['.html']))
      env = env_for('/statics/missing')

      status, _headers, _body = middleware.call(env)

      _(status).must_equal 200
      _(seen).must_equal '/statics/missing'
      _(env['PATH_INFO']).must_equal '/statics/missing'
    end
  end
end
