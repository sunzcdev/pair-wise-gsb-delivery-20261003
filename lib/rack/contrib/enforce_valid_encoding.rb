# frozen_string_literal: true

module Rack
  # Ensure that the path and query string presented to the application
  # contains only valid characters.  If the validation fails, then a
  # 400 Bad Request response is returned immediately.
  #
  # The path and the query string are validated as separate components,
  # so a percent sign at the end of one component can never be completed
  # by bytes from the other.
  #
  # Pass <tt>strict: true</tt> to additionally reject malformed percent
  # escapes (a lone percent sign, an incomplete triplet, or non-hex
  # escape digits) and decoded bytes that are not valid UTF-8.  Strict
  # validation decodes exactly one layer of percent encoding, treats a
  # plus sign in the path as a literal character and a plus sign in the
  # query string as a form-encoded space, and never modifies the
  # request environment.
  #
  # Requires: Ruby 1.9
  #
  class EnforceValidEncoding
    PERCENT_ESCAPE = /%[0-9A-Fa-f]{2}/
    INVALID_ESCAPE = /%(?![0-9A-Fa-f]{2})/

    def initialize(app, strict: false)
      @app = app
      @strict = strict
    end

    def call env
      path = env.fetch('PATH_INFO', '')
      query = env.fetch('QUERY_STRING', '')
      if valid_component?(path, false) && valid_component?(query, true)
        @app.call env
      else
        [400, {'content-type'=>'text/plain'}, ['Bad Request']]
      end
    end

    private

    def valid_component?(component, form_encoded)
      if @strict
        strict_valid?(component, form_encoded)
      else
        legacy_valid?(component)
      end
    end

    def legacy_valid?(component)
      return false unless component.dup.force_encoding(Encoding::US_ASCII).valid_encoding?
      Rack::Utils.unescape(component).valid_encoding?
    rescue ArgumentError
      false
    end

    def strict_valid?(component, form_encoded)
      bytes = component.b
      bytes.tr!('+', ' ') if form_encoded
      return false if bytes.match?(INVALID_ESCAPE)
      decoded = bytes.gsub(PERCENT_ESCAPE) { |escape| escape[1, 2].hex.chr }
      decoded.force_encoding(Encoding::UTF_8).valid_encoding?
    end
  end
end
