# frozen_string_literal: true

require 'minitest/autorun'
require 'rack'
require 'rack/mock'
require 'rack/contrib/route_exceptions'

class RouteTestError < StandardError; end
class RouteFooError < StandardError; end
class RouteBarError < RouteFooError; end
class RoutePageError < StandardError; end

describe "Rack::RouteExceptions" do
  PATH_INFO_KEY = Rack::RouteExceptions::PATH_INFO
  EXCEPTION_KEY = Rack::RouteExceptions::EXCEPTION
  RETURNED_KEY  = Rack::RouteExceptions::RETURNED

  DEFAULT_ROUTES = [[Exception, '/error/internal']].freeze

  def build_backend(visits)
    lambda do |env|
      visits << env['PATH_INFO']
      case env['PATH_INFO']
      when '/boom'           then raise RouteTestError, 'boom'
      when '/raise_standard' then raise StandardError, 'plain'
      when '/raise_foo'      then raise RouteFooError, 'foo'
      when '/raise_bar'      then raise RouteBarError, 'bar'
      when '/raise_script'   then raise ScriptError
      when '/error/fail'     then raise RoutePageError, 'page failure'
      when '/error/internal', '/error/standard', '/error/standard2',
           '/error/foo', '/error/instance'
        [
          500,
          { 'content-type' => 'text/plain' },
          ["page=#{env['PATH_INFO']};" \
           "original=#{env[PATH_INFO_KEY]};" \
           "exception=#{env[EXCEPTION_KEY].class};" \
           "returned=#{env[RETURNED_KEY].inspect}"]
        ]
      else
        [404, { 'content-type' => 'text/plain' }, ['not found']]
      end
    end
  end

  def build_middleware(routes = nil, visits = [])
    backend = build_backend(visits)
    middleware =
      if routes
        Rack::RouteExceptions.new(backend, routes)
      else
        Rack::RouteExceptions.new(backend)
      end
    [middleware, visits]
  end

  def call(middleware, path)
    env = Rack::MockRequest.env_for(path)
    status, _headers, body = middleware.call(env)
    [status, body.join, env]
  end

  after do
    Rack::RouteExceptions::ROUTES.replace(DEFAULT_ROUTES.map(&:dup))
  end

  specify "routes to the most specific matching exception class" do
    Rack::RouteExceptions.route(StandardError, '/error/standard')
    Rack::RouteExceptions.route(RouteFooError, '/error/foo')
    middleware, = build_middleware

    _status, body, = call(middleware, '/raise_standard')
    _(body).must_include 'page=/error/standard'

    _status, body, = call(middleware, '/boom')
    _(body).must_include 'page=/error/standard'

    _status, body, = call(middleware, '/raise_foo')
    _(body).must_include 'page=/error/foo'

    _status, body, = call(middleware, '/raise_bar')
    _(body).must_include 'page=/error/foo'

    _status, body, = call(middleware, '/raise_script')
    _(body).must_include 'page=/error/internal'
  end

  specify "uses the last registration for a repeated exception class" do
    Rack::RouteExceptions.route(StandardError, '/error/standard')
    Rack::RouteExceptions.route(StandardError, '/error/standard2')
    middleware, = build_middleware

    _status, body, = call(middleware, '/boom')
    _(body).must_include 'page=/error/standard2'
  end

  specify "keeps instance routes private to that instance" do
    visits_one = []
    visits_two = []
    middleware_one, = build_middleware({ StandardError => '/error/instance' }, visits_one)
    middleware_two, = build_middleware(nil, visits_two)

    Rack::RouteExceptions.route(StandardError, '/error/standard')

    _status, body, = call(middleware_one, '/boom')
    _(body).must_include 'page=/error/instance'

    _status, body, = call(middleware_two, '/boom')
    _(body).must_include 'page=/error/internal'

    _(visits_one).must_equal ['/boom', '/error/instance']
    _(visits_two).must_equal ['/boom', '/error/internal']
  end

  specify "snapshots class-level routes at creation time" do
    Rack::RouteExceptions.route(StandardError, '/error/standard')
    middleware, visits = build_middleware

    Rack::RouteExceptions.route(StandardError, '/error/standard2')

    _status, body, = call(middleware, '/boom')
    _(body).must_include 'page=/error/standard'
    _(visits).must_equal ['/boom', '/error/standard']
  end

  specify "exposes the original path, exception and returned response to the handler" do
    Rack::RouteExceptions.route(StandardError, '/error/standard')
    middleware, = build_middleware

    _status, body, env = call(middleware, '/boom')
    _(body).must_include 'original=/boom'
    _(body).must_include 'exception=RouteTestError'
    _(body).must_include 'returned=nil'

    _(env['PATH_INFO']).must_equal '/boom'
    _(env).wont_include PATH_INFO_KEY
    _(env).wont_include EXCEPTION_KEY
    _(env).wont_include RETURNED_KEY
  end

  specify "restores pre-existing diagnostic fields after handling" do
    Rack::RouteExceptions.route(StandardError, '/error/standard')
    middleware, = build_middleware
    env = Rack::MockRequest.env_for('/boom')
    env[PATH_INFO_KEY] = '/previous'
    env[EXCEPTION_KEY] = 'previous exception'
    env[RETURNED_KEY] = 'previous response'

    middleware.call(env)

    _(env['PATH_INFO']).must_equal '/boom'
    _(env[PATH_INFO_KEY]).must_equal '/previous'
    _(env[EXCEPTION_KEY]).must_equal 'previous exception'
    _(env[RETURNED_KEY]).must_equal 'previous response'
  end

  specify "reraises a failing error page without routing it back into an error page" do
    routes = { RoutePageError => '/error/foo', Exception => '/error/fail' }
    middleware, visits = build_middleware(routes)

    error = assert_raises(RoutePageError) do
      middleware.call(Rack::MockRequest.env_for('/boom'))
    end
    _(error.message).must_equal 'page failure'

    _(visits).must_equal ['/boom', '/error/fail']
    _(visits).wont_include '/error/foo'
  end

  specify "restores the caller env when the error page raises" do
    middleware, = build_middleware({ Exception => '/error/fail' })
    env = Rack::MockRequest.env_for('/boom')

    assert_raises(RoutePageError) { middleware.call(env) }

    _(env['PATH_INFO']).must_equal '/boom'
    _(env).wont_include PATH_INFO_KEY
    _(env).wont_include EXCEPTION_KEY
    _(env).wont_include RETURNED_KEY
  end
end
