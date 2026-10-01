# frozen_string_literal: true

require 'json'
require 'stringio'

module Rack
  # A Rack middleware that makes JSON-encoded request bodies available in the
  # request.params hash. By default it parses POST, PATCH, and PUT requests
  # whose media type is <tt>application/json</tt>. You can configure it to match
  # any verb or media type via the <tt>:verbs</tt> and <tt>:media</tt> options.
  # Pass <tt>:max_body_bytes</tt> to reject bodies larger than the given
  # number of bytes with a 413 response; the limit is enforced against the
  # actual bytes read from rack.input (never Content-Length), reading at
  # most one byte past the limit.
  #
  # The body is buffered into a fresh StringIO per request, so rack.input
  # does not need to be rewindable and downstream middleware can always
  # re-read the raw body. Ownership stays clear: the original rack.input
  # belongs to the server (this middleware never closes it), while the
  # replay buffer placed in the env belongs to the downstream app.
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
  # === Reject JSON bodies larger than 1 megabyte with a 413 response
  #   use Rack::JSONBodyParser, max_body_bytes: 1_048_576
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

    def initialize(
      app,
      verbs: %w[POST PATCH PUT],
      media: 'application/json',
      max_body_bytes: nil,
      &block
    )
      @app = app
      @verbs, @media = verbs, media
      @matcher = CONTENT_TYPE_MATCHERS.fetch(@media.class)
      @parser = block || DEFAULT_PARSER
      @max_body_bytes = max_body_bytes
    end

    def call(env)
      begin
        if @verbs.include?(env[Rack::REQUEST_METHOD]) &&
           @matcher.call(@media, env['CONTENT_TYPE'])

          update_form_hash_with_json_body(env)
        end
      rescue BodyTooLargeError
        body = { error: 'Request body too large' }.to_json
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
    class BodyTooLargeError < StandardError; end

    def update_form_hash_with_json_body(env)
      body_content = read_body(env[Rack::RACK_INPUT])

      unless body_content.empty?
        begin
          parsed_body = @parser.call(body_content)
        rescue StandardError
          raise ParserError
        end
      end

      # Hand downstream a fresh, rewindable copy of the body. The env is
      # only mutated once the body has been read and parsed successfully,
      # so a failure never leaves a half-updated form cache behind.
      replay = StringIO.new(body_content)
      env[Rack::RACK_INPUT] = replay
      return if body_content.empty?

      env.update(
        Rack::RACK_REQUEST_FORM_HASH => parsed_body,
        Rack::RACK_REQUEST_FORM_INPUT => replay
      )
    end

    # Reads the entire body, enforcing @max_body_bytes against the actual
    # bytes returned by rack.input regardless of any Content-Length
    # header. At most one byte past the limit is read before giving up.
    def read_body(input)
      return input.read.to_s unless @max_body_bytes

      content = +''
      remaining = @max_body_bytes + 1
      while remaining.positive?
        chunk = input.read(remaining)
        break if chunk.nil? || chunk.empty?

        content << chunk
        remaining -= chunk.bytesize
      end
      raise BodyTooLargeError if content.bytesize > @max_body_bytes

      content
    end
  end
end
