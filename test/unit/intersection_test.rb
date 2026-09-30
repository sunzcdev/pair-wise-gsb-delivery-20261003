# frozen_string_literal: true

require 'minitest/autorun'
require 'rack'
require 'rack/cors'

describe Rack::Cors, 'intersection mode' do
  ORIGIN = 'http://example.com'

  def build_cors(order = [:platform, :business], options = {})
    counts = Hash.new(0)
    origin_proc = proc do |source, env|
      counts[:origin] += 1
      source == env['HTTP_ORIGIN']
    end
    path_proc = proc do |env|
      counts[:condition] += 1
      env['HTTP_X_ENABLED'] == 'true'
    end

    layers = {
      platform: ->(cors) do
        cors.allow do
          origins origin_proc
          resource '/',
                   methods: %i[get post],
                   headers: %w[x-shared x-platform],
                   expose: %w[x-shared x-platform],
                   max_age: 600
        end
      end,
      business: ->(cors) do
        cors.allow do
          origins ORIGIN
          resource '/',
                   methods: %i[post put],
                   headers: %w[x-shared x-business],
                   expose: %w[x-shared x-business],
                   max_age: 300,
                   credentials: true,
                   if: path_proc
        end
      end
    }

    app = ->(_env) { [200, {}, ['ok']] }
    cors = Rack::Cors.new(app, { mode: :intersection }.merge(options)) do
      order.map { |name| layers.fetch(name) }.each { |layer| layer.call(self) }
    end
    [cors, counts]
  end

  def preflight(cors, headers: 'X-Shared', method: 'POST', origin: ORIGIN)
    env = {
      'REQUEST_METHOD' => 'OPTIONS',
      'PATH_INFO' => '/',
      Rack::Cors::HTTP_ORIGIN => origin,
      Rack::Cors::HTTP_ACCESS_CONTROL_REQUEST_METHOD => method,
      'HTTP_X_ENABLED' => 'true'
    }
    env[Rack::Cors::HTTP_ACCESS_CONTROL_REQUEST_HEADERS] = headers if headers
    cors.call(env)
  end

  it 'combines every capability as an intersection in preflight responses' do
    cors, = build_cors
    status, headers, = preflight(cors)

    _(status).must_equal 200
    _(headers['access-control-allow-origin']).must_equal ORIGIN
    _(headers['access-control-allow-methods']).must_equal 'POST'
    _(headers['access-control-allow-headers']).must_equal 'X-Shared'
    _(headers['access-control-expose-headers']).must_equal 'x-shared'
    _(headers['access-control-max-age']).must_equal '300'
    _(headers).wont_include 'access-control-allow-credentials'
  end

  it 'produces the same result when layers are declared in either order' do
    _, first_headers, = preflight(build_cors([:platform, :business]).first)
    _, second_headers, = preflight(build_cors([:business, :platform]).first)

    _(second_headers).must_equal first_headers
  end

  it 'narrows wildcard request headers to the explicit headers of another layer' do
    app = ->(_env) { [200, {}, ['ok']] }
    cors = Rack::Cors.new(app, mode: :intersection) do
      allow do
        origins ORIGIN
        resource '/', methods: :post, headers: :any
      end
      allow do
        origins ORIGIN
        resource '/', methods: :post, headers: ['x-allowed']
      end
    end

    _, denied_headers, = preflight(cors, headers: 'X-Denied')
    _, allowed_headers, = preflight(cors, headers: 'X-Allowed')

    _(denied_headers).must_equal({})
    _(allowed_headers['access-control-allow-headers']).must_equal 'X-Allowed'
  end

  it 'omits request-header capabilities when the preflight does not request headers' do
    cors, = build_cors
    _, headers, = preflight(cors, headers: nil)

    _(headers).wont_include 'access-control-allow-headers'
    _(headers['access-control-allow-methods']).must_equal 'POST'
  end

  it 'does not allow a method or header rejected by any layer' do
    cors, = build_cors

    _, get_headers, = preflight(cors, method: 'GET')
    _(get_headers).must_equal({})

    _, header_headers, = preflight(cors, headers: 'X-Platform')
    _(header_headers).must_equal({})
  end

  it 'does not grant credentials when one layer uses a wildcard origin' do
    app = ->(_env) { [200, {}, ['ok']] }
    cors = Rack::Cors.new(app, mode: :intersection) do
      allow do
        origins '*'
        resource '/',
                 methods: :post,
                 headers: :any,
                 expose: ['x-shared'],
                 max_age: 600
      end
      allow do
        origins ORIGIN
        resource '/',
                 methods: :post,
                 headers: :any,
                 expose: ['x-shared'],
                 max_age: 300,
                 credentials: true
      end
    end

    status, headers, body = preflight(cors, headers: nil)

    _(status).must_equal 200
    _(body).must_equal []
    _(headers['access-control-allow-origin']).must_equal ORIGIN
    _(headers['access-control-expose-headers']).must_equal 'x-shared'
    _(headers['access-control-max-age']).must_equal '300'
    _(headers).wont_include 'access-control-allow-credentials'
  end

  it 'keeps a wildcard origin when every layer is public' do
    app = ->(_env) { [200, {}, ['ok']] }
    cors = Rack::Cors.new(app, mode: :intersection) do
      allow do
        origins '*'
        resource '/', methods: :post, headers: :any
      end
      allow do
        origins '*'
        resource '/', methods: %i[get post], headers: :any
      end
    end

    _, headers, = preflight(cors, headers: nil)

    _(headers['access-control-allow-origin']).must_equal '*'
    _(headers).wont_include 'access-control-allow-credentials'
  end

  it 'rejects a request when any origin layer does not match' do
    cors, = build_cors
    _, headers, = preflight(cors, origin: 'http://other.example')

    _(headers).must_equal({})
  end

  it 'evaluates origin procs and resource conditions once per request' do
    cors, counts = build_cors

    preflight(cors)

    _(counts[:origin]).must_equal 1
    _(counts[:condition]).must_equal 1
  end

  it 'supports mode as a string' do
    app = ->(_env) { [200, {}, ['ok']] }
    cors = Rack::Cors.new(app, mode: 'intersection') do
      allow do
        origins '*'
        resource '/', methods: :post, headers: :any
      end
      allow do
        origins ORIGIN
        resource '/', methods: %i[get post], headers: :any
      end
    end

    status, headers, = preflight(cors)

    _(status).must_equal 200
    _(headers['access-control-allow-methods']).must_equal 'POST'
  end

  it 'supports an explicit intersection block' do
    origin_calls = 0
    origin_proc = proc do |source, env|
      origin_calls += 1
      source == env['HTTP_ORIGIN']
    end

    cors = Rack::Cors.new(proc {}) do
      intersection do
        allow do
          origins origin_proc
          resource '/', methods: :post, headers: :any, credentials: true
        end
        allow do
          origins origin_proc
          resource '/', methods: %i[get post], headers: :any, credentials: true
        end
      end
    end

    status, headers, = preflight(cors)

    _(status).must_equal 200
    _(headers['access-control-allow-methods']).must_equal 'POST'
    _(headers['access-control-allow-credentials']).must_equal 'true'
    _(origin_calls).must_equal 1
  end
end

