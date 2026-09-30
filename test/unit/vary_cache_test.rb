# frozen_string_literal: true

require 'minitest/autorun'
require 'rack'
require 'rack/cors'

# A minimal HTTP cache keyed on (path, Vary request header values).
#
# It models RFC 7234 selection behaviour: a stored response is reused only if
# the new request carries the same values for every request header listed in
# the stored response's Vary field. If a response carries no usable Vary
# declaration, a single stored entry is served for every request to the same
# path (which is exactly how an under-declared Vary header leaks one kind of
# preflight answer to a different kind of request).
class MockVaryCache
  def initialize(app)
    @app = app
    @store = Hash.new { |hash, path| hash[path] = [] }
  end

  def call(env)
    path = env['PATH_INFO']

    cached = @store[path].find { |entry| match?(entry, env) }
    return cached[:response] if cached

    status, headers, = response = @app.call(env)
    vary = parse_vary(headers)
    @store[path] << { vary: vary, values: vary_values(vary, env), response: response }
    [status, headers.dup, []]
  end

  private

  def parse_vary(headers)
    vary_key = headers.keys.detect { |key| key.to_s.downcase == 'vary' }
    return nil unless vary_key

    fields = Array(headers[vary_key]).flat_map { |value| value.to_s.split(/,\s*/) }
    return :wildcard if fields.include?('*')

    fields
  end

  def vary_values(vary, env)
    return {} if vary.nil? || vary == :wildcard

    vary.each_with_object({}) do |field, values|
      key = "HTTP_#{field.upcase.tr('-', '_')}"
      values[key] = env[key]
    end
  end

  def match?(entry, env)
    vary = entry[:vary]
    return true if vary.nil?
    return false if vary == :wildcard

    entry[:values].all? { |key, value| env[key] == value }
  end
end

describe 'Vary-aware simulated cache' do
  def build_app
    Rack::Builder.new do
      use Rack::Cors do
        allow do
          origins 'http://allowed.example'
          resource '/preflight', methods: :get, headers: 'x-custom'
        end
      end
      run ->(_env) { [200, { 'content-type' => 'text/plain' }, ['ok']] }
    end
  end

  def request(app, method: 'GET', headers: nil, origin: 'http://allowed.example')
    env = Rack::MockRequest.env_for(
      '/preflight',
      method: 'OPTIONS',
      'HTTP_ORIGIN' => origin,
      'HTTP_ACCESS_CONTROL_REQUEST_METHOD' => method,
      'HTTP_ACCESS_CONTROL_REQUEST_HEADERS' => headers
    )
    status, response_headers, = app.call(env)
    [status, response_headers.dup]
  end

  it 'does not serve an allowed GET preflight to a denied POST preflight' do
    app = MockVaryCache.new(build_app)

    get_allowed = request(app, method: 'GET')
    post_denied = request(app, method: 'POST')

    _(get_allowed[1]['access-control-allow-origin']).wont_be_nil
    _(post_denied[1]['access-control-allow-origin']).must_be_nil
    _(get_allowed[1]['vary']).must_include 'Access-Control-Request-Method'
  end

  it 'does not serve an allowed header preflight to a denied header preflight' do
    app = MockVaryCache.new(build_app)

    allowed = request(app, headers: 'X-Custom')
    denied = request(app, headers: 'X-Secret')

    _(allowed[1]['access-control-allow-origin']).wont_be_nil
    _(denied[1]['access-control-allow-origin']).must_be_nil
    _(allowed[1]['vary']).must_include 'Access-Control-Request-Headers'
  end

  it 'does not serve an allowed-origin preflight to a denied-origin preflight' do
    app = MockVaryCache.new(build_app)

    allowed = request(app, origin: 'http://allowed.example')
    denied = request(app, origin: 'http://evil.example')

    _(allowed[1]['access-control-allow-origin']).wont_be_nil
    _(denied[1]['access-control-allow-origin']).must_be_nil
    _(allowed[1]['vary']).must_include 'Origin'
  end

  it 'reuses the stored response when the decision inputs are identical' do
    app = MockVaryCache.new(build_app)

    first = request(app, method: 'GET')
    second = request(app, method: 'GET')

    _(first[1]['access-control-allow-origin']).wont_be_nil
    _(second[1]['access-control-allow-origin']).wont_be_nil
    _(second[0]).must_equal 200
  end
end
