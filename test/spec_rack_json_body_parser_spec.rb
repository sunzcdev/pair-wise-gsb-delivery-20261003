# frozen_string_literal: true

require 'minitest/autorun'
require 'rack/mock'
require 'rack/lint'
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

  # A minimal, non-rewindable rack.input: it only supports read/gets/each/close
  # and serves its data through arbitrary short reads.
  class ForwardOnlyInput
    def initialize(data, chunk_size = data.bytesize)
      @data = data.dup.force_encoding(Encoding::ASCII_8BIT)
      @chunk_size = chunk_size
      @offset = 0
      @closed = false
    end

    attr_reader :bytes_read, :closed

    def external_encoding
      Encoding::ASCII_8BIT
    end

    def binmode?
      true
    end

    def read(length = nil, _buffer = nil)
      raise IOError, 'input is closed' if @closed

      if length.nil?
        length = @data.bytesize - @offset
      end

      return nil if length.positive? && @offset >= @data.bytesize

      chunk = @data.byteslice(@offset, [length, @chunk_size].min)
      @offset += chunk.bytesize
      @bytes_read = @offset
      chunk
    end

    def gets
      read(1)
    end

    def each
      while (line = gets)
        yield line
      end
    end

    def close
      @closed = true
    end

    def respond_to?(name, include_private = false)
      return false if name.to_sym == :rewind
      super
    end
  end

  # A non-rewindable input that raises while being read.
  class ExplodingInput
    def external_encoding
      Encoding::ASCII_8BIT
    end

    def binmode?
      true
    end

    attr_reader :closed

    def read(*)
      raise IOError, 'boom'
    end

    def gets
      read
    end

    def each
      yield read
    end

    def close
      @closed = true
    end

    def respond_to?(name, include_private = false)
      return false if name.to_sym == :rewind
      super
    end
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

  describe "max_body_bytes" do
    specify "accepts a body exactly at the limit" do
      body = '{"a":1}'
      parser = create_parser(app, max_body_bytes: body.bytesize)
      res = parser.call(mock_env(input: body))
      _(res[0]).must_equal 200
      _(res[2].to_enum.to_a.join).must_equal '{"a"=>1}'
    end

    specify "rejects a body one byte over the limit with 413" do
      body = '{"a":1}'
      parser = create_parser(app, max_body_bytes: body.bytesize - 1)
      status, headers, response = parser.call(mock_env(input: body))
      _(status).must_equal 413
      _(headers['Content-Type']).must_equal 'application/json'
      response.each { |part| _(JSON.parse(part)['error']).wont_be_nil }
    end

    specify "still returns 400 for malformed JSON within the limit" do
      body = 'not json'
      parser = create_parser(app, max_body_bytes: 100)
      status, = parser.call(mock_env(input: body))
      _(status).must_equal 400
    end

    specify "returns 413 when malformed JSON exceeds the limit" do
      body = 'this is definitely not json'
      parser = create_parser(app, max_body_bytes: 5)
      status, = parser.call(mock_env(input: body))
      _(status).must_equal 413
    end

    specify "ignores a Content-Length that understates the body" do
      input = ForwardOnlyInput.new('{"a":1}', 3)
      env = mock_env
      env['rack.input'] = input
      env['CONTENT_LENGTH'] = '1'
      parser = create_parser(app, max_body_bytes: 4)
      status, = parser.call(env)
      _(status).must_equal 413
      _(input.bytes_read).must_equal 5
    end

    specify "ignores a Content-Length that overstates the body" do
      input = ForwardOnlyInput.new('{"a":1}', 3)
      env = mock_env
      env['rack.input'] = input
      env['CONTENT_LENGTH'] = '9999'
      parser = create_parser(app, max_body_bytes: 100)
      status, = parser.call(env)
      _(status).must_equal 200
    end

    specify "uses actual content when Content-Length is missing" do
      input = ForwardOnlyInput.new('{"a":1}', 2)
      env = mock_env
      env['rack.input'] = input
      env.delete('CONTENT_LENGTH')
      parser = create_parser(app, max_body_bytes: 4)
      status, = parser.call(env)
      _(status).must_equal 413
    end

    specify "handles short, fragmented reads and reads at most one byte over the limit" do
      body = '{"a":12345}'
      input = ForwardOnlyInput.new(body, 2)
      env = mock_env
      env['rack.input'] = input
      env.delete('CONTENT_LENGTH')
      parser = create_parser(app, max_body_bytes: 6)
      status, = parser.call(env)
      _(status).must_equal 413
      _(input.bytes_read).must_equal 7
    end

    specify "reads only one byte past the limit even for a very large body" do
      body = 'x' * 1_000_000
      input = ForwardOnlyInput.new(body, 4096)
      env = mock_env(input: body)
      env['rack.input'] = input
      parser = create_parser(app, max_body_bytes: 1024)
      status, = parser.call(env)
      _(status).must_equal 413
      _(input.bytes_read).must_equal 1025
    end

    specify "does not invoke the app or set form caches when the body is too large" do
      called = false
      tracking_app = ->(_env) { called = true; [200, {}, []] }
      env = mock_env(input: '{"a":12345}')
      parser = Rack::JSONBodyParser.new(tracking_app, max_body_bytes: 4)
      status, = parser.call(env)
      _(status).must_equal 413
      _(called).must_equal false
      _(env['rack.request.form_hash']).must_be_nil
      _(env['rack.request.form_input']).must_be_nil
    end

    specify "does not leave a half-updated form cache when parsing fails" do
      env = mock_env(input: 'not json')
      parser = Rack::JSONBodyParser.new(app, max_body_bytes: 100)
      status, = parser.call(env)
      _(status).must_equal 400
      _(env['rack.request.form_hash']).must_be_nil
      _(env['rack.request.form_input']).must_be_nil
    end
  end

  describe "non-rewindable input" do
    specify "parses params from a read-only, short-read input" do
      input = ForwardOnlyInput.new('{"key":"value"}', 4)
      env = mock_env
      env['rack.input'] = input
      parser = create_parser(app)
      status, = parser.call(env)
      _(status).must_equal 200
    end

    specify "lets the downstream app re-read the raw body after parsing" do
      input = ForwardOnlyInput.new('{"key":"value"}', 3)
      seen = nil
      replay_app = lambda do |env|
        request = Rack::Request.new(env)
        seen = [request.params, request.body.read]
        [200, {}, []]
      end
      env = mock_env
      env['rack.input'] = input
      Rack::JSONBodyParser.new(replay_app).call(env)
      _(seen[0]).must_equal('key' => 'value')
      _(seen[1]).must_equal '{"key":"value"}'
    end

    specify "provides a rewindable replay buffer for repeated downstream reads" do
      input = ForwardOnlyInput.new('{"key":"value"}', 5)
      replayed = []
      replay_app = lambda do |env|
        2.times do
          env['rack.input'].rewind
          replayed << env['rack.input'].read
        end
        [200, {}, []]
      end
      env = mock_env
      env['rack.input'] = input
      Rack::JSONBodyParser.new(replay_app).call(env)
      _(replayed).must_equal ['{"key":"value"}', '{"key":"value"}']
    end

    specify "never closes the original server-owned input" do
      input = ForwardOnlyInput.new('{"key":"value"}', 4)
      env = mock_env
      env['rack.input'] = input
      create_parser(app).call(env)
      _(input.closed).must_equal false
    end

    specify "returns 400 and skips the app when reading raises" do
      input = ExplodingInput.new
      called = false
      tracking_app = ->(_env) { called = true; [200, {}, []] }
      env = mock_env
      env['rack.input'] = input
      status, = Rack::JSONBodyParser.new(tracking_app, max_body_bytes: 100).call(env)
      _(status).must_equal 400
      _(called).must_equal false
      _(env['rack.request.form_hash']).must_be_nil
      _(env['rack.request.form_input']).must_be_nil
    end

    specify "keeps replay buffers isolated across two consecutive requests" do
      middleware = Rack::JSONBodyParser.new(app)

      first = mock_env
      first['rack.input'] = ForwardOnlyInput.new('{"key":"one"}', 3)
      second = mock_env
      second['rack.input'] = ForwardOnlyInput.new('{"key":"two"}', 3)

      middleware.call(first)
      middleware.call(second)
      _(first['rack.input'].read).must_equal '{"key":"one"}'
      _(second['rack.input'].read).must_equal '{"key":"two"}'
    end
  end

  describe "requests outside the configured scope" do
    specify "does not read rack.input for a non-matching media type" do
      input = ForwardOnlyInput.new('{"key":"value"}', 4)
      env = mock_env(type: 'text/plain')
      env['rack.input'] = input
      create_parser(app).call(env)
      _(input.bytes_read).must_be_nil
    end

    specify "honors the verbs and media configuration together" do
      parser = create_parser(app, verbs: ['PATCH'], media: ['application/json'])
      patch_env = mock_env
      patch_env['REQUEST_METHOD'] = 'PATCH'
      res = parser.call(patch_env)
      _(res[2].to_enum.to_a.join).must_equal '{"key"=>"value"}'

      post_env = mock_env
      post_env['REQUEST_METHOD'] = 'POST'
      res = parser.call(post_env)
      _(res[2].to_enum.to_a.join).must_equal '{}'
    end
  end
end
