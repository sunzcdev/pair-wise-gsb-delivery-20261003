# frozen_string_literal: true

require 'minitest/autorun'
require 'rack/lint'
require 'rack/mock'
require 'rack/request'
require 'rack/contrib/json_body_parser'

describe Rack::JSONBodyParser do

  def app
   ->(env) { [200, {}, [Rack::Request.new(env).params.to_s]] }
  end

  def mock_env(input: '{"key": "value"}', type: 'application/json')
    headers = if type
                { input: input, 'REQUEST_METHOD' => 'POST', 'CONTENT_TYPE' => type }
              else
                { input: input, 'REQUEST_METHOD' => 'POST' }
              end

    Rack::MockRequest.env_for '/', headers
  end

  def create_parser(app, **args, &block)
    Rack::Lint.new(Rack::JSONBodyParser.new(app, **args, &block))
  end

  # A rack.input that only implements +read+ (no +rewind+), records the
  # requested read sizes, optionally serves short reads, and can raise
  # from +read+ to simulate a broken stream.
  class TrackingInput
    attr_reader :read_sizes, :bytes_served

    def initialize(data, chunk_size: nil, read_error: nil)
      @data = data
      @chunk_size = chunk_size
      @read_error = read_error
      @pos = 0
      @read_sizes = []
      @bytes_served = 0
    end

    def read(length = nil, buffer = nil)
      @read_sizes << length
      raise @read_error if @read_error
      return nil if @pos >= @data.bytesize

      length = @data.bytesize - @pos unless length
      length = [length, @chunk_size].compact.min
      chunk = @data.byteslice(@pos, length)
      @pos += chunk.bytesize
      @bytes_served += chunk.bytesize
      buffer ? buffer.replace(chunk) : chunk
    end
  end

  def plain_parser(app, **args, &block)
    Rack::JSONBodyParser.new(app, **args, &block)
  end

  def tracking_env(input, type: 'application/json', method: 'POST')
    env = Rack::MockRequest.env_for '/', 'REQUEST_METHOD' => method, 'CONTENT_TYPE' => type, input: ''
    env['rack.input'] = input
    env
  end

  def silent_app
    ->(env) { [200, {}, ['OK']] }
  end

  specify "should parse 'application/json' requests" do
    res = create_parser(app).call(mock_env)
    _(res[2].to_enum.to_a.join).must_equal '{"key"=>"value"}'
  end

  specify "should parse 'application/json; charset=utf-8' requests" do
    env = mock_env(type: 'application/json; charset=utf-8')
    res = create_parser(app).call(env)
    _(res[2].to_enum.to_a.join).must_equal '{"key"=>"value"}'
  end

  specify "should parse 'application/json' requests with an empty body" do
    res = create_parser(app).call(mock_env(input: ''))
    _(res[2].to_enum.to_a.join).must_equal '{}'
  end

  specify "shouldn't affect form-urlencoded requests" do
    env = mock_env(input: 'key=value', type: 'application/x-www-form-urlencoded')
    res = create_parser(app).call(env)
    _(res[2].to_enum.to_a.join).must_equal '{"key"=>"value"}'
  end

  specify "should not parse non-json media types" do
    env = mock_env(type: 'text/plain')
    res = create_parser(app).call(env)
    _(res[2].to_enum.to_a.join).must_equal '{}'
  end

  specify "shouldn't parse or error when CONTENT_TYPE is nil" do
    env = mock_env(type: nil)
    res = create_parser(app).call(env)
    _(res[2].to_enum.to_a.join).must_equal %Q({"{\\"key\\": \\"value\\"}"=>nil})
  end

  specify "should not rescue JSON:ParserError raised by the app" do
    env = mock_env
    app = ->(env) { raise JSON::ParserError }
    _( -> { create_parser(app).call(env) }).must_raise JSON::ParserError
  end

  specify "should rescue StandardError subclasses raised by the parser" do
    class CustomParserError < StandardError; end

    parser = create_parser(app) do |_body|
      raise CustomParserError.new
    end

    res = parser.call(mock_env)
    _(res[0]).must_equal 400
  end

  describe "contradiction between body and type" do
    specify "should return bad request with a JSON-encoded error message" do
      env = mock_env(input: 'This is not JSON')
      status, headers, body = create_parser(app).call(env)
      _(status).must_equal 400
      _(headers['Content-Type']).must_equal 'application/json'
      body.each { |part| _(JSON.parse(part)['error']).wont_be_nil }
    end
  end

  describe "with configuration" do
    specify "should use a given block to parse the JSON body" do
      parser = create_parser(app) do |body|
        { 'payload' => JSON.parse(body) }
      end
      res = parser.call(mock_env)
      _(res[2].to_enum.to_a.join).must_equal '{"payload"=>{"key"=>"value"}}'
    end

    specify "should accept an array of HTTP verbs to parse" do
      env = mock_env.merge('REQUEST_METHOD' => 'GET')
      parser = create_parser(app, verbs: %w[GET])

      res = parser.call(env)
      _(res[2].to_enum.to_a.join).must_equal '{"key"=>"value"}'

      res = parser.call(mock_env)
      _(res[2].to_enum.to_a.join).must_equal '{}'
    end

    specify "should accept an Array of media-types to parse" do
      parser = create_parser(app, media: ['application/json', 'text/plain'])
      env = mock_env(type: 'text/plain')
      res = parser.call(env)
      _(res[2].to_enum.to_a.join).must_equal '{"key"=>"value"}'

      html_env = mock_env(type: 'text/html')
      res = parser.call(html_env)
      _(res[2].to_enum.to_a.join).must_equal '{}'
    end

    specify "should accept a Regexp as a media-type matcher" do
      parser = create_parser(app, media: /json/)
      env = mock_env(type: 'weird/json.odd')
      res = parser.call(env)
      _(res[2].to_enum.to_a.join).must_equal '{"key"=>"value"}'
    end

    specify "should accept a String as a media-type matcher" do
      parser = create_parser(app, media: 'application/vnd.api+json')
      env = mock_env(type: 'application/vnd.api+json')
      res = parser.call(env)
      _(res[2].to_enum.to_a.join).must_equal '{"key"=>"value"}'
    end
  end

  describe "with max_body_bytes" do
    specify "accepts a body exactly at the limit" do
      body = '{"key":"value"}'
      input = TrackingInput.new(body)
      status, = plain_parser(silent_app, max_body_bytes: body.bytesize).call(tracking_env(input))
      _(status).must_equal 200
    end

    specify "rejects an oversized body with 413 without calling the app or touching the form cache" do
      called = false
      app = ->(_env) { called = true; [200, {}, ['OK']] }
      input = TrackingInput.new('{"key":"value"}')
      env = tracking_env(input)

      status, headers, res_body = plain_parser(app, max_body_bytes: 5).call(env)

      _(status).must_equal 413
      _(headers['Content-Type']).must_equal 'application/json'
      _(JSON.parse(res_body.each.to_a.join)['error']).wont_be_nil
      _(called).must_equal false
      _(env[Rack::RACK_REQUEST_FORM_HASH]).must_be_nil
      _(env[Rack::RACK_REQUEST_FORM_INPUT]).must_be_nil
      _(env['rack.input']).must_be_same_as input
    end

    specify "reads at most one byte past the limit" do
      input = TrackingInput.new('x' * 100)
      plain_parser(silent_app, max_body_bytes: 10).call(tracking_env(input))
      _(input.bytes_served).must_equal 11
    end

    specify "ignores a lying Content-Length in both directions" do
      body = '{"key":"value"}'

      env = tracking_env(TrackingInput.new(body))
      env['CONTENT_LENGTH'] = '2'
      status, = plain_parser(silent_app, max_body_bytes: 5).call(env)
      _(status).must_equal 413

      env = tracking_env(TrackingInput.new(body))
      env['CONTENT_LENGTH'] = '1000'
      status, = plain_parser(silent_app, max_body_bytes: 1024).call(env)
      _(status).must_equal 200
    end

    specify "enforces the limit when Content-Length is missing" do
      env = tracking_env(TrackingInput.new('{"key":"value"}'))
      env.delete('CONTENT_LENGTH')
      status, = plain_parser(silent_app, max_body_bytes: 4).call(env)
      _(status).must_equal 413
    end

    specify "assembles bodies delivered in short reads" do
      captured = nil
      app = ->(env) { captured = Rack::Request.new(env).params; [200, {}, ['OK']] }
      input = TrackingInput.new('{"key":"value"}', chunk_size: 3)

      status, = plain_parser(app, max_body_bytes: 1024).call(tracking_env(input))

      _(status).must_equal 200
      _(captured).must_equal({ 'key' => 'value' })
      _(input.read_sizes.length).must_be :>, 1
    end

    specify "lets downstream re-read the raw body of a non-rewindable input" do
      replayed = nil
      app = lambda do |env|
        replayed = env['rack.input'].read
        [200, {}, [Rack::Request.new(env).params.to_s]]
      end

      status, _, res_body = plain_parser(app, max_body_bytes: 1024)
                            .call(tracking_env(TrackingInput.new('{"key":"value"}')))

      _(status).must_equal 200
      _(replayed).must_equal '{"key":"value"}'
      _(res_body.each.to_a.join).must_equal '{"key"=>"value"}'
    end

    specify "handles non-rewindable input without a limit" do
      replayed = nil
      app = ->(env) { replayed = env['rack.input'].read; [200, {}, ['OK']] }

      status, = plain_parser(app).call(tracking_env(TrackingInput.new('{"key":"value"}')))

      _(status).must_equal 200
      _(replayed).must_equal '{"key":"value"}'
    end

    specify "still returns 400 for malformed JSON within the limit" do
      status, = plain_parser(silent_app, max_body_bytes: 1024)
                .call(tracking_env(TrackingInput.new('This is not JSON')))
      _(status).must_equal 400
    end

    specify "read failures do not call the app or touch the form cache" do
      called = false
      app = ->(_env) { called = true; [200, {}, ['OK']] }
      input = TrackingInput.new('{"key":"value"}', read_error: IOError.new('boom'))
      env = tracking_env(input)

      _(-> { plain_parser(app, max_body_bytes: 1024).call(env) }).must_raise IOError
      _(called).must_equal false
      _(env[Rack::RACK_REQUEST_FORM_HASH]).must_be_nil
      _(env[Rack::RACK_REQUEST_FORM_INPUT]).must_be_nil
    end

    specify "does not read bodies of requests it does not handle" do
      input = TrackingInput.new('{"key":"value"}')
      status, = plain_parser(silent_app, max_body_bytes: 1)
                .call(tracking_env(input, method: 'GET'))
      _(status).must_equal 200
      _(input.read_sizes).must_be_empty
    end

    specify "does not share bodies between consecutive requests" do
      parser = plain_parser(silent_app, max_body_bytes: 1024)
      env1 = tracking_env(TrackingInput.new('{"a":1}'))
      env2 = tracking_env(TrackingInput.new('{"b":2}'))

      parser.call(env1)
      parser.call(env2)

      _(env1['rack.input'].read).must_equal '{"a":1}'
      _(env2['rack.input'].read).must_equal '{"b":2}'
      _(env1['rack.input']).wont_be_same_as env2['rack.input']
    end
  end
end
