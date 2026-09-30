# frozen_string_literal: true

require 'rack'
require 'minitest/autorun'
require 'rack/cors'

describe Rack::Cors, 'intersection mode' do
  ORIGIN = 'https://example.test'

  def cors(order = :platform_first, options = {})
    platform = proc do |cors|
      cors.allow do
        origins '*'
        resource '/items',
                 methods: %i[get post],
                 headers: :any,
                 expose: %w[X-Shared X-Platform],
                 max_age: 600
      end
    end

    business = proc do |cors|
      cors.allow do
        origins 'example.test'
        resource '/items',
                 methods: %i[get put],
                 headers: %w[x-shared x-business],
                 expose: %w[x-shared x-business],
                 credentials: true,
                 max_age: 300
      end
    end

    layers = order == :platform_first ? [platform, business] : [business, platform]
    ::Rack::Cors.new(->(_env) { [200, {}, []] }, { mode: :intersection }.merge(options)) do |cors|
      layers.each { |layer| layer.call(cors) }
    end
  end

  def preflight(method: 'GET', headers: nil, origin: ORIGIN, path: '/items')
    env = {
      'HTTP_ORIGIN' => origin,
      'PATH_INFO' => path,
      'REQUEST_METHOD' => 'OPTIONS',
      'HTTP_ACCESS_CONTROL_REQUEST_METHOD' => method
    }
    env['HTTP_ACCESS_CONTROL_REQUEST_HEADERS'] = headers if headers
    env
  end

  it 'intersects matched policies independently of declaration order' do
    first = cors.call(preflight(headers: 'X-Shared, X-Business'))[1]
    second = cors(:business_first).call(preflight(headers: 'X-Shared, X-Business'))[1]

    expected = {
      'access-control-allow-origin' => ORIGIN,
      'access-control-allow-methods' => 'GET',
      'access-control-allow-headers' => 'X-Shared, X-Business',
      'access-control-expose-headers' => 'X-Shared',
      'access-control-max-age' => '300'
    }

    _(first.slice(*expected.keys)).must_equal expected
    _(second.slice(*expected.keys)).must_equal expected
    _(first.key?('access-control-allow-credentials')).must_equal false
    _(second.key?('access-control-allow-credentials')).must_equal false
  end

  it 'uses the same synthesized headers for actual CORS requests' do
    env = {
      'HTTP_ORIGIN' => ORIGIN,
      'PATH_INFO' => '/items',
      'REQUEST_METHOD' => 'GET'
    }

    _, headers, = cors.call(env)

    _(headers['access-control-allow-origin']).must_equal ORIGIN
    _(headers['access-control-allow-methods']).must_equal 'GET'
    _(headers['access-control-expose-headers']).must_equal 'X-Shared'
    _(headers['access-control-max-age']).must_equal '300'
    _(headers.key?('access-control-allow-credentials')).must_equal false
  end

  it 'accepts the intersection mode as a string' do
    _, headers, = cors(:platform_first, mode: 'intersection').call(preflight(headers: 'X-Shared'))

    _(headers['access-control-allow-methods']).must_equal 'GET'
  end

  it 'does not return capabilities for a method rejected by any layer' do
    _, headers, = cors.call(preflight(method: 'POST'))

    _(headers).wont_include 'access-control-allow-origin'
    _(headers).wont_include 'access-control-allow-methods'
    _(headers).wont_include 'access-control-allow-credentials'
  end

  it 'does not return capabilities for a header rejected by any layer' do
    _, headers, = cors.call(preflight(headers: 'X-Platform'))

    _(headers).wont_include 'access-control-allow-origin'
    _(headers).wont_include 'access-control-allow-headers'
  end

  it 'requires the origin to match every applicable layer' do
    _, headers, = cors.call(preflight(origin: 'https://other.test'))

    _(headers).wont_include 'access-control-allow-origin'
  end

  it 'allows credentials only when every matched layer allows them and none is public' do
    credentialed = proc do |cors|
      cors.allow do
        origins 'example.test'
        resource '/items', methods: %i[get post], credentials: true
      end
    end

    middleware = ::Rack::Cors.new(->(_env) { [200, {}, []] }, mode: :intersection) do |cors|
      credentialed.call(cors)
      cors.allow do
        origins 'example.test'
        resource '/items', methods: %i[get put], credentials: true
      end
    end

    _, headers, = middleware.call(preflight)
    _(headers['access-control-allow-credentials']).must_equal 'true'
  end

  it 'evaluates each dynamic origin and resource condition only once' do
    origin_calls = 0
    resource_calls = 0
    origin_condition = proc { |source, _env| origin_calls += 1; source == ORIGIN }
    resource_condition = proc { |_env| resource_calls += 1; true }

    build = proc do |cors|
      cors.allow do
        origins origin_condition
        resource '/items', methods: :get, if: resource_condition
      end
      cors.allow do
        origins origin_condition
        resource '/items', methods: :get, if: resource_condition
      end
    end

    middleware = ::Rack::Cors.new(->(_env) { [200, {}, []] }, mode: :intersection, &build)
    middleware.call(preflight)

    _(origin_calls).must_equal 1
    _(resource_calls).must_equal 1
  end
end
