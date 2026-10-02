# frozen_string_literal: true

require 'minitest/autorun'

require 'rack'
require 'rack/contrib/try_static'
require 'rack/mock'

class CloseCountingBody
  attr_reader :close_count

  def initialize(content = 'body')
    @content = content
    @close_count = 0
  end

  def each
    yield @content
  end

  def close
    @close_count += 1
  end
end

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

  describe 'suffix priority' do
    it 'should serve the bare path when the empty suffix matches' do
      res = request(build_options(:try => ['.html'])).get('/statics/existing.html')
      _(res.ok?).must_equal(true)
      _(res.body.strip).must_equal "existing.html"
    end

    it 'should serve the first matching suffix' do
      res = request(build_options(:try => ['.html', '/index.html'])).get('/statics/existing')
      _(res.ok?).must_equal(true)
      _(res.body.strip).must_equal "existing.html"
    end

    it 'should serve the last matching suffix when earlier ones miss' do
      res = request(build_options(:try => ['/nothing', '/index.htm'])).get('/statics')
      _(res.ok?).must_equal(true)
      _(res.body.strip).must_equal "index.htm"
    end

    it 'should respect the configured order of the suffixes' do
      res = request(build_options(:try => ['/index.htm', '/index.html'])).get('/statics')
      _(res.ok?).must_equal(true)
      _(res.body.strip).must_equal "index.htm"
    end

    it 'should call the app with the original path when no suffix matches' do
      seen_path = nil
      app = lambda { |env| seen_path = env['PATH_INFO']; [200, {}, ['Hello World']] }
      Rack::TryStatic.new(app, build_options(:try => ['.nope'])).
        call(Rack::MockRequest.env_for('/statics/missing'))
      _(seen_path).must_equal '/statics/missing'
    end
  end

  describe 'losing candidate responses' do
    def try_static_with(static_behavior, try: ['.a', '.b', '.c'])
      attempts = []
      static = lambda do |env|
        attempts << env['PATH_INFO']
        static_behavior.call(env['PATH_INFO'])
      end
      middleware = Rack::TryStatic.new(
        lambda { |_| [200, {}, ['from app']] },
        build_options(:try => try))
      middleware.instance_variable_set(:@static, static)
      [middleware, attempts]
    end

    it 'should close 403, 404 and 405 bodies and leave the winning body to the consumer' do
      losers = [403, 404, 405].map { |status| [status, {}, CloseCountingBody.new("status #{status}")] }
      winner_body = CloseCountingBody.new('winner')
      queue = losers + [[200, {}, winner_body]]
      middleware, attempts = try_static_with(lambda { |_| queue.shift })

      status, _headers, body = middleware.call(Rack::MockRequest.env_for('/thing'))

      _(status).must_equal 200
      _(attempts).must_equal ['/thing', '/thing.a', '/thing.b', '/thing.c']
      losers.each { |(_, _, loser_body)| _(loser_body.close_count).must_equal 1 }
      _(winner_body.close_count).must_equal 0
      body.close
      _(winner_body.close_count).must_equal 1
    end

    it 'should stop trying and restore PATH_INFO when a candidate raises' do
      calls = 0
      middleware, attempts = try_static_with(lambda do |_|
        calls += 1
        raise 'boom' if calls == 2
        [404, {}, CloseCountingBody.new]
      end)
      env = Rack::MockRequest.env_for('/thing')

      _(proc { middleware.call(env) }).must_raise(RuntimeError)
      _(attempts).must_equal ['/thing', '/thing.a']
      _(env['PATH_INFO']).must_equal '/thing'
    end

    it 'should stop trying and restore PATH_INFO when closing a losing body fails' do
      failing_body = CloseCountingBody.new
      def failing_body.close
        raise 'close failed'
      end
      middleware, attempts = try_static_with(lambda { |_| [404, {}, failing_body] })
      env = Rack::MockRequest.env_for('/thing')

      error = _(proc { middleware.call(env) }).must_raise(RuntimeError)
      _(error.message).must_equal 'close failed'
      _(attempts).must_equal ['/thing']
      _(env['PATH_INFO']).must_equal '/thing'
    end
  end

  describe 'PATH_INFO restoration' do
    it 'should restore PATH_INFO for outer middleware after the call returns' do
      try_static = Rack::TryStatic.new(
        lambda { |_| [200, {}, ['Hello World']] },
        build_options(:try => ['.html', '/index.html']))
      path_after_call = nil
      outer = lambda do |env|
        response = try_static.call(env)
        path_after_call = env['PATH_INFO']
        response
      end

      outer.call(Rack::MockRequest.env_for('/statics'))
      _(path_after_call).must_equal '/statics'
    end
  end
end
