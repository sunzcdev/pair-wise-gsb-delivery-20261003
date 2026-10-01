# frozen_string_literal: true

require 'json'
require 'stringio'

module Rack
  # A Rack middleware that makes JSON-encoded request bodies available in the
  # request.params hash. By default it parses POST, PATCH, and PUT requests
  # whose media type is <tt>application/json</tt>. You can configure it to match
  # any verb or media type via the <tt>:verbs</tt> and <tt>:media</tt> options.
  #
  #
  # == Examples:
  #
  # === Parse POST and GET requests only
  #   use Rack::JSONBodyParser, verbs: ['POST', 'GET']
  #
  # === Parse POST|PATCH|PUT requests whose content-type matches 'json'
  #   use Rack::JSONBodyParser, media: /json/
  #
  # === Parse POST requests whose content-type is 'application/json' or 'application/vnd+json'
  #   use Rack::JSONBodyParser, verbs: ['POST'], media: ['application/json', 'application/vnd.api+json']
  #
  # === Reject JSON bodies larger than 64 KiB with a 413 response
  #   use Rack::JSONBodyParser, max_body_bytes: 65536
  #
  class JSONBodyParser
    CONTENT_TYPE_MATCHERS = {
      String => lambda { |option, header|
        Rack::MediaType.type(header) == option
      },
      Array => lambda { |options, header|
        media_type = Rack::MediaType.type(header)
        options.any? { |opt| media_type == opt }
      },
      Regexp => lambda {
        if //.respond_to?(:match?)
          # Use Ruby's fast regex matcher when available
          ->(option, header) { option.match? header }
        else
          # Fall back to the slower matcher for rubies older than 2.4
          ->(option, header) { option.match header }
        end
      }.call(),
    }.freeze

    DEFAULT_PARSER = ->(body) { JSON.parse(body, create_additions: false) }

    # Number of bytes read in one go while enforcing the body size limit.
    READ_CHUNK_SIZE = 65_536

    def initialize(
      app,
      verbs: %w[POST PATCH PUT],
      media: 'application/json',
      max_body_bytes: nil,
      &block
    )
      @app = app
      @verbs, @media, @max_body_bytes = verbs, media, max_body_bytes
      @matcher = CONTENT_TYPE_MATCHERS.fetch(@media.class)
      @parser = block || DEFAULT_PARSER
    end

    def call(env)
      begin
        if @verbs.include?(env[Rack::REQUEST_METHOD]) &&
           @matcher.call(@media, env['CONTENT_TYPE'])

          update_form_hash_with_json_body(env)
        end
      rescue PayloadTooLarge
        body = { error: 'Request body exceeds the allowed size' }.to_json
        header = { 'content-type' => 'application/json' }
        return Rack::Response.new(body, 413, header).finish
      rescue ParserError
        body = { error: 'Failed to parse body as JSON' }.to_json
        header = { 'content-type' => 'application/json' }
        return Rack::Response.new(body, 400, header).finish
      end
      @app.call(env)
    end

    private

    class ParserError < StandardError; end
    class PayloadTooLarge < StandardError; end

    def update_form_hash_with_json_body(env)
      input = env[Rack::RACK_INPUT]
      body_content = read_body(input)
      return if body_content.empty?

      begin
        parsed_body = @parser.call(body_content)
      rescue StandardError
        raise ParserError
      end

      # rack.input is not guaranteed to be rewindable, so back it with a fresh
      # in-memory copy so the app can both read the parsed params and re-read
      # the raw body. The original input is owned by the server and is never
      # closed by this middleware.
      replay_input = StringIO.new(body_content.b)
      replay_input.binmode
      replay_input.rewind

      env.update(
        RACK_INPUT => replay_input,
        CONTENT_LENGTH => body_content.bytesize.to_s,
        RACK_REQUEST_FORM_INPUT => replay_input,
        RACK_REQUEST_FORM_HASH => parsed_body
      )
    end

    # Reads the request body while enforcing the configured byte limit.
    #
    # Content-Length is deliberately ignored: it may be missing, understated,
    # or disagree with the bytes actually delivered by chunked or short
    # reads, so reads are looped until EOF. When a limit is configured at most
    # +max_body_bytes + 1+ bytes are consumed from the input (one extra byte is
    # enough to detect an overflow), so oversized bodies are never buffered.
    def read_body(input)
      content = +''
      limit = @max_body_bytes ? @max_body_bytes + 1 : nil

      while limit.nil? || content.bytesize < limit
        chunk_size = READ_CHUNK_SIZE
        chunk_size = [chunk_size, limit - content.bytesize].min if limit

        begin
          chunk = input.read(chunk_size)
        rescue StandardError
          raise ParserError
        end
        break if chunk.nil? || chunk.empty?
        content << chunk
      end

      raise PayloadTooLarge if limit && content.bytesize > @max_body_bytes
      content
    end
  end
end
