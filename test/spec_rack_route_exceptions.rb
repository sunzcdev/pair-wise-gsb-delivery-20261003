# frozen_string_literal: true

require 'minitest/autorun'
require 'rack/mock'
require 'rack/contrib/route_exceptions'

class RouteExTestError < StandardError; end
class RouteExTestSpecial < RouteExTestError; end
class RouteExTestFatal < Exception; end

describe "Rack::RouteExceptions" do
  PATH_INFO_KEY  = Rack::RouteExceptions::PATH_INFO
  EXCEPTION_KEY  = Rack::RouteExceptions::EXCEPTION
  RETURNED_KEY   = Rack::RouteExceptions::RETURNED

  before do
    @saved_routes = Rack::RouteExceptions::ROUTES.dup
    Rack::RouteExceptions::ROUTES.replace(Exception => '/error/internal')
  end

  after do
    Rack::RouteExceptions::ROUTES.replace(@saved_routes)
  end

  def build_backend(failures, pages)
    lambda do |env|
      path = env['PATH_INFO']
      raise failures[path] if failures.key?(path)
      page = pages[path]
      page ? page.call(env) : [404, {}, ['not found']]
    end
  end

  def env_for(path = '/boom')
    Rack::MockRequest.env_for(path)
  end

  specify "routes to the most specific registered ancestor of the exception" do
    Rack::RouteExceptions.route(StandardError,     '/standard')
    Rack::RouteExceptions.route(RouteExTestError,  '/custom')

    pages = {
      '/error/internal' => ->(_env) { [200, {}, ['fatal page']] },
      '/standard'       => ->(_env) { [200, {}, ['standard page']] },
      '/custom'         => ->(env)  { [200, {}, ["custom #{env[EXCEPTION_KEY].class}"]] },
    }
    backend = build_backend(
      { '/boom' => RouteExTestSpecial.new, '/std' => StandardError.new,
        '/fatal' => RouteExTestFatal.new },
      pages
    )
    app = Rack::RouteExceptions.new(backend)

    _(Rack::MockRequest.new(app).get('/boom').body).must_equal(
      "custom RouteExTestSpecial"
    )
    _(Rack::MockRequest.new(app).get('/std').body).must_equal('standard page')
    _(Rack::MockRequest.new(app).get('/fatal').body).must_equal('fatal page')
  end

  specify "uses the last registration when the same exception class is routed twice" do
    Rack::RouteExceptions.route(RouteExTestError, '/first')
    Rack::RouteExceptions.route(RouteExTestError, '/second')

    backend = build_backend(
      { '/boom' => RouteExTestError.new },
      '/first'  => ->(_env) { [200, {}, ['first']] },
      '/second' => ->(_env) { [200, {}, ['second']] }
    )
    app = Rack::RouteExceptions.new(backend)

    _(Rack::MockRequest.new(app).get('/boom').body).must_equal('second')
  end

  specify "supports the legacy class-level []= registration" do
    Rack::RouteExceptions[RouteExTestError] = '/custom'

    backend = build_backend(
      { '/boom' => RouteExTestError.new },
      '/custom' => ->(_env) { [200, {}, ['custom']] }
    )
    app = Rack::RouteExceptions.new(backend)

    _(Rack::MockRequest.new(app).get('/boom').body).must_equal('custom')
  end

  specify "keeps instance routes private to that instance" do
    backend = build_backend(
      { '/boom' => RouteExTestSpecial.new },
      '/a' => ->(_env) { [200, {}, ['a page']] },
      '/b' => ->(_env) { [200, {}, ['b page']] }
    )
    app_a = Rack::RouteExceptions.new(backend, RouteExTestError => '/a')
    app_b = Rack::RouteExceptions.new(backend, RouteExTestError => '/b')

    _(Rack::MockRequest.new(app_a).get('/boom').body).must_equal('a page')
    _(Rack::MockRequest.new(app_b).get('/boom').body).must_equal('b page')
    _(Rack::RouteExceptions::ROUTES).wont_include(RouteExTestError)
  end

  specify "lets instance routes override the class-level defaults" do
    Rack::RouteExceptions.route(RouteExTestError, '/shared')

    backend = build_backend(
      { '/boom' => RouteExTestError.new },
      '/shared' => ->(_env) { [200, {}, ['shared']] },
      '/mine'   => ->(_env) { [200, {}, ['mine']] }
    )
    app = Rack::RouteExceptions.new(backend, RouteExTestError => '/mine')

    _(Rack::MockRequest.new(app).get('/boom').body).must_equal('mine')
  end

  specify "keeps a snapshot of routes at creation time" do
    backend = build_backend(
      { '/boom' => RouteExTestSpecial.new },
      '/standard' => ->(_env) { [200, {}, ['standard page']] },
      '/error/internal' => ->(_env) { [200, {}, ['fatal page']] }
    )
    app = Rack::RouteExceptions.new(backend)

    Rack::RouteExceptions.route(StandardError, '/standard')

    _(Rack::MockRequest.new(app).get('/boom').body).must_equal('fatal page')
  end

  specify "does not see later mutations of the routes passed to the constructor" do
    routes = { RouteExTestError => '/first' }
    backend = build_backend(
      { '/boom' => RouteExTestError.new },
      '/first'  => ->(_env) { [200, {}, ['first']] },
      '/second' => ->(_env) { [200, {}, ['second']] }
    )
    app = Rack::RouteExceptions.new(backend, routes)

    routes[RouteExTestError] = '/second'
    routes[StandardError] = '/second'

    _(Rack::MockRequest.new(app).get('/boom').body).must_equal('first')
  end

  specify "exposes the original path and exception to the error page" do
    seen = nil
    backend = build_backend(
      { '/boom' => RouteExTestSpecial.new },
      '/custom' => lambda do |env|
        seen = [
          env[PATH_INFO_KEY],
          env[EXCEPTION_KEY],
          env[RETURNED_KEY]
        ]
        [200, {}, ['handled']]
      end
    )
    app = Rack::RouteExceptions.new(backend, RouteExTestError => '/custom')

    Rack::MockRequest.new(app).get('/boom')

    _(seen[0]).must_equal('/boom')
    _(seen[1]).must_be_instance_of(RouteExTestSpecial)
    _(seen[2]).must_be_nil
  end

  specify "restores the original env after a successful error page" do
    backend = build_backend(
      { '/boom' => RouteExTestError.new },
      '/custom' => lambda do |env|
        [200, {}, ["handled at #{env[PATH_INFO_KEY]}"]]
      end
    )
    app = Rack::RouteExceptions.new(backend, RouteExTestError => '/custom')
    env = env_for

    status, _headers, body = app.call(env)

    _(status).must_equal(200)
    _(body.each.to_a).must_equal(['handled at /boom'])
    _(env['PATH_INFO']).must_equal('/boom')
    _(env.key?(PATH_INFO_KEY)).must_equal(false)
    _(env.key?(EXCEPTION_KEY)).must_equal(false)
    _(env.key?(RETURNED_KEY)).must_equal(false)
  end

  specify "re-raises the original exception when no route matches and restores env" do
    backend = lambda do |env|
      raise RouteExTestError if env['PATH_INFO'] == '/boom'
      [200, {}, ['page']]
    end
    Rack::RouteExceptions::ROUTES.clear
    app = Rack::RouteExceptions.new(backend)
    env = env_for

    _(proc { app.call(env) }).must_raise(RouteExTestError)

    _(env['PATH_INFO']).must_equal('/boom')
    _(env.key?(PATH_INFO_KEY)).must_equal(false)
    _(env.key?(EXCEPTION_KEY)).must_equal(false)
  end

  specify "re-enters the application only once when the error page raises too" do
    page_calls = 0
    backend = build_backend(
      { '/boom' => RouteExTestError.new },
      '/custom' => lambda do |_env|
        page_calls += 1
        raise RouteExTestSpecial, 'error page failed'
      end
    )
    app = Rack::RouteExceptions.new(
      backend,
      RouteExTestError => '/custom',
      RouteExTestSpecial => '/custom'
    )
    env = env_for

    error = _(proc { app.call(env) }).must_raise(RouteExTestSpecial)

    _(error.message).must_equal('error page failed')
    _(page_calls).must_equal(1)
    _(env['PATH_INFO']).must_equal('/boom')
    _(env.key?(PATH_INFO_KEY)).must_equal(false)
    _(env.key?(EXCEPTION_KEY)).must_equal(false)
    _(env.key?(RETURNED_KEY)).must_equal(false)
  end

  specify "restores pre-existing diagnostic fields after handling" do
    backend = build_backend(
      { '/boom' => RouteExTestError.new },
      '/custom' => ->(_env) { [200, {}, ['handled']] }
    )
    app = Rack::RouteExceptions.new(backend, RouteExTestError => '/custom')
    env = env_for
    env[PATH_INFO_KEY] = '/previous-path'
    env[EXCEPTION_KEY] = :previous_exception
    env[RETURNED_KEY] = :previous_return

    app.call(env)

    _(env['PATH_INFO']).must_equal('/boom')
    _(env[PATH_INFO_KEY]).must_equal('/previous-path')
    _(env[EXCEPTION_KEY]).must_equal(:previous_exception)
    _(env[RETURNED_KEY]).must_equal(:previous_return)
  end
end
