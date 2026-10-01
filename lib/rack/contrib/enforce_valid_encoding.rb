# frozen_string_literal: true

module Rack
  # Ensure that the path and query string presented to the application
  # contains only valid characters.  If the validation fails, then a
  # 400 Bad Request response is returned immediately.
  #
  # The path and the query string are validated as separate components,
  # so bytes from one can never complete a percent-encoded sequence in
  # the other.
  #
  # Pass <tt>strict: true</tt> to additionally reject malformed
  # percent-encoding: a bare %, an incomplete triplet, or an escape
  # with non-hexadecimal digits, as well as invalid UTF-8 produced by
  # decoding.  Validation decodes a single layer only, so %252F is seen
  # as the literal text %2F and never as a slash.  A + in the path is
  # an ordinary character, while a + in the query string is decoded as
  # a space per the form convention; the env is never rewritten for
  # validation.
  #
  # Requires: Ruby 1.9
  #
  class EnforceValidEncoding
    PERCENT_TRIPLET = /%[0-9A-Fa-f]{2}/.freeze
    MALFORMED_PERCENT = /%(?![0-9A-Fa-f]{2})/.freeze

    def initialize(app, strict: false)
      @app = app
      @strict = strict
    end

    def call env
      if valid_component?(env.fetch('PATH_INFO', ''), false) &&
         valid_component?(env.fetch('QUERY_STRING', ''), true)
        @app.call env
      else
        [400, {'content-type'=>'text/plain'}, ['Bad Request']]
      end
    end

    private

    def valid_component? component, plus_as_space
      return false unless component.dup.force_encoding(Encoding::US_ASCII).valid_encoding?
      return false if @strict && component =~ MALFORMED_PERCENT

      decode_once(component, plus_as_space).valid_encoding?
    end

    def decode_once component, plus_as_space
      decoded = component.dup.force_encoding(Encoding::BINARY)
      decoded = decoded.tr('+', ' ') if plus_as_space
      decoded.gsub(PERCENT_TRIPLET) { |triplet| triplet[1, 2].to_i(16).chr(Encoding::BINARY) }.
        force_encoding(Encoding::UTF_8)
    end
  end
end
